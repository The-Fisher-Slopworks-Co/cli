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
		return &tg.MessagesForumTopics{
			Count:  1,
			Topics: []tg.ForumTopicClass{&tg.ForumTopic{ID: 1, Title: "General", Peer: &tg.PeerChannel{ChannelID: 1}, FromID: &tg.PeerUser{UserID: 7}}},
		}, nil
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

// TestListTopicsPaginates asserts a limit larger than one page is fetched in
// several requests, advancing the offsets each time.
func TestListTopicsPaginates(t *testing.T) {
	var reqs []*tg.MessagesGetForumTopicsRequest
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.MessagesGetForumTopicsRequest)
		if !ok {
			return nil, errors.Errorf("unexpected request %T", req)
		}
		reqs = append(reqs, r)

		// One full page, then a short one that ends the walk.
		if len(reqs) == 1 {
			topics := make([]tg.ForumTopicClass, 0, topicsPageLimit)
			for i := 1; i <= topicsPageLimit; i++ {
				topics = append(topics, &tg.ForumTopic{ID: i, Date: i, TopMessage: 1000 + i, Peer: &tg.PeerChannel{ChannelID: 1}, FromID: &tg.PeerUser{UserID: 7}})
			}
			return &tg.MessagesForumTopics{Count: 120, Topics: topics}, nil
		}
		return &tg.MessagesForumTopics{
			Count:  120,
			Topics: []tg.ForumTopicClass{&tg.ForumTopic{ID: 101, Date: 101, TopMessage: 1101, Peer: &tg.PeerChannel{ChannelID: 1}, FromID: &tg.PeerUser{UserID: 7}}},
		}, nil
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
	if reqs[1].OffsetTopic != topicsPageLimit || reqs[1].OffsetID != 1000+topicsPageLimit {
		t.Errorf("offsets = topic %d, id %d", reqs[1].OffsetTopic, reqs[1].OffsetID)
	}
	if len(res.Topics) != topicsPageLimit+1 {
		t.Errorf("got %d topics, want %d", len(res.Topics), topicsPageLimit+1)
	}
	if res.Count != 120 {
		t.Errorf("count = %d, want 120", res.Count)
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
