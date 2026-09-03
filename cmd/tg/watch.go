package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/go-faster/errors"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/tg"

	"github.com/gotd/cli/internal/output"
)

// watchEvent is one streamed message.
type watchEvent struct {
	Peer    peerRef     `json:"peer"`
	Message messageItem `json:"message"`
}

// messageStream turns incoming new-message updates into watchEvents, filtered by
// an optional peer id and forum topic, delivered to onEvent. Safe for concurrent
// dispatch.
type messageStream struct {
	filterID    int64
	filterTopic int
	onEvent     func(watchEvent)
	mu          sync.Mutex
}

func (s *messageStream) handle(msg tg.MessageClass, e tg.Entities) {
	m, ok := msg.(*tg.Message)
	if !ok {
		return
	}
	ent := peer.EntitiesFromUpdate(e)
	ev := watchEvent{Peer: describePeer(m.PeerID, ent), Message: buildMessageItem(m, ent)}
	if !s.match(ev) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onEvent(ev)
}

// match reports whether an event passes the peer and topic filters.
func (s *messageStream) match(ev watchEvent) bool {
	if s.filterID != 0 && ev.Peer.ID != s.filterID {
		return false
	}
	if s.filterTopic == 0 {
		return true
	}
	// Messages posted straight to the General topic carry no reply header, so
	// an absent topic means General rather than "unknown".
	topic := ev.Message.TopicID
	if topic == 0 {
		topic = generalTopicID
	}
	return topic == s.filterTopic
}

// register wires the stream onto an update dispatcher.
func (s *messageStream) register(d tg.UpdateDispatcher) {
	d.OnNewMessage(func(_ context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		s.handle(u.Message, e)
		return nil
	})
	d.OnNewChannelMessage(func(_ context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		s.handle(u.Message, e)
		return nil
	})
}

// resolveFilterFor resolves an optional peer argument to a filter id, using the
// given account's peer cache. It also returns any topic id carried by the
// argument (a t.me topic link).
func (a *app) resolveFilterFor(
	ctx context.Context,
	api *tg.Client,
	st *accountState,
	args []string,
	topic topicOptions,
) (int64, int, error) {
	if len(args) == 0 {
		if topic.id != 0 {
			return 0, 0, errors.New("--topic needs a peer argument")
		}
		return 0, 0, nil
	}
	peerArg, topicID := topic.resolve(args[0])
	m, err := a.managerFor(api, st)
	if err != nil {
		return 0, 0, err
	}
	p, err := resolvePeerArg(ctx, m, peerArg)
	if err != nil {
		return 0, 0, errors.Wrapf(err, "resolve %q", peerArg)
	}
	return p.ID(), topicID, nil
}

// emitLine writes one streamed event (JSON line or text line) to stdout. When
// account is non-empty (multi-account watch) it is included.
func emitLine(format output.Format, account string, ev watchEvent) {
	if format == output.JSON {
		payload := struct {
			Account string `json:"account,omitempty"`
			watchEvent
		}{Account: account, watchEvent: ev}
		b, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintln(os.Stdout, string(b))
		return
	}
	line := ev.Peer.label()
	if ev.Message.Text != "" {
		line += ": " + ev.Message.Text
	} else if ev.Message.Media != "" {
		line += ": [" + ev.Message.Media + "]"
	}
	if account != "" {
		line = "[" + account + "] " + line
	}
	_, _ = fmt.Fprintln(os.Stdout, line)
}

func (a *app) newWatchCmd() *cobra.Command {
	var topic topicOptions

	cmd := &cobra.Command{
		Use:     "watch [peer]",
		Short:   "Stream new messages as they arrive",
		GroupID: groupMessaging,
		Long: `Stream incoming messages (optionally for one peer, or one forum topic
of it) as JSON lines until interrupted.`,
		Example:           "  tg watch\n  tg watch @durov --output json\n  tg watch @myforum --topic 42",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: peerArgCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			labels, err := a.selectedLabels()
			if err != nil {
				return err
			}
			if len(labels) > 1 {
				return a.watchAll(cmd.Context(), labels, args, topic)
			}
			return a.watchOne(cmd.Context(), labels[0], "", args, topic)
		},
	}

	topic.register(cmd.Flags())

	return cmd
}

// watchOne streams messages from a single account. The label header (account)
// is non-empty only in multi-account mode.
func (a *app) watchOne(ctx context.Context, label, header string, args []string, topic topicOptions) error {
	st, err := a.accountState(label)
	if err != nil {
		return err
	}
	return a.watchWith(ctx, st, header, args, topic, nil)
}

// watchAll streams messages from every account concurrently, merged into one
// labeled stream.
func (a *app) watchAll(ctx context.Context, labels, args []string, topic topicOptions) error {
	var mu sync.Mutex
	g, ctx := errgroup.WithContext(ctx)
	for _, label := range labels {
		st, err := a.accountState(label)
		if err != nil {
			return err
		}
		g.Go(func() error {
			if err := a.watchWith(ctx, st, st.label, args, topic, &mu); err != nil {
				return errors.Wrapf(err, "account %q", st.label)
			}
			return nil
		})
	}
	return g.Wait()
}

// watchWith connects to one account and streams until ctx is done. mu, if set,
// serializes stdout across concurrent accounts.
func (a *app) watchWith(
	ctx context.Context,
	st *accountState,
	header string,
	args []string,
	topic topicOptions,
	mu *sync.Mutex,
) error {
	format := a.printer.Format()
	return a.connectWith(ctx, st, runParams{auth: authUser, updates: true},
		func(ctx context.Context, client *telegram.Client, d tg.UpdateDispatcher) error {
			if err := requireAuth(ctx, client); err != nil {
				return err
			}
			filterID, filterTopic, err := a.resolveFilterFor(ctx, client.API(), st, args, topic)
			if err != nil {
				return err
			}

			stream := &messageStream{
				filterID:    filterID,
				filterTopic: filterTopic,
				onEvent: func(ev watchEvent) {
					if mu != nil {
						mu.Lock()
						defer mu.Unlock()
					}
					emitLine(format, header, ev)
				},
			}
			stream.register(d)

			_, _ = fmt.Fprintf(os.Stderr, "Watching %s for new messages (Ctrl-C to stop)…\n", st.label)
			<-ctx.Done()
			return nil
		})
}

// requireAuth returns errNotAuthorized if the session is not logged in.
func requireAuth(ctx context.Context, client *telegram.Client) error {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return errors.Wrap(err, "auth status")
	}
	if !status.Authorized {
		return errNotAuthorized
	}
	return nil
}
