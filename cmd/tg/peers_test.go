package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-faster/errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"

	"github.com/gotd/cli/internal/peercache"
)

// newTestManager builds a peerManager backed by a temp peer cache and the mock
// API, so id: resolution can be exercised without a network. The mock expects
// no calls; tests that need API responses build their own manager.
func newTestManager(t *testing.T) *peerManager {
	t.Helper()
	store, err := peercache.Open(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	api, _ := newTestAPI(t)
	return &peerManager{Manager: peers.Options{Storage: store}.Build(api), store: store}
}

func TestIsIDArg(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"id:42", true},
		{"  id:42  ", true},
		{"@durov", false},
		{"42", false},
		{"identity", false},
	} {
		if got := isIDArg(c.in); got != c.want {
			t.Errorf("isIDArg(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// selfAliases are the peer strings that target Saved Messages.
var selfAliases = []string{"", "me", "self", "ME"}

func TestResolvePeerSelf(t *testing.T) {
	m := newTestManager(t)
	for _, in := range selfAliases {
		p, err := resolvePeer(context.Background(), m, in)
		if err != nil {
			t.Fatalf("resolvePeer(%q): %v", in, err)
		}
		if _, ok := p.(*tg.InputPeerSelf); !ok {
			t.Errorf("resolvePeer(%q) = %T, want InputPeerSelf", in, p)
		}
	}
}

func TestResolvePeerIDNotCached(t *testing.T) {
	m := newTestManager(t)
	if _, err := resolvePeer(context.Background(), m, "id:777"); err == nil {
		t.Fatal("expected error for uncached id")
	}
}

func TestResolvePeerIDInvalid(t *testing.T) {
	m := newTestManager(t)
	if _, err := resolvePeer(context.Background(), m, "id:notanumber"); err == nil {
		t.Fatal("expected error for non-numeric id")
	}
}

func TestResolvePeerIDUser(t *testing.T) {
	ctx := context.Background()
	store, err := peercache.Open(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, peers.Key{Prefix: "users_", ID: 42}, peers.Value{AccessHash: 999}); err != nil {
		t.Fatal(err)
	}

	api, mock := newTestAPI(t)
	mock.Expect().ThenResult(&tg.UserClassVector{Elems: []tg.UserClass{
		&tg.User{ID: 42, AccessHash: 999, Username: "durov"},
	}})
	m := &peerManager{Manager: peers.Options{Storage: store}.Build(api), store: store}

	p, err := resolvePeer(ctx, m, "id:42")
	if err != nil {
		t.Fatal(err)
	}
	u, ok := p.(*tg.InputPeerUser)
	if !ok {
		t.Fatalf("resolvePeer = %T, want InputPeerUser", p)
	}
	if u.UserID != 42 || u.AccessHash != 999 {
		t.Errorf("input peer = %+v, want id 42 / hash 999", u)
	}
}

// TestBuilderForID guards the send path: "id:" must be resolved eagerly via the
// cache, not handed to sender.Resolve (which rejects the ":" as a bad domain).
func TestBuilderForID(t *testing.T) {
	ctx := context.Background()
	store, err := peercache.Open(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, peers.Key{Prefix: "users_", ID: 42}, peers.Value{AccessHash: 999}); err != nil {
		t.Fatal(err)
	}

	api, mock := newTestAPI(t)
	mock.Expect().ThenResult(&tg.UserClassVector{Elems: []tg.UserClass{
		&tg.User{ID: 42, AccessHash: 999, Username: "durov"},
	}})
	m := &peerManager{Manager: peers.Options{Storage: store}.Build(api), store: store}
	sender := message.NewSender(api).WithResolver(peerResolver{pm: m})

	if _, err := builderFor(ctx, m, sender, "id:42"); err != nil {
		t.Fatalf("builderFor(id:42): %v", err)
	}
}

func TestBuilderForSelf(t *testing.T) {
	store, err := peercache.Open(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	api, _ := newTestAPI(t)
	m := &peerManager{Manager: peers.Options{Storage: store}.Build(api), store: store}
	sender := message.NewSender(api).WithResolver(peerResolver{pm: m})
	for _, in := range selfAliases {
		if _, err := builderFor(context.Background(), m, sender, in); err != nil {
			t.Errorf("builderFor(%q): %v", in, err)
		}
	}
}

// TestResolvePeerArgID covers the typed-peer path shared by every command that
// must branch on user/chat/channel (chat get/full, set-title, set-photo,
// invite, leave, subscribe, ban/unban, contacts delete, watch): "id:<n>" must
// be resolved from the peer cache for all three kinds. The user case is also
// the ban/unban regression — there the "id:" argument is the second one.
func TestResolvePeerArgID(t *testing.T) {
	const (
		id   = 42
		hash = 999
	)
	for _, tc := range []struct {
		name  string
		key   peers.Key
		check func(peers.Peer) bool
	}{
		{"user", peers.Key{Prefix: "users_", ID: id}, func(p peers.Peer) bool { _, ok := p.(peers.User); return ok }},
		{"chat", peers.Key{Prefix: "chats_", ID: id}, func(p peers.Peer) bool { _, ok := p.(peers.Chat); return ok }},
		{"channel", peers.Key{Prefix: "channel_", ID: id}, func(p peers.Peer) bool { _, ok := p.(peers.Channel); return ok }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := peercache.Open(filepath.Join(t.TempDir(), "peers.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(ctx, tc.key, peers.Value{AccessHash: hash}); err != nil {
				t.Fatal(err)
			}
			api := newFuncAPI(t, func(req bin.Encoder) (bin.Encoder, error) {
				switch req.(type) {
				case *tg.UsersGetUsersRequest:
					return &tg.UserClassVector{Elems: []tg.UserClass{
						&tg.User{ID: id, AccessHash: hash, Username: "durov"},
					}}, nil
				case *tg.MessagesGetChatsRequest:
					return &tg.MessagesChats{Chats: []tg.ChatClass{
						&tg.Chat{ID: id, Title: "group", Photo: &tg.ChatPhotoEmpty{}},
					}}, nil
				case *tg.ChannelsGetChannelsRequest:
					return &tg.MessagesChats{Chats: []tg.ChatClass{
						&tg.Channel{ID: id, AccessHash: hash, Title: "channel", Photo: &tg.ChatPhotoEmpty{}},
					}}, nil
				default:
					return nil, errors.Errorf("unexpected request %T", req)
				}
			})
			m := &peerManager{Manager: peers.Options{Storage: store}.Build(api), store: store}

			p, err := resolvePeerArg(ctx, m, "id:42")
			if err != nil {
				t.Fatalf("resolvePeerArg(id:42): %v", err)
			}
			if !tc.check(p) {
				t.Errorf("resolvePeerArg = %T, want %s", p, tc.name)
			}
			if p.ID() != id {
				t.Errorf("id = %d, want %d", p.ID(), id)
			}
		})
	}
}

// TestPeerArgsUseIDAwareResolvers is a guard against the "id:" prefix quietly
// dropping out of a command again. Peer arguments must be resolved through
// resolvePeer or resolvePeerArg, which check isIDArg first; calling
// peers.Manager.Resolve directly hands "id:42" to the library's domain
// validator, which rejects it with a misleading `validate domain: unexpected :
// at 2`. Those two helpers are the only place the direct call belongs — they
// reach it only on the non-"id:" branch.
func TestPeerArgsUseIDAwareResolvers(t *testing.T) {
	allowed := map[string]bool{"resolvePeer": true, "resolvePeerArg": true}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || allowed[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				// peers.Manager.Resolve is the only two-argument Resolve
				// method in play; message.Sender.Resolve takes one.
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Resolve" {
					return true
				}
				t.Errorf("%s: %s resolves a peer with a direct .Resolve call; use resolvePeerArg (or resolvePeer) so \"id:<n>\" works",
					fset.Position(call.Pos()), fn.Name.Name)
				return true
			})
		}
	}
}
