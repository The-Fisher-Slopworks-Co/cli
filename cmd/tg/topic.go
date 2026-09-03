package main

import (
	"context"
	"strconv"
	"strings"

	"github.com/go-faster/errors"
	"github.com/spf13/pflag"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
)

// generalTopicID is the id of the implicit "General" topic every forum has. It
// is the only topic that can be hidden, and it has no creation message.
const generalTopicID = 1

// topicOptions holds the --topic flag shared by the peer-taking commands.
type topicOptions struct {
	id int
}

// register binds --topic onto the given flag set.
func (o *topicOptions) register(fs *pflag.FlagSet) {
	fs.IntVar(&o.id, "topic", 0,
		"forum topic id to target (see 'tg topics list'); 1 is the General topic")
}

// resolve merges the --topic flag with a topic id embedded in the peer
// argument. An explicit --topic wins; the returned peer string has the topic
// path segment stripped.
func (o *topicOptions) resolve(peerArg string) (string, int) {
	p, topic := splitTopicLink(peerArg)
	if o.id != 0 {
		return p, o.id
	}
	return p, topic
}

// trimLinkHost strips the scheme and a Telegram link host from arg, reporting
// whether arg looked like a deep link at all.
func trimLinkHost(arg string) (string, bool) {
	s := strings.TrimSpace(arg)
	for _, scheme := range []string{"https://", "http://", "//"} {
		s = strings.TrimPrefix(s, scheme)
	}
	s = strings.TrimPrefix(s, "www.")
	for _, host := range []string{"t.me", "telegram.me", "telegram.dog"} {
		if rest, ok := strings.CutPrefix(s, host+"/"); ok {
			return rest, true
		}
	}
	return "", false
}

// splitTopicLink extracts a forum topic id from a message deep link, returning
// the peer part of the link and the topic id.
//
// Telegram carries the topic as an extra path segment:
//
//	t.me/<username>/<topic>/<message>
//	t.me/c/<channel-id>/<topic>/<message>
//
// A two-segment link (t.me/<username>/<message>) addresses a message, not a
// topic. Anything that is not a topic link is returned unchanged, so peers that
// already resolved keep resolving exactly as before.
func splitTopicLink(arg string) (string, int) {
	rest, ok := trimLinkHost(arg)
	if !ok {
		return arg, 0
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	segs := strings.Split(strings.Trim(rest, "/"), "/")

	// Private links address the channel by its numeric id, which the CLI
	// already understands as "id:<n>" (resolved from the peer cache).
	if segs[0] == "c" {
		if len(segs) < 3 || !isDigits(segs[1]) || !isDigits(segs[2]) {
			return arg, 0
		}
		peerArg := peerIDPrefix + segs[1]
		if len(segs) < 4 || !isDigits(segs[3]) {
			// t.me/c/<channel-id>/<message>: no topic.
			return peerArg, 0
		}
		topic, _ := strconv.Atoi(segs[2])
		return peerArg, topic
	}

	if len(segs) < 3 || !isDigits(segs[1]) || !isDigits(segs[2]) {
		return arg, 0
	}
	topic, _ := strconv.Atoi(segs[1])
	return "@" + segs[0], topic
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// forumChannel resolves a peer argument and asserts that it is a supergroup
// with forum topics enabled, so topic commands fail with an actionable message
// instead of a raw RPC error.
func forumChannel(ctx context.Context, m *peerManager, arg string) (peers.Channel, error) {
	if isSelf(arg) {
		return peers.Channel{}, errors.New("forum topics only exist in supergroups, not in Saved Messages")
	}
	p, err := resolvePeerArg(ctx, m, arg)
	if err != nil {
		return peers.Channel{}, errors.Wrapf(err, "resolve %q", arg)
	}
	ch, ok := p.(peers.Channel)
	if !ok {
		return peers.Channel{}, errors.Errorf("%q is not a supergroup: forum topics only exist in supergroups", arg)
	}
	if !ch.Raw().Forum {
		return peers.Channel{}, errors.Errorf(
			"%q has no forum topics; enable them with `tg topics enable %s`", arg, arg)
	}
	return ch, nil
}

// requireForum checks that a --topic target really is a forum. It is a no-op
// when no topic was requested.
func requireForum(ctx context.Context, m *peerManager, arg string, topic int) error {
	if topic == 0 {
		return nil
	}
	_, err := forumChannel(ctx, m, arg)
	return err
}

// topicInvoker wraps a tg.Invoker and directs outgoing sends into a forum
// topic.
//
// gotd's message.Builder cannot set top_msg_id: Builder.Reply fills only
// InputReplyToMessage.ReplyToMsgID, and the field has no setter (checked
// against gotd/td v0.160.0 and v0.161.0). Rather than reimplement the styling,
// album and upload machinery per command just to reach one field, the topic is
// stamped onto send-shaped requests on their way out — so send, reply, upload,
// album, poll, schedule, forward and draft all gain topics through one seam.
type topicInvoker struct {
	next  tg.Invoker
	topic int
}

// Invoke implements tg.Invoker.
func (i topicInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	applyTopic(input, i.topic)
	return i.next.Invoke(ctx, input, output)
}

// replyToSetter is the shape of a request whose target topic lives in its
// reply_to field.
type replyToSetter interface {
	GetReplyTo() (tg.InputReplyToClass, bool)
	SetReplyTo(tg.InputReplyToClass)
}

// applyTopic stamps the topic onto a request. Request types are matched
// explicitly rather than by a blanket interface check: several unrelated RPCs
// carry the same fields, and rewriting those would silently change their
// meaning.
func applyTopic(input bin.Encoder, topic int) {
	if topic == 0 {
		return
	}
	switch req := input.(type) {
	// These carry top_msg_id directly; their reply_to means something else.
	case *tg.MessagesForwardMessagesRequest:
		req.SetTopMsgID(topic)
	case *tg.MessagesSetTypingRequest:
		req.SetTopMsgID(topic)

	// For the rest the topic travels inside reply_to.
	case *tg.MessagesSendMessageRequest:
		setTopicReplyTo(req, topic)
	case *tg.MessagesSendMediaRequest:
		setTopicReplyTo(req, topic)
	case *tg.MessagesSendMultiMediaRequest:
		setTopicReplyTo(req, topic)
	case *tg.MessagesSendInlineBotResultRequest:
		setTopicReplyTo(req, topic)
	case *tg.MessagesSaveDraftRequest:
		setTopicReplyTo(req, topic)
	}
}

// setTopicReplyTo points a request's reply_to at a topic, preserving any
// existing reply target so that `tg reply --topic` replies inside the topic.
func setTopicReplyTo(req replyToSetter, topic int) {
	rt, ok := req.GetReplyTo()
	if !ok || rt == nil {
		rt = &tg.InputReplyToMessage{}
	}
	msg, ok := rt.(*tg.InputReplyToMessage)
	if !ok {
		// Replying to a story: not a topic target.
		return
	}
	msg.SetTopMsgID(topic)
	req.SetReplyTo(msg)
}

// topicClient returns a client whose sends land in the given topic. With no
// topic it returns api unchanged.
func topicClient(api *tg.Client, topic int) *tg.Client {
	if topic == 0 {
		return api
	}
	return tg.NewClient(topicInvoker{next: api.Invoker(), topic: topic})
}

// senderIn is sender, scoped to a forum topic.
func (a *app) senderIn(api *tg.Client, topic int) (*message.Sender, *peerManager, error) {
	m, err := a.manager(api)
	if err != nil {
		return nil, nil, err
	}
	return message.NewSender(topicClient(api, topic)).WithResolver(peerResolver{pm: m}), m, nil
}

// topicFromReply splits a message's reply header into the forum topic it
// belongs to and the message it actually replies to.
//
// Telegram encodes both in one header: a reply inside a topic carries
// reply_to_top_id, while the first message of a topic branch points its
// reply_to_msg_id straight at the topic and only sets the forum_topic flag.
func topicFromReply(h *tg.MessageReplyHeader) (topic, replyTo int) {
	if h == nil {
		return 0, 0
	}
	if !h.ForumTopic {
		return 0, h.ReplyToMsgID
	}
	if top, ok := h.GetReplyToTopID(); ok && top != 0 {
		return top, h.ReplyToMsgID
	}
	return h.ReplyToMsgID, 0
}
