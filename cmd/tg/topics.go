package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/go-faster/errors"
	"github.com/spf13/cobra"

	"github.com/gotd/td/tg"
)

// topicsPageLimit is the per-request topic count. Telegram serves at most 100
// per messages.getForumTopics call.
const topicsPageLimit = 100

// randomID returns a random int64 suitable for MTProto random_id fields.
func randomID() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, errors.Wrap(err, "read random")
	}
	return int64(binary.LittleEndian.Uint64(b[:])), nil //nolint:gosec // random_id, not security-sensitive
}

// topicItem is one forum topic.
type topicItem struct {
	ID             int    `json:"id"`
	Title          string `json:"title"`
	Date           int    `json:"date,omitempty"`
	TopMessage     int    `json:"top_message,omitempty"`
	Unread         int    `json:"unread,omitempty"`
	UnreadMentions int    `json:"unread_mentions,omitempty"`
	Closed         bool   `json:"closed,omitempty"`
	Pinned         bool   `json:"pinned,omitempty"`
	Hidden         bool   `json:"hidden,omitempty"`
	My             bool   `json:"my,omitempty"`
	// Deleted marks a topic Telegram reported as gone (forumTopicDeleted),
	// which carries nothing but an id.
	Deleted   bool `json:"deleted,omitempty"`
	IconColor int  `json:"icon_color,omitempty"`
	// IconEmojiID is a custom emoji (document) id. It is rendered as a string
	// because the value exceeds what a JSON consumer using float64 numbers can
	// represent exactly.
	IconEmojiID string   `json:"icon_emoji_id,omitempty"`
	From        *peerRef `json:"from,omitempty"`
}

// flags renders the boolean state of a topic for text output.
func (t topicItem) flags() []string {
	var out []string
	for _, f := range []struct {
		on   bool
		name string
	}{
		{t.Pinned, "pinned"},
		{t.Closed, "closed"},
		{t.Hidden, "hidden"},
		{t.My, "mine"},
		{t.Deleted, "deleted"},
	} {
		if f.on {
			out = append(out, f.name)
		}
	}
	return out
}

// topicsResult is the result of `tg topics list` and `tg topics get`.
type topicsResult struct {
	Topics []topicItem `json:"topics"`
	// Count is the server's total for the query, which can exceed the number
	// returned. It does not include the General topic: that one exists
	// implicitly in every forum, so it is listed but never counted.
	Count int `json:"count,omitempty"`
}

// MarshalText renders one topic per line.
func (r topicsResult) MarshalText(w io.Writer) error {
	for _, t := range r.Topics {
		line := fmt.Sprintf("#%-6d %s", t.ID, t.Title)
		if t.Unread > 0 {
			line += fmt.Sprintf("  unread=%d", t.Unread)
		}
		if f := t.flags(); len(f) > 0 {
			line += "  [" + strings.Join(f, ",") + "]"
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// createdTopicResult is the result of `tg topics create`.
type createdTopicResult struct {
	TopicID int    `json:"topic_id"`
	Title   string `json:"title"`
}

// MarshalText renders the created topic's id, which is what a caller needs in
// order to post into it.
func (r createdTopicResult) MarshalText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "created topic #%d %s\n", r.TopicID, r.Title)
	return err
}

// newTopicsResult maps a messages.forumTopics response into a result, resolving
// topic creators through the response's own entities.
func newTopicsResult(res *tg.MessagesForumTopics) topicsResult {
	ent := entitiesOf(res.Users, res.Chats)
	out := topicsResult{Count: res.Count, Topics: make([]topicItem, 0, len(res.Topics))}
	for _, tc := range res.Topics {
		switch t := tc.(type) {
		case *tg.ForumTopic:
			item := topicItem{
				ID:             t.ID,
				Title:          t.Title,
				Date:           t.Date,
				TopMessage:     t.TopMessage,
				Unread:         t.UnreadCount,
				UnreadMentions: t.UnreadMentionsCount,
				Closed:         t.Closed,
				Pinned:         t.Pinned,
				Hidden:         t.Hidden,
				My:             t.My,
				IconColor:      t.IconColor,
			}
			if emoji, ok := t.GetIconEmojiID(); ok {
				item.IconEmojiID = strconv.FormatInt(emoji, 10)
			}
			if t.FromID != nil {
				ref := describePeer(t.FromID, ent)
				item.From = &ref
			}
			out.Topics = append(out.Topics, item)
		case *tg.ForumTopicDeleted:
			out.Topics = append(out.Topics, topicItem{ID: t.ID, Deleted: true})
		}
	}
	return out
}

// nextTopicOffsets derives the pagination offsets for the page after res, from
// its last topic. It reports false when there is nothing left to page from.
//
// The offset date has to match whatever order the server used, which it reports
// in order_by_create_date: the topic's own date when topics come ordered by
// creation, and otherwise the date of the message that top_message points at.
// Using the wrong one moves the page boundary and duplicates or skips topics.
func nextTopicOffsets(res *tg.MessagesForumTopics) (date, id, topic int, ok bool) {
	var last *tg.ForumTopic
	for _, tc := range res.Topics {
		if t, isTopic := tc.(*tg.ForumTopic); isTopic {
			last = t
		}
	}
	if last == nil {
		return 0, 0, 0, false
	}
	date = last.Date
	if !res.OrderByCreateDate {
		for _, mc := range res.Messages {
			if m, isMsg := mc.(*tg.Message); isMsg && m.ID == last.TopMessage {
				date = m.Date
				break
			}
		}
	}
	return date, last.TopMessage, last.ID, true
}

// countedTopics reports how many of the collected topics the server includes in
// the total it reports as count: the General topic exists implicitly in every
// forum, so it is listed but never counted, and a deleted entry carries nothing
// to count. Undercounting here only costs one extra request; overcounting would
// end the walk early, so borderline entries are left out.
func countedTopics(topics []topicItem) int {
	n := 0
	for _, t := range topics {
		if t.ID != generalTopicID && !t.Deleted {
			n++
		}
	}
	return n
}

// listTopics pages through a forum's topics until limit is reached or the forum
// runs out. A limit of zero or less fetches every topic.
//
// Note what does *not* end the walk: a page shorter than the one asked for.
// Telegram does not document its per-page ceiling, so a short page is
// indistinguishable from the end of the list, and treating it as the end
// silently truncates the result on any forum whose pages the server caps below
// the requested limit. The walk ends on an empty page instead, and count — the
// server's own total for the query — is what saves the extra round trip.
func listTopics(
	ctx context.Context,
	api *tg.Client,
	peer tg.InputPeerClass,
	query string,
	limit int,
) (topicsResult, error) {
	unlimited := limit <= 0

	var (
		out                          topicsResult
		offsetDate, offsetID, offset int
	)
	for unlimited || len(out.Topics) < limit {
		page := topicsPageLimit
		if !unlimited {
			page = min(limit-len(out.Topics), topicsPageLimit)
		}
		req := &tg.MessagesGetForumTopicsRequest{
			Peer:        peer,
			OffsetDate:  offsetDate,
			OffsetID:    offsetID,
			OffsetTopic: offset,
			Limit:       page,
		}
		if query != "" {
			req.SetQ(query)
		}
		res, err := api.MessagesGetForumTopics(ctx, req)
		if err != nil {
			return topicsResult{}, errors.Wrap(err, "messages.getForumTopics")
		}
		got := newTopicsResult(res)
		out.Count = got.Count
		out.Topics = append(out.Topics, got.Topics...)

		// An empty page is the end of the list.
		if len(got.Topics) == 0 {
			break
		}
		// The server has handed over everything it counts for this query.
		if out.Count > 0 && countedTopics(out.Topics) >= out.Count {
			break
		}
		nextDate, nextID, nextTopic, ok := nextTopicOffsets(res)
		// Offsets that do not move would loop forever.
		if !ok || (nextDate == offsetDate && nextID == offsetID && nextTopic == offset) {
			break
		}
		offsetDate, offsetID, offset = nextDate, nextID, nextTopic
	}
	if !unlimited && len(out.Topics) > limit {
		out.Topics = out.Topics[:limit]
	}
	return out, nil
}

// topicIDFromUpdates finds the id of a topic just created, which Telegram
// reports as the service message announcing it.
func topicIDFromUpdates(u tg.UpdatesClass) (int, bool) {
	var updates []tg.UpdateClass
	switch v := u.(type) {
	case *tg.Updates:
		updates = v.Updates
	case *tg.UpdatesCombined:
		updates = v.Updates
	default:
		return 0, false
	}
	for _, upd := range updates {
		var msg tg.MessageClass
		switch v := upd.(type) {
		case *tg.UpdateNewChannelMessage:
			msg = v.Message
		case *tg.UpdateNewMessage:
			msg = v.Message
		default:
			continue
		}
		svc, ok := msg.(*tg.MessageService)
		if !ok {
			continue
		}
		if _, ok := svc.Action.(*tg.MessageActionTopicCreate); ok {
			return svc.ID, true
		}
	}
	return 0, false
}

func (a *app) newTopicsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "topics",
		Short:   "Manage forum topics",
		GroupID: groupChats,
		Long: `List, inspect and manage the forum topics of a supergroup.

Topics are addressed by their numeric id, which every subcommand here reports.
To read or post inside a topic, pass that id to the messaging commands as
--topic (` + "`tg send --peer @forum --topic 42 …`" + `, ` + "`tg history @forum --topic 42`" + `).
The always-present "General" topic has id 1.`,
	}
	cmd.AddCommand(
		a.newTopicsListCmd(),
		a.newTopicsGetCmd(),
		a.newTopicsCreateCmd(),
		a.newTopicsEditCmd(),
		a.newTopicsCloseCmd(),
		a.newTopicsReopenCmd(),
		a.newTopicsHideCmd(),
		a.newTopicsUnhideCmd(),
		a.newTopicsPinCmd(),
		a.newTopicsUnpinCmd(),
		a.newTopicsReorderCmd(),
		a.newTopicsDeleteCmd(),
		a.newTopicsEnableCmd(),
		a.newTopicsDisableCmd(),
	)
	return cmd
}

// withForum resolves a forum peer and runs f with its input peer. It rejects
// peers that are not topic-enabled supergroups up front, so the commands fail
// with an actionable message rather than a raw RPC error.
func (a *app) withForum(
	ctx context.Context,
	api *tg.Client,
	arg string,
	f func(peer tg.InputPeerClass) error,
) error {
	m, err := a.manager(api)
	if err != nil {
		return err
	}
	ch, err := forumChannel(ctx, m, arg)
	if err != nil {
		return err
	}
	return f(ch.InputPeer())
}

// topicIDArg parses a topic id argument.
func topicIDArg(arg string) (int, error) {
	id, err := strconv.Atoi(arg)
	if err != nil {
		return 0, errors.Wrapf(err, "invalid topic id %q", arg)
	}
	return id, nil
}

func (a *app) newTopicsListCmd() *cobra.Command {
	var (
		limit int
		query string
		all   bool
	)

	cmd := &cobra.Command{
		Use:               cmdList + " <peer>",
		Short:             "List forum topics",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: peerArgCompletion,
		Long: `List a forum's topics newest-first, with unread counts and
pinned/closed/hidden flags. Use the reported id with --topic on the messaging
commands.

JSON output carries "count", the server's total for the query, which is what to
compare against when checking for more. It does not include the General topic
(id 1): that one exists implicitly in every forum, so it is listed but not
counted.`,
		Example: `  tg topics list @myforum
  tg topics list @myforum --all --output json
  tg topics list @myforum --limit 200
  tg topics list @myforum --query "release"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && cmd.Flags().Changed("limit") {
				return errors.New("--all and --limit are mutually exclusive")
			}
			if all {
				limit = 0
			}
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				return a.withForum(ctx, api, args[0], func(peer tg.InputPeerClass) error {
					res, err := listTopics(ctx, api, peer, query, limit)
					if err != nil {
						return err
					}
					return a.printer.Emit(res)
				})
			})
		},
	}

	fs := cmd.Flags()
	fs.IntVarP(&limit, "limit", "n", topicsPageLimit, "maximum number of topics to list")
	fs.BoolVar(&all, "all", false, "list every topic, however many there are")
	fs.StringVarP(&query, "query", "q", "", "only topics whose title matches this query")

	return cmd
}

func (a *app) newTopicsGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "get <peer> <topic-id>...",
		Short:             "Show specific forum topics by id",
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: peerArgCompletion,
		Example:           "  tg topics get @myforum 42\n  tg topics get @myforum 1 42 43 --output json",
		RunE: func(cmd *cobra.Command, args []string) error {
			ids, err := parseIDs(args[1:])
			if err != nil {
				return err
			}
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				return a.withForum(ctx, api, args[0], func(peer tg.InputPeerClass) error {
					res, err := api.MessagesGetForumTopicsByID(ctx, &tg.MessagesGetForumTopicsByIDRequest{
						Peer:   peer,
						Topics: ids,
					})
					if err != nil {
						return errors.Wrap(err, "messages.getForumTopicsByID")
					}
					return a.printer.Emit(newTopicsResult(res))
				})
			})
		},
	}
	return cmd
}

func (a *app) newTopicsCreateCmd() *cobra.Command {
	var (
		iconColor int
		iconEmoji string
	)

	cmd := &cobra.Command{
		Use:               "create <peer> <title>",
		Short:             "Create a forum topic",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: peerArgCompletion,
		Long: `Create a forum topic and report its id, which is what --topic takes on
the messaging commands.`,
		Example: `  tg topics create @myforum "Deploys"
  ID=$(tg topics create @myforum "Deploys" -o json | jq -r .data.topic_id)
  tg send --peer @myforum --topic "$ID" "first"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			rndID, err := randomID()
			if err != nil {
				return err
			}
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				return a.withForum(ctx, api, args[0], func(peer tg.InputPeerClass) error {
					req := &tg.MessagesCreateForumTopicRequest{
						Peer:     peer,
						Title:    args[1],
						RandomID: rndID,
					}
					if iconColor != 0 {
						req.SetIconColor(iconColor)
					}
					if iconEmoji != "" {
						emoji, err := strconv.ParseInt(iconEmoji, 10, 64)
						if err != nil {
							return errors.Wrapf(err, "invalid --icon-emoji %q", iconEmoji)
						}
						req.SetIconEmojiID(emoji)
					}
					upd, err := api.MessagesCreateForumTopic(ctx, req)
					if err != nil {
						return errors.Wrap(err, "messages.createForumTopic")
					}
					id, ok := topicIDFromUpdates(upd)
					if !ok {
						return errors.New("topic created, but its id was missing from the response; run `tg topics list` to find it")
					}
					return a.printer.Emit(createdTopicResult{TopicID: id, Title: args[1]})
				})
			})
		},
	}

	fs := cmd.Flags()
	fs.IntVar(&iconColor, "icon-color", 0,
		"fallback icon color (RGB int), one of 7322096, 16766590, 13338331, 9367192, 16749490, 16478047")
	fs.StringVar(&iconEmoji, "icon-emoji", "", "custom emoji id to use as the topic icon")

	return cmd
}

// editTopic applies a messages.editForumTopic change to one topic.
func editTopic(
	ctx context.Context,
	api *tg.Client,
	peer tg.InputPeerClass,
	topicID int,
	apply func(*tg.MessagesEditForumTopicRequest),
) error {
	req := &tg.MessagesEditForumTopicRequest{Peer: peer, TopicID: topicID}
	apply(req)
	if _, err := api.MessagesEditForumTopic(ctx, req); err != nil {
		return errors.Wrap(err, "messages.editForumTopic")
	}
	return nil
}

func (a *app) newTopicsEditCmd() *cobra.Command {
	var (
		title     string
		iconEmoji string
	)

	cmd := &cobra.Command{
		Use:               "edit <peer> <topic-id>",
		Aliases:           []string{"rename"},
		Short:             "Rename a forum topic or change its icon",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: peerArgCompletion,
		Example:           "  tg topics edit @myforum 42 --title \"Deploys (v2)\"",
		RunE: func(cmd *cobra.Command, args []string) error {
			if title == "" && iconEmoji == "" {
				return errors.New("nothing to change: pass --title and/or --icon-emoji")
			}
			topicID, err := topicIDArg(args[1])
			if err != nil {
				return err
			}
			var emoji int64
			if iconEmoji != "" {
				if emoji, err = strconv.ParseInt(iconEmoji, 10, 64); err != nil {
					return errors.Wrapf(err, "invalid --icon-emoji %q", iconEmoji)
				}
			}
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				return a.withForum(ctx, api, args[0], func(peer tg.InputPeerClass) error {
					if err := editTopic(ctx, api, peer, topicID, func(req *tg.MessagesEditForumTopicRequest) {
						if title != "" {
							req.SetTitle(title)
						}
						if iconEmoji != "" {
							req.SetIconEmojiID(emoji)
						}
					}); err != nil {
						return err
					}
					return a.printer.Emit(okResult{OK: true})
				})
			})
		},
	}

	fs := cmd.Flags()
	fs.StringVar(&title, "title", "", "new topic title")
	fs.StringVar(&iconEmoji, "icon-emoji", "", "custom emoji id to use as the topic icon")

	return cmd
}

// topicAction acts on one topic of a forum.
type topicAction func(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, topicID int) error

// topicActionCmd builds a "<verb> <peer> <topic-id>" command around a single
// action, acknowledging it with the usual ok result.
func (a *app) topicActionCmd(use, short string, act topicAction) *cobra.Command {
	return &cobra.Command{
		Use:               use + " <peer> <topic-id>",
		Short:             short,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: peerArgCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			topicID, err := topicIDArg(args[1])
			if err != nil {
				return err
			}
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				return a.withForum(ctx, api, args[0], func(peer tg.InputPeerClass) error {
					if err := act(ctx, api, peer, topicID); err != nil {
						return err
					}
					return a.printer.Emit(okResult{OK: true})
				})
			})
		},
	}
}

// topicToggleCmd builds a command that flips one boolean of a forum topic.
func (a *app) topicToggleCmd(use, short string, set func(*tg.MessagesEditForumTopicRequest)) *cobra.Command {
	return a.topicActionCmd(use, short,
		func(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, topicID int) error {
			return editTopic(ctx, api, peer, topicID, set)
		})
}

func (a *app) newTopicsCloseCmd() *cobra.Command {
	return a.topicToggleCmd("close", "Close a forum topic (no new messages)",
		func(req *tg.MessagesEditForumTopicRequest) { req.SetClosed(true) })
}

func (a *app) newTopicsReopenCmd() *cobra.Command {
	return a.topicToggleCmd("reopen", "Reopen a closed forum topic",
		func(req *tg.MessagesEditForumTopicRequest) { req.SetClosed(false) })
}

// topicHideCmd builds the hide/unhide commands, which only apply to General.
func (a *app) topicHideCmd(use, short string, hidden bool) *cobra.Command {
	return &cobra.Command{
		Use:               use + " <peer>",
		Short:             short,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: peerArgCompletion,
		Long:              "Only the General topic (id 1) can be hidden.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				return a.withForum(ctx, api, args[0], func(peer tg.InputPeerClass) error {
					if err := editTopic(ctx, api, peer, generalTopicID,
						func(req *tg.MessagesEditForumTopicRequest) { req.SetHidden(hidden) }); err != nil {
						return err
					}
					return a.printer.Emit(okResult{OK: true})
				})
			})
		},
	}
}

func (a *app) newTopicsHideCmd() *cobra.Command {
	return a.topicHideCmd("hide", "Hide the General topic", true)
}

func (a *app) newTopicsUnhideCmd() *cobra.Command {
	return a.topicHideCmd("unhide", "Show the General topic again", false)
}

// topicPinCmd builds the pin/unpin commands.
func (a *app) topicPinCmd(use, short string, pinned bool) *cobra.Command {
	return a.topicActionCmd(use, short,
		func(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, topicID int) error {
			if _, err := api.MessagesUpdatePinnedForumTopic(ctx, &tg.MessagesUpdatePinnedForumTopicRequest{
				Peer:    peer,
				TopicID: topicID,
				Pinned:  pinned,
			}); err != nil {
				return errors.Wrap(err, "messages.updatePinnedForumTopic")
			}
			return nil
		})
}

func (a *app) newTopicsPinCmd() *cobra.Command {
	return a.topicPinCmd("pin", "Pin a forum topic", true)
}

func (a *app) newTopicsUnpinCmd() *cobra.Command {
	return a.topicPinCmd("unpin", "Unpin a forum topic", false)
}

func (a *app) newTopicsReorderCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:               "reorder <peer> <topic-id>...",
		Short:             "Set the order of pinned forum topics",
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: peerArgCompletion,
		Example:           "  tg topics reorder @myforum 42 7 13",
		RunE: func(cmd *cobra.Command, args []string) error {
			order, err := parseIDs(args[1:])
			if err != nil {
				return err
			}
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				return a.withForum(ctx, api, args[0], func(peer tg.InputPeerClass) error {
					if _, err := api.MessagesReorderPinnedForumTopics(ctx, &tg.MessagesReorderPinnedForumTopicsRequest{
						Peer:  peer,
						Order: order,
						Force: force,
					}); err != nil {
						return errors.Wrap(err, "messages.reorderPinnedForumTopics")
					}
					return a.printer.Emit(okResult{OK: true})
				})
			})
		},
	}

	cmd.Flags().BoolVar(&force, "force", false,
		"unpin any pinned topic that is not listed in the new order")

	return cmd
}

func (a *app) newTopicsDeleteCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:               "delete <peer> <topic-id>",
		Aliases:           []string{"del", "rm"},
		Short:             "Delete a forum topic and all of its messages",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: peerArgCompletion,
		Long:              "Delete a forum topic together with its whole history. Destructive: requires --yes.",
		Example:           "  tg topics delete @myforum 42 --yes",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("refusing to delete a topic without --yes")
			}
			topicID, err := topicIDArg(args[1])
			if err != nil {
				return err
			}
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				return a.withForum(ctx, api, args[0], func(peer tg.InputPeerClass) error {
					if err := deleteHistory(ctx, api, peer, topicID, false, false); err != nil {
						return err
					}
					return a.printer.Emit(deletedResult{Count: 1})
				})
			})
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "confirm deletion")

	return cmd
}

// topicsToggleForumCmd builds the enable/disable commands.
func (a *app) topicsToggleForumCmd(use, short string, enabled bool) *cobra.Command {
	var tabs bool

	cmd := &cobra.Command{
		Use:               use + " <peer>",
		Short:             short,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: peerArgCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				m, err := a.manager(api)
				if err != nil {
					return err
				}
				// Not withForum: enabling is precisely the case where the peer
				// is not a forum yet.
				ch, err := asInputChannel(ctx, m, args[0])
				if err != nil {
					return err
				}
				if _, err := api.ChannelsToggleForum(ctx, &tg.ChannelsToggleForumRequest{
					Channel: ch,
					Enabled: enabled,
					Tabs:    tabs,
				}); err != nil {
					return errors.Wrap(err, "channels.toggleForum")
				}
				return a.printer.Emit(okResult{OK: true})
			})
		},
	}

	if enabled {
		cmd.Flags().BoolVar(&tabs, "tabs", false, "display topics as tabs rather than as a list")
	}

	return cmd
}

func (a *app) newTopicsEnableCmd() *cobra.Command {
	return a.topicsToggleForumCmd("enable", "Enable forum topics in a supergroup", true)
}

func (a *app) newTopicsDisableCmd() *cobra.Command {
	return a.topicsToggleForumCmd("disable", "Disable forum topics in a supergroup", false)
}
