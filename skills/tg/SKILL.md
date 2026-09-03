---
name: tg
description: Drive a Telegram account from the shell with the `tg` CLI — send/read/search messages, manage chats, contacts, groups and channels, upload/download media, and stream messages in real time. Use whenever the user asks to do something on Telegram (message someone, check chats, download a file, watch for messages) from this machine.
---

# tg — Telegram from the command line

`tg` is a single static Go binary (this repo: `gotd/cli`, built on `gotd/td`) for
driving a Telegram **personal account** or a **bot** non-interactively. It is
designed for agents: structured JSON output, stable peer syntax, explicit safety
gates.

## Before anything: check it's set up

`tg` needs a one-time config + login. Verify first, and stop to ask the user if
not authenticated — login is interactive and must be done by a human.

```bash
tg whoami -o json   # authenticated → {"schema":1,"data":{...}}; else errors
```

If it errors with "no config … run `tg init` first" or a not-authorized error:

```bash
tg init             # writes ~/.config/gotd/gotd.cli.yaml (release binaries embed app creds)
tg login            # QR by default: the USER scans from Settings → Devices
```

Release binaries embed app credentials; source builds need them supplied
(`tg init --app-id … --app-hash …`, from https://my.telegram.org). If `tg init`
errors with "no app credentials", that's the cause — ask the user.

Do not run `tg login` autonomously expecting it to succeed headless — QR/phone
login needs the user. `tg init` is safe to run yourself.

## Golden rules for agent use

- **Always pass `-o json`** (`--output json`) for anything you need to parse. It
  emits `{"schema":1,"data":…}` on **stdout**; logs/prompts/progress go to
  **stderr**. Pipe stdout to `jq`.
- **Peers** (`--peer`/`<peer>`) accept: `me` or `self` (Saved Messages),
  `@username`, a phone number, a `t.me/…` link, or **`id:<n>`** — the `id`
  field from `chats list -o json`, which is how you address a chat that has no
  username. Keep the `id:` prefix: a bare number is parsed as a username and
  fails with `contact not found`. A chat title is never a peer (`tg history
  "Some Group"` cannot work — peers are usernames, not names). Resolved
  access-hashes are cached locally, so reuse the same form.
- **`id:` reads the local access-hash cache**, so populate it once per machine
  with `tg chats list` (or `tg contacts list`); it persists between runs.
- **Forum topics** are a second coordinate on top of the peer: pass
  `--topic <id>` to any messaging command, or hand it a topic link
  (`t.me/<forum>/<topic>/<message>`) as the peer. Ids come from
  `tg topics list <peer> -o json`. The always-present "General" topic is id 1.
- **Destructive commands require `--yes`**: `delete`, `delete-history`,
  `unpin-all`, and similar. Never add `--yes` without the user asking for the
  destructive action — confirm intent first.
- **Read before write.** Prefer `history`/`search`/`chats list` to understand
  state before sending, deleting, or editing.
- **When a peer fails to resolve, read the quoted string in the error** — it is
  exactly what `tg` tried to resolve, so it shows whether the argument arrived
  as intended. Three failures that look alike but are not:
  `contact not found` (wrong peer form — most often a bare number that needed
  `id:`, or a chat title); `peer id <n> not in cache` (run `tg chats list`,
  then retry); `is not a supergroup` / `has no forum topics` (a property of
  that chat, not of addressing). The first two are one command away from
  working — retry with the fix rather than reporting the chat as unreachable or
  hunting for workarounds, and never repeat the same failing command unchanged.

## Common recipes

Note the shapes: `send`/`upload` take the peer via the `--peer` flag (default
self); most others take `<peer>` (and a `<message-id>`) as **positional** args.

```bash
# Message yourself (Saved Messages) and a peer
tg send "note to self"
tg send --peer @durov "hello"

# List chats, extract usernames
tg chats list -o json | jq -r '.data.chats[].peer.username // empty'

# A chat with no username: cache it once, then address it by numeric id
tg chats list >/dev/null
tg history id:4483395565 --limit 20 -o json

# Read recent history / search (peer is positional)
tg history @gotd_test --limit 20 -o json
tg search @gotd_test "invoice" -o json     # within a chat
tg search --global "invoice" -o json       # across all chats

# Reply / edit / react: <peer> <message-id> [text|emoji], ids come from history
tg reply @x 12345 "on it"
tg edit  @x 12345 "fixed typo"
tg react @x 12345 👍

# Media (upload uses --peer; download is positional + --out)
tg upload --peer me ./report.pdf
tg upload --peer me ./clip.mp4 --type video
tg download @x 12345 --out ./downloads/

# Realtime
tg watch @x -o json                 # stream new messages as JSON lines (long-running)
tg wait  @x --timeout 30s -o json   # block for the next message, then exit

# Triage
tg read @x                          # mark read
tg mute @x
tg archive @x
```

Forum topics (supergroups with topics enabled — `tg chats list` flags them
`forum`). Everything is addressed by the numeric topic id:

```bash
# Discover topics and their ids (a forum without a username takes id:<n>)
tg topics list @myforum -o json | jq -r '.data.topics[] | "\(.id)\t\(.title)"'
tg topics list id:4483395565 -o json
tg topics list @myforum --all -o json     # every topic, however many

# Read and post inside one topic
tg history @myforum --topic 42 -o json
tg send --peer @myforum --topic 42 "status update"
tg reply @myforum 1337 "on it" --topic 42
tg upload --peer @myforum --topic 42 ./build.log
tg search @myforum --topic 42 "deploy" -o json
tg watch @myforum --topic 42 -o json

# A topic link works as the peer, no --topic needed
tg history https://t.me/myforum/42/1337

# Create one and post into it
ID=$(tg topics create @myforum "Deploys" -o json | jq -r .data.topic_id)
tg send --peer @myforum --topic "$ID" "first"

# Manage
tg topics close @myforum 42          # also: reopen, pin, unpin, edit --title, get
```

`history`, `search` and `watch` on a forum **without** `--topic` return every
topic mixed together; each message carries `topic_id` in JSON so you can tell
them apart.

`tg topics list` defaults to 100 topics; pass `--all` for the rest. Its JSON
`count` is the server's total for the query — compare it against the number of
returned topics to detect more. It excludes the General topic (id 1), which
exists implicitly in every forum and is listed but never counted.

Destructive (only when the user explicitly asks):

```bash
tg delete @x 12345 --yes            # <peer> <message-id>...
tg delete-history @x --yes
tg delete-history @myforum --topic 42 --yes   # clear one topic
tg topics delete @myforum 42 --yes            # delete the topic itself
```

## Multiple accounts

```bash
tg accounts                          # list configured accounts + auth status
tg accounts add work --app-id … --app-hash …
tg login --account work              # or: tg login --account <new-label> (auto-created)
tg chats list --account work -o json
tg watch --account all -o json       # fan out across all accounts, labeled
```

`tg login --account <label>` creates the account entry on the fly (reusing the
build-time app credentials), so `tg accounts add` is only needed for custom app
credentials, a bot token, or a per-account proxy.

## Bots

```bash
tg init --token <bot-token>
tg whoami --bot
# pass --bot to commands that support a bot session
```

## Global flags

| Flag | Meaning |
| --- | --- |
| `-o, --output text\|json` | output format (use `json` for parsing) |
| `-a, --account <label>` | pick an account, or `all` to fan out |
| `-c, --config <path>` | config file to use |
| `--proxy <url>` | `socks5://…` or `tg://proxy?…` (MTProxy) |

## Discovery

The command surface is large and self-documenting. When unsure, ask `tg` rather
than guessing flags:

```bash
tg --help              # grouped command list
tg <command> --help    # flags + examples for one command
```

Notes:
- `--test` is set at config time (`tg init --test`), not per command.
- Errors print to stderr prefixed with `tg:`; a non-zero exit means failure.
