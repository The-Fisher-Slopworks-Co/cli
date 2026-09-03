package main

import (
	"context"
	"strconv"

	"github.com/go-faster/errors"
	"github.com/spf13/cobra"

	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/tg"
)

func (a *app) newReplyCmd() *cobra.Command {
	var (
		msg   messageOptions
		topic topicOptions
	)

	cmd := &cobra.Command{
		Use:     "reply <peer> <message-id> <text>",
		Short:   "Reply to a specific message",
		GroupID: groupMessaging,
		Long: `Send a reply to a specific message in a peer's history. The peer is
me/self, @username, phone, or a t.me link.`,
		Example: `  tg reply @durov 12345 "great post"
  tg reply me 1000 "note to self"
  tg reply @myforum 999 "on it" --topic 42`,
		Args:              cobra.ExactArgs(3),
		ValidArgsFunction: peerArgCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			replyTo, err := strconv.Atoi(args[1])
			if err != nil {
				return errors.Wrap(err, "message-id must be an integer")
			}
			text := args[2]

			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				peer, topicID := topic.resolve(args[0])
				sender, m, err := a.senderIn(api, topicID)
				if err != nil {
					return err
				}
				if err := requireForum(ctx, m, peer, topicID); err != nil {
					return err
				}
				bf, err := builderFor(ctx, m, sender, peer)
				if err != nil {
					return err
				}

				b, options := msg.apply(bf, text)
				id, err := unpack.MessageID(b.Reply(replyTo).StyledText(ctx, options...))
				if err != nil {
					return errors.Wrap(err, "reply")
				}
				return a.printer.Emit(sentResult{Peer: peer, MessageID: id})
			})
		},
	}

	msg.register(cmd.Flags())
	topic.register(cmd.Flags())

	return cmd
}
