package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-faster/errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"

	"github.com/gotd/cli/internal/peercache"
)

func TestSplitTopicLink(t *testing.T) {
	for _, tt := range []struct {
		in        string
		wantPeer  string
		wantTopic int
	}{
		// Not links: returned untouched.
		{in: "@durov", wantPeer: "@durov"},
		{in: "me", wantPeer: "me"},
		{in: "id:42", wantPeer: "id:42"},
		{in: "+79001234567", wantPeer: "+79001234567"},

		// Plain peer and message links keep today's behaviour.
		{in: "https://t.me/durov", wantPeer: "https://t.me/durov"},
		{in: "t.me/durov/1337", wantPeer: "t.me/durov/1337"},
		{in: "t.me/joinchat/AAAA", wantPeer: "t.me/joinchat/AAAA"},
		{in: "t.me/+AbCdEf", wantPeer: "t.me/+AbCdEf"},

		// Topic links.
		{in: "https://t.me/myforum/42/1337", wantPeer: "@myforum", wantTopic: 42},
		{in: "t.me/myforum/42/1337", wantPeer: "@myforum", wantTopic: 42},
		{in: "http://www.telegram.me/myforum/42/1337", wantPeer: "@myforum", wantTopic: 42},
		{in: "https://t.me/myforum/42/1337?single", wantPeer: "@myforum", wantTopic: 42},

		// Private links address the channel by numeric id.
		{in: "https://t.me/c/1234567890/42/1337", wantPeer: "id:1234567890", wantTopic: 42},
		{in: "https://t.me/c/1234567890/1337", wantPeer: "id:1234567890"},
		{in: "https://t.me/c/notanid/1337", wantPeer: "https://t.me/c/notanid/1337"},
	} {
		gotPeer, gotTopic := splitTopicLink(tt.in)
		if gotPeer != tt.wantPeer || gotTopic != tt.wantTopic {
			t.Errorf("splitTopicLink(%q) = %q, %d; want %q, %d",
				tt.in, gotPeer, gotTopic, tt.wantPeer, tt.wantTopic)
		}
	}
}

// TestTopicOptionsResolve asserts an explicit --topic wins over a topic carried
// by a link, and that the peer is still stripped of the topic segments.
func TestTopicOptionsResolve(t *testing.T) {
	var none topicOptions
	if p, topic := none.resolve("@myforum"); p != "@myforum" || topic != 0 {
		t.Errorf("no flag: got %q, %d", p, topic)
	}

	flag := topicOptions{id: 7}
	if p, topic := flag.resolve("@myforum"); p != "@myforum" || topic != 7 {
		t.Errorf("flag only: got %q, %d", p, topic)
	}
	if p, topic := flag.resolve("t.me/myforum/42/1337"); p != "@myforum" || topic != 7 {
		t.Errorf("flag over link: got %q, %d want @myforum, 7", p, topic)
	}
	if p, topic := none.resolve("t.me/myforum/42/1337"); p != "@myforum" || topic != 42 {
		t.Errorf("link only: got %q, %d", p, topic)
	}
}

func TestTopicFromReply(t *testing.T) {
	for _, tt := range []struct {
		name        string
		header      *tg.MessageReplyHeader
		wantTopic   int
		wantReplyTo int
	}{
		{name: "nil"},
		{
			name:        "plain reply",
			header:      &tg.MessageReplyHeader{ReplyToMsgID: 10},
			wantReplyTo: 10,
		},
		{
			name:      "first message of a topic branch",
			header:    &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 42},
			wantTopic: 42,
		},
		{
			name: "reply inside a topic",
			header: func() *tg.MessageReplyHeader {
				h := &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 99}
				h.SetReplyToTopID(42)
				return h
			}(),
			wantTopic:   42,
			wantReplyTo: 99,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			topic, replyTo := topicFromReply(tt.header)
			if topic != tt.wantTopic || replyTo != tt.wantReplyTo {
				t.Errorf("got topic=%d replyTo=%d, want topic=%d replyTo=%d",
					topic, replyTo, tt.wantTopic, tt.wantReplyTo)
			}
		})
	}
}

func TestApplyTopic(t *testing.T) {
	t.Run("send", func(t *testing.T) {
		req := &tg.MessagesSendMessageRequest{Message: "hi"}
		applyTopic(req, 42)
		rt, ok := req.GetReplyTo()
		if !ok {
			t.Fatal("reply_to not set")
		}
		m, ok := rt.(*tg.InputReplyToMessage)
		if !ok {
			t.Fatalf("reply_to = %T", rt)
		}
		if got, ok := m.GetTopMsgID(); !ok || got != 42 {
			t.Errorf("top_msg_id = %d (set=%v), want 42", got, ok)
		}
		if m.ReplyToMsgID != 0 {
			t.Errorf("reply_to_msg_id = %d, want 0", m.ReplyToMsgID)
		}
	})

	t.Run("reply inside a topic keeps its target", func(t *testing.T) {
		req := &tg.MessagesSendMessageRequest{}
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: 99})
		applyTopic(req, 42)
		rt, _ := req.GetReplyTo()
		m := rt.(*tg.InputReplyToMessage)
		if m.ReplyToMsgID != 99 {
			t.Errorf("reply_to_msg_id = %d, want 99", m.ReplyToMsgID)
		}
		if got, _ := m.GetTopMsgID(); got != 42 {
			t.Errorf("top_msg_id = %d, want 42", got)
		}
	})

	t.Run("forward uses its own top_msg_id", func(t *testing.T) {
		req := &tg.MessagesForwardMessagesRequest{ID: []int{1}}
		applyTopic(req, 42)
		if got, ok := req.GetTopMsgID(); !ok || got != 42 {
			t.Errorf("top_msg_id = %d (set=%v), want 42", got, ok)
		}
		if _, ok := req.GetReplyTo(); ok {
			t.Error("forward must not gain a reply_to")
		}
	})

	t.Run("zero topic is a no-op", func(t *testing.T) {
		req := &tg.MessagesSendMessageRequest{Message: "hi"}
		applyTopic(req, 0)
		if _, ok := req.GetReplyTo(); ok {
			t.Error("reply_to set for a zero topic")
		}
	})

	t.Run("story replies are left alone", func(t *testing.T) {
		req := &tg.MessagesSendMessageRequest{}
		req.SetReplyTo(&tg.InputReplyToStory{StoryID: 3})
		applyTopic(req, 42)
		rt, _ := req.GetReplyTo()
		if _, ok := rt.(*tg.InputReplyToStory); !ok {
			t.Errorf("reply_to = %T, want InputReplyToStory", rt)
		}
	})

	t.Run("unrelated requests are untouched", func(t *testing.T) {
		req := &tg.MessagesSearchRequest{Q: "x"}
		applyTopic(req, 42)
		if _, ok := req.GetTopMsgID(); ok {
			t.Error("messages.search must not be rewritten by the topic invoker")
		}
	})
}

// TestTopicInvokerSend drives a real message.Sender through the topic invoker,
// which is the seam that gives every sending command topic support.
func TestTopicInvokerSend(t *testing.T) {
	var got *tg.MessagesSendMessageRequest
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.MessagesSendMessageRequest)
		if !ok {
			return nil, errors.Errorf("unexpected request %T", req)
		}
		got = r
		return &tg.Updates{Updates: []tg.UpdateClass{
			&tg.UpdateNewMessage{Message: &tg.Message{
				ID:     1000,
				PeerID: &tg.PeerUser{UserID: 1},
			}},
		}}, nil
	})

	sender := message.NewSender(topicClient(api, 42))
	id, err := unpack.MessageID(sender.Self().StyledText(context.Background(), styling.Plain("hi")))
	if err != nil {
		t.Fatal(err)
	}
	if id != 1000 {
		t.Errorf("message id = %d, want 1000", id)
	}
	rt, ok := got.GetReplyTo()
	if !ok {
		t.Fatal("reply_to not set on the outgoing request")
	}
	m, ok := rt.(*tg.InputReplyToMessage)
	if !ok {
		t.Fatalf("reply_to = %T", rt)
	}
	if topic, ok := m.GetTopMsgID(); !ok || topic != 42 {
		t.Errorf("top_msg_id = %d (set=%v), want 42", topic, ok)
	}
}

// TestTopicClientPassthrough asserts no wrapping happens without a topic, so
// the non-forum path stays exactly as it was.
func TestTopicClientPassthrough(t *testing.T) {
	api := newFuncAPI(t, func(bin.Encoder) (bin.Encoder, error) { return nil, nil })
	if topicClient(api, 0) != api {
		t.Error("topicClient wrapped the client for a zero topic")
	}
	if topicClient(api, 1) == api {
		t.Error("topicClient did not wrap the client for a topic")
	}
}

func TestMessageStreamMatch(t *testing.T) {
	for _, tt := range []struct {
		name   string
		peerID int64
		topic  int
		ev     watchEvent
		want   bool
	}{
		{
			name: "no filters",
			ev:   watchEvent{Peer: peerRef{ID: 1}},
			want: true,
		},
		{
			name:   "wrong peer",
			peerID: 2,
			ev:     watchEvent{Peer: peerRef{ID: 1}},
		},
		{
			name:   "matching topic",
			peerID: 1,
			topic:  42,
			ev:     watchEvent{Peer: peerRef{ID: 1}, Message: messageItem{TopicID: 42}},
			want:   true,
		},
		{
			name:   "other topic",
			peerID: 1,
			topic:  42,
			ev:     watchEvent{Peer: peerRef{ID: 1}, Message: messageItem{TopicID: 7}},
		},
		{
			name:   "no topic header means General",
			peerID: 1,
			topic:  generalTopicID,
			ev:     watchEvent{Peer: peerRef{ID: 1}},
			want:   true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &messageStream{filterID: tt.peerID, filterTopic: tt.topic}
			if got := s.match(tt.ev); got != tt.want {
				t.Errorf("match = %v, want %v", got, tt.want)
			}
		})
	}
}

// newSeededManager builds a peerManager over a scratch peer cache and the given
// API, pre-seeded with access hashes so "id:<n>" resolves through the cache.
// Unlike newTestManager it lets the caller answer the follow-up RPCs.
func newSeededManager(t *testing.T, api *tg.Client, seed map[peers.Key]peers.Value) *peerManager {
	t.Helper()
	store, err := peercache.Open(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range seed {
		if err := store.Save(context.Background(), k, v); err != nil {
			t.Fatal(err)
		}
	}
	return &peerManager{Manager: peers.Options{Storage: store}.Build(api), store: store}
}

func TestRequireForum(t *testing.T) {
	ctx := context.Background()

	t.Run("no topic is a no-op", func(t *testing.T) {
		api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
			return nil, errors.Errorf("no RPC expected, got %T", req)
		})
		if err := requireForum(ctx, newSeededManager(t, api, nil), "@anything", 0); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("Saved Messages has no topics", func(t *testing.T) {
		api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
			return nil, errors.Errorf("no RPC expected, got %T", req)
		})
		err := requireForum(ctx, newSeededManager(t, api, nil), "me", 42)
		if err == nil || !strings.Contains(err.Error(), "supergroups") {
			t.Errorf("error = %v, want a supergroup-only message", err)
		}
	})

	t.Run("a supergroup without topics is rejected", func(t *testing.T) {
		api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
			if _, ok := req.(*tg.ChannelsGetChannelsRequest); !ok {
				return nil, errors.Errorf("unexpected request %T", req)
			}
			return &tg.MessagesChats{Chats: []tg.ChatClass{
				&tg.Channel{ID: 7, AccessHash: 9, Title: "plain group", Photo: &tg.ChatPhotoEmpty{}},
			}}, nil
		})
		m := newSeededManager(t, api, map[peers.Key]peers.Value{
			{Prefix: "channel_", ID: 7}: {AccessHash: 9},
		})
		err := requireForum(ctx, m, "id:7", 42)
		if err == nil || !strings.Contains(err.Error(), "topics enable") {
			t.Errorf("error = %v, want a hint to enable topics", err)
		}
	})

	t.Run("a forum passes", func(t *testing.T) {
		api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
			if _, ok := req.(*tg.ChannelsGetChannelsRequest); !ok {
				return nil, errors.Errorf("unexpected request %T", req)
			}
			return &tg.MessagesChats{Chats: []tg.ChatClass{
				&tg.Channel{ID: 7, AccessHash: 9, Title: "forum", Forum: true, Photo: &tg.ChatPhotoEmpty{}},
			}}, nil
		})
		m := newSeededManager(t, api, map[peers.Key]peers.Value{
			{Prefix: "channel_", ID: 7}: {AccessHash: 9},
		})
		if err := requireForum(ctx, m, "id:7", 42); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
