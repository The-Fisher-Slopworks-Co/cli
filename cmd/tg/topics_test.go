package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/go-faster/errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// topicMsgID is the top_message id topicPage gives topic id n.
func topicMsgID(n int) int { return 1000 + n }

// topicPage builds n consecutive topics starting at id `from`, shaped well
// enough to encode over the mock invoker.
func topicPage(from, n int) []tg.ForumTopicClass {
	out := make([]tg.ForumTopicClass, 0, n)
	for i := from; i < from+n; i++ {
		out = append(out, &tg.ForumTopic{
			ID:         i,
			Date:       i,
			TopMessage: topicMsgID(i),
			Peer:       &tg.PeerChannel{ChannelID: 1},
			FromID:     &tg.PeerUser{UserID: 7},
		})
	}
	return out
}

func TestNewTopicsResult(t *testing.T) {
	full := &tg.ForumTopic{
		ID:                  42,
		Title:               "Deploys",
		Date:                100,
		TopMessage:          1337,
		UnreadCount:         3,
		UnreadMentionsCount: 1,
		Closed:              true,
		Pinned:              true,
		My:                  true,
		IconColor:           0x6FB9F0,
		FromID:              &tg.PeerUser{UserID: 7},
	}
	full.SetIconEmojiID(5789012345678901234)

	res := newTopicsResult(&tg.MessagesForumTopics{
		Count: 9,
		Topics: []tg.ForumTopicClass{
			full,
			&tg.ForumTopicDeleted{ID: 43},
		},
		Users: []tg.UserClass{&tg.User{ID: 7, Username: "alice"}},
	})

	if res.Count != 9 {
		t.Errorf("count = %d, want 9", res.Count)
	}
	if len(res.Topics) != 2 {
		t.Fatalf("got %d topics, want 2", len(res.Topics))
	}

	got := res.Topics[0]
	if got.ID != 42 || got.Title != "Deploys" || got.TopMessage != 1337 {
		t.Errorf("unexpected topic: %+v", got)
	}
	if got.Unread != 3 || got.UnreadMentions != 1 {
		t.Errorf("unread = %d/%d, want 3/1", got.Unread, got.UnreadMentions)
	}
	if !got.Closed || !got.Pinned || !got.My {
		t.Errorf("flags lost: %+v", got)
	}
	// Custom emoji ids exceed float64's exact integer range, so they travel as
	// strings rather than JSON numbers.
	if got.IconEmojiID != "5789012345678901234" {
		t.Errorf("icon_emoji_id = %q", got.IconEmojiID)
	}
	if got.From == nil || got.From.Username != "alice" {
		t.Errorf("from = %+v", got.From)
	}

	// A deleted topic carries nothing but its id, and must not be dropped.
	if del := res.Topics[1]; del.ID != 43 || !del.Deleted {
		t.Errorf("deleted topic = %+v", del)
	}
}

func TestTopicsResultMarshalText(t *testing.T) {
	res := topicsResult{Topics: []topicItem{
		{ID: 1, Title: "General"},
		{ID: 42, Title: "Deploys", Unread: 3, Pinned: true, Closed: true},
	}}

	var buf bytes.Buffer
	if err := res.MarshalText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"#1", "General", "#42", "Deploys", "unread=3", "pinned,closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

func TestNextTopicOffsets(t *testing.T) {
	t.Run("uses the top message's date", func(t *testing.T) {
		date, id, topic, ok := nextTopicOffsets(&tg.MessagesForumTopics{
			Topics: []tg.ForumTopicClass{
				&tg.ForumTopic{ID: 1, Date: 10, TopMessage: 100},
				&tg.ForumTopic{ID: 2, Date: 20, TopMessage: 200},
			},
			Messages: []tg.MessageClass{
				&tg.Message{ID: 200, Date: 999},
			},
		})
		if !ok {
			t.Fatal("no offsets")
		}
		if date != 999 || id != 200 || topic != 2 {
			t.Errorf("got date=%d id=%d topic=%d, want 999/200/2", date, id, topic)
		}
	})

	t.Run("orders by create date: uses the topic's date", func(t *testing.T) {
		// With order_by_create_date set the server ordered by forumTopic.date,
		// so the offset must use that even though top_message is resolvable.
		date, id, topic, ok := nextTopicOffsets(&tg.MessagesForumTopics{
			OrderByCreateDate: true,
			Topics: []tg.ForumTopicClass{
				&tg.ForumTopic{ID: 2, Date: 20, TopMessage: 200},
			},
			Messages: []tg.MessageClass{&tg.Message{ID: 200, Date: 999}},
		})
		if !ok {
			t.Fatal("no offsets")
		}
		if date != 20 || id != 200 || topic != 2 {
			t.Errorf("got date=%d id=%d topic=%d, want 20/200/2", date, id, topic)
		}
	})

	t.Run("falls back to the topic's own date", func(t *testing.T) {
		date, _, _, ok := nextTopicOffsets(&tg.MessagesForumTopics{
			Topics: []tg.ForumTopicClass{&tg.ForumTopic{ID: 2, Date: 20, TopMessage: 200}},
		})
		if !ok || date != 20 {
			t.Errorf("date = %d (ok=%v), want 20", date, ok)
		}
	})

	t.Run("empty page", func(t *testing.T) {
		if _, _, _, ok := nextTopicOffsets(&tg.MessagesForumTopics{}); ok {
			t.Error("expected no offsets for an empty page")
		}
	})
}

// TestListTopicsLimit asserts a real per-request limit is sent. Before topics
// were paginated the request went out with limit=0.
func TestListTopicsLimit(t *testing.T) {
	var gotLimit int
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.MessagesGetForumTopicsRequest)
		if !ok {
			return nil, errors.Errorf("unexpected request %T", req)
		}
		gotLimit = r.Limit
		return &tg.MessagesForumTopics{Count: 1, Topics: topicPage(5, 1)}, nil
	})

	res, err := listTopics(context.Background(), api, &tg.InputPeerChannel{ChannelID: 1}, "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if gotLimit != 30 {
		t.Errorf("request limit = %d, want 30", gotLimit)
	}
	if len(res.Topics) != 1 {
		t.Errorf("got %d topics, want 1", len(res.Topics))
	}
}

// TestListTopicsPaginates asserts a forum larger than one page is walked to the
// end, advancing the offsets each time, and that the server's count ends the
// walk without an extra empty request.
func TestListTopicsPaginates(t *testing.T) {
	var reqs []*tg.MessagesGetForumTopicsRequest
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.MessagesGetForumTopicsRequest)
		if !ok {
			return nil, errors.Errorf("unexpected request %T", req)
		}
		reqs = append(reqs, r)
		if len(reqs) == 1 {
			return &tg.MessagesForumTopics{Count: 101, Topics: topicPage(2, topicsPageLimit)}, nil
		}
		return &tg.MessagesForumTopics{Count: 101, Topics: topicPage(2+topicsPageLimit, 1)}, nil
	})

	res, err := listTopics(context.Background(), api, &tg.InputPeerChannel{ChannelID: 1}, "", 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2", len(reqs))
	}
	if reqs[0].Limit != topicsPageLimit || reqs[1].Limit != 20 {
		t.Errorf("limits = %d, %d; want %d, 20", reqs[0].Limit, reqs[1].Limit, topicsPageLimit)
	}
	// The second page must continue from the last topic of the first.
	lastID := 1 + topicsPageLimit
	if reqs[1].OffsetTopic != lastID || reqs[1].OffsetID != topicMsgID(lastID) {
		t.Errorf("offsets = topic %d, id %d; want %d, %d",
			reqs[1].OffsetTopic, reqs[1].OffsetID, lastID, topicMsgID(lastID))
	}
	if len(res.Topics) != topicsPageLimit+1 {
		t.Errorf("got %d topics, want %d", len(res.Topics), topicsPageLimit+1)
	}
	if res.Count != 101 {
		t.Errorf("count = %d, want 101", res.Count)
	}
}

// TestListTopicsShortPageIsNotTheEnd is the regression test for silent
// truncation: Telegram does not document its per-page ceiling, so a page
// shorter than the one requested must not be read as the end of the list.
func TestListTopicsShortPageIsNotTheEnd(t *testing.T) {
	// A 60-topic forum whose pages the server caps at 20, well below the
	// requested limit. id 1 is General, so the server counts 59.
	const (
		total   = 60
		pageCap = 20
	)

	var reqs int
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.MessagesGetForumTopicsRequest)
		if !ok {
			return nil, errors.Errorf("unexpected request %T", req)
		}
		reqs++
		from := r.OffsetTopic + 1
		n := min(pageCap, total-r.OffsetTopic)
		if n <= 0 {
			return &tg.MessagesForumTopics{Count: total - 1}, nil
		}
		return &tg.MessagesForumTopics{Count: total - 1, Topics: topicPage(from, n)}, nil
	})

	res, err := listTopics(context.Background(), api, &tg.InputPeerChannel{ChannelID: 1}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Topics) != total {
		t.Errorf("got %d topics, want all %d (a short page must not end the walk)", len(res.Topics), total)
	}
	if reqs != 3 {
		t.Errorf("made %d requests, want 3", reqs)
	}
}

// TestListTopicsStopsOnEmptyPage asserts the walk still terminates when count
// carries no usable total, which is the correctness backstop.
func TestListTopicsStopsOnEmptyPage(t *testing.T) {
	var reqs int
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.MessagesGetForumTopicsRequest)
		if !ok {
			return nil, errors.Errorf("unexpected request %T", req)
		}
		reqs++
		if r.OffsetTopic >= 5 {
			return &tg.MessagesForumTopics{}, nil
		}
		return &tg.MessagesForumTopics{Topics: topicPage(1, 5)}, nil
	})

	res, err := listTopics(context.Background(), api, &tg.InputPeerChannel{ChannelID: 1}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Topics) != 5 || reqs != 2 {
		t.Errorf("got %d topics in %d requests, want 5 in 2", len(res.Topics), reqs)
	}
}

// TestListTopicsGeneralIsNotCounted guards the count rule against the General
// topic, which is listed but excluded from the server's total. Counting it
// would end the walk one topic early.
func TestListTopicsGeneralIsNotCounted(t *testing.T) {
	var reqs int
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.MessagesGetForumTopicsRequest)
		if !ok {
			return nil, errors.Errorf("unexpected request %T", req)
		}
		reqs++
		if r.OffsetTopic >= 3 {
			return &tg.MessagesForumTopics{Count: 2}, nil
		}
		// ids 1..3, where id 1 is General: three listed, two counted.
		return &tg.MessagesForumTopics{Count: 2, Topics: topicPage(1, 3)}, nil
	})

	res, err := listTopics(context.Background(), api, &tg.InputPeerChannel{ChannelID: 1}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Topics) != 3 {
		t.Errorf("got %d topics, want 3", len(res.Topics))
	}
	if reqs != 1 {
		t.Errorf("made %d requests, want 1 (count reached after the first page)", reqs)
	}
}

// TestListTopicsUnlimited asserts a non-positive limit means "everything", which
// is what --all passes in.
func TestListTopicsUnlimited(t *testing.T) {
	const total = 150

	var reqs int
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.MessagesGetForumTopicsRequest)
		if !ok {
			return nil, errors.Errorf("unexpected request %T", req)
		}
		reqs++
		if r.Limit != topicsPageLimit {
			t.Errorf("unlimited request limit = %d, want %d", r.Limit, topicsPageLimit)
		}
		n := min(topicsPageLimit, total-r.OffsetTopic)
		if n <= 0 {
			return &tg.MessagesForumTopics{Count: total - 1}, nil
		}
		return &tg.MessagesForumTopics{Count: total - 1, Topics: topicPage(r.OffsetTopic+1, n)}, nil
	})

	res, err := listTopics(context.Background(), api, &tg.InputPeerChannel{ChannelID: 1}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Topics) != total {
		t.Errorf("got %d topics, want %d", len(res.Topics), total)
	}
	if reqs != 2 {
		t.Errorf("made %d requests, want 2", reqs)
	}
}

// TestListTopicsQuery asserts --query reaches the request as the optional q
// field, rather than being sent as an empty string.
func TestListTopicsQuery(t *testing.T) {
	for _, tt := range []struct {
		query   string
		wantSet bool
	}{
		{query: "", wantSet: false},
		{query: "release", wantSet: true},
	} {
		var gotQ string
		var gotSet bool
		api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
			r, ok := req.(*tg.MessagesGetForumTopicsRequest)
			if !ok {
				return nil, errors.Errorf("unexpected request %T", req)
			}
			gotQ, gotSet = r.GetQ()
			return &tg.MessagesForumTopics{}, nil
		})

		if _, err := listTopics(context.Background(), api, &tg.InputPeerChannel{ChannelID: 1}, tt.query, 10); err != nil {
			t.Fatal(err)
		}
		if gotSet != tt.wantSet || (tt.wantSet && gotQ != tt.query) {
			t.Errorf("query %q: got %q (set=%v)", tt.query, gotQ, gotSet)
		}
	}
}

func TestTopicIDFromUpdates(t *testing.T) {
	t.Run("finds the creation service message", func(t *testing.T) {
		upd := &tg.Updates{Updates: []tg.UpdateClass{
			&tg.UpdateReadChannelInbox{},
			&tg.UpdateNewChannelMessage{Message: &tg.MessageService{
				ID:     4242,
				Action: &tg.MessageActionTopicCreate{Title: "Deploys"},
			}},
		}}
		id, ok := topicIDFromUpdates(upd)
		if !ok || id != 4242 {
			t.Errorf("id = %d (ok=%v), want 4242", id, ok)
		}
	})

	t.Run("ignores unrelated service messages", func(t *testing.T) {
		upd := &tg.Updates{Updates: []tg.UpdateClass{
			&tg.UpdateNewChannelMessage{Message: &tg.MessageService{
				ID:     1,
				Action: &tg.MessageActionPinMessage{},
			}},
		}}
		if _, ok := topicIDFromUpdates(upd); ok {
			t.Error("matched a non-topic action")
		}
	})

	t.Run("no updates", func(t *testing.T) {
		if _, ok := topicIDFromUpdates(&tg.UpdatesTooLong{}); ok {
			t.Error("expected no id")
		}
	})
}

func TestTopicIDArg(t *testing.T) {
	if id, err := topicIDArg("42"); err != nil || id != 42 {
		t.Errorf("got %d, %v", id, err)
	}
	if _, err := topicIDArg("general"); err == nil {
		t.Error("expected an error for a non-numeric topic id")
	}
}
