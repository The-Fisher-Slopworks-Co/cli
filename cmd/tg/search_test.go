package main

import (
	"context"
	"testing"

	"github.com/go-faster/errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestSearchMessages(t *testing.T) {
	var gotFilter tg.MessagesFilterClass
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		if r, ok := req.(*tg.MessagesSearchRequest); ok {
			gotFilter = r.Filter
			return &tg.MessagesMessages{
				Messages: []tg.MessageClass{
					&tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 9}, Message: "found it", Date: 5},
				},
				Users: []tg.UserClass{&tg.User{ID: 9, Username: "bob"}},
			}, nil
		}
		return nil, errors.Errorf("unexpected request %T", req)
	})

	res, err := searchMessages(context.Background(), api, &tg.InputPeerUser{UserID: 9}, "found", nil, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Text != "found it" {
		t.Errorf("unexpected messages: %+v", res.Messages)
	}
	if _, ok := gotFilter.(*tg.InputMessagesFilterEmpty); !ok {
		t.Errorf("default filter = %T, want InputMessagesFilterEmpty", gotFilter)
	}
}

func TestPinnedUsesPinnedFilter(t *testing.T) {
	var gotFilter tg.MessagesFilterClass
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		if r, ok := req.(*tg.MessagesSearchRequest); ok {
			gotFilter = r.Filter
			return &tg.MessagesMessages{}, nil
		}
		return nil, errors.Errorf("unexpected request %T", req)
	})

	if _, err := searchMessages(context.Background(), api, &tg.InputPeerUser{UserID: 1}, "", &tg.InputMessagesFilterPinned{}, 10, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotFilter.(*tg.InputMessagesFilterPinned); !ok {
		t.Errorf("filter = %T, want InputMessagesFilterPinned", gotFilter)
	}
}

// TestSearchMessagesTopic asserts --topic reaches the request as top_msg_id.
func TestSearchMessagesTopic(t *testing.T) {
	var gotTopic int
	var gotSet bool
	api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
		r, ok := req.(*tg.MessagesSearchRequest)
		if !ok {
			return nil, errors.Errorf("unexpected request %T", req)
		}
		gotTopic, gotSet = r.GetTopMsgID()
		return &tg.MessagesMessages{}, nil
	})

	if _, err := searchMessages(context.Background(), api, &tg.InputPeerChannel{ChannelID: 1}, "q", nil, 10, 42); err != nil {
		t.Fatal(err)
	}
	if !gotSet || gotTopic != 42 {
		t.Errorf("top_msg_id = %d (set=%v), want 42", gotTopic, gotSet)
	}
}
