package main

import (
	"context"
	"fmt"
	"io"

	"github.com/go-faster/errors"
	"github.com/spf13/cobra"

	"github.com/gotd/td/tg"
)

// readResult is the result of `tg read`.
type readResult struct {
	OK bool `json:"ok"`
}

// MarshalText renders a short acknowledgement.
func (r readResult) MarshalText(w io.Writer) error {
	_, err := fmt.Fprintln(w, "marked as read")
	return err
}

// markRead marks the peer's history as read up to the latest message. A
// non-zero topic marks just that forum topic, which Telegram tracks as the
// discussion thread of the topic's root message.
func markRead(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, topic int) error {
	if topic != 0 {
		if _, err := api.MessagesReadDiscussion(ctx, &tg.MessagesReadDiscussionRequest{
			Peer:      peer,
			MsgID:     topic,
			ReadMaxID: 0,
		}); err != nil {
			return errors.Wrap(err, "messages.readDiscussion")
		}
		return nil
	}
	if ch, ok := peer.(*tg.InputPeerChannel); ok {
		if _, err := api.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			MaxID:   0,
		}); err != nil {
			return errors.Wrap(err, "channels.readHistory")
		}
		return nil
	}
	if _, err := api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{
		Peer:  peer,
		MaxID: 0,
	}); err != nil {
		return errors.Wrap(err, "messages.readHistory")
	}
	return nil
}

func (a *app) newReadCmd() *cobra.Command {
	var topic topicOptions

	cmd := &cobra.Command{
		Use:     "read <peer>",
		Short:   "Mark a chat as read",
		GroupID: groupMessaging,
		Long: `Mark a peer's history as read up to the latest message. The peer is
me/self, @username, phone, or a t.me link.`,
		Example: `  tg read @durov
  tg read @somechannel
  tg read @myforum --topic 42`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: peerArgCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				m, err := a.manager(api)
				if err != nil {
					return err
				}
				peerArg, topicID := topic.resolve(args[0])
				peer, err := resolvePeer(ctx, m, peerArg)
				if err != nil {
					return err
				}
				if err := markRead(ctx, api, peer, topicID); err != nil {
					return err
				}
				return a.printer.Emit(readResult{OK: true})
			})
		},
	}

	topic.register(cmd.Flags())

	return cmd
}
