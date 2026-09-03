# tg — Telegram CLI for humans and agents

`tg` is a single static Go binary for driving a Telegram **personal account** (or a
bot) from the command line or an AI agent: message yourself, list and triage chats,
read history, send/forward/edit/delete messages, upload and download files, manage
groups and channels, and stream new messages in real time.

Built on [`gotd/td`](https://github.com/gotd/td).

## Installation

Download a prebuilt binary or package (`.deb`/`.rpm`/`.apk`) for your platform from
the [latest release](https://github.com/gotd/cli/releases/latest).

## Quick start

```console
$ tg init                                          # write config (release binaries embed app credentials)
$ tg login                                         # QR login — scan once from a logged-in device
$ tg whoami                                         # confirm you're authenticated
$ tg send "Hello world"                             # message yourself (Saved Messages)
$ tg send --peer @gotd_test "Hello world"           # message a peer
```

App credentials are required. The prebuilt [release](https://github.com/gotd/cli/releases/latest)
binaries embed them at build time and present the session as a desktop client, so
`tg init` just works. If you build from source, provide your own from
<https://my.telegram.org>: `tg init --app-id APP_ID --app-hash APP_HASH`.

The config is written to the `gotd` subdirectory of your config dir, e.g.
`~/.config/gotd/gotd.cli.yaml`. The session persists too, so subsequent commands
run headless. On macOS the session is stored in the login Keychain by default; an
existing file session is migrated into the Keychain automatically on first use.
Set `keychain: false` in the config to keep it in a file alongside the config
instead (useful for headless macOS). Other platforms always use a file.

### Login options

```console
$ tg login                     # QR (default): scan from Settings → Devices → Link Desktop Device
$ tg login --phone +1234567890  # phone-code login (use --phone= to be prompted for the number)
$ TG_PASSWORD=secret tg login   # supply the 2FA cloud password non-interactively
```

Bot login still works too: provide a token via `tg init --token <bot-token>` and pass
`--bot` to commands that support it (e.g. `tg whoami --bot`).

## Agent-friendly by design

- **Structured output:** `--output json` (`-o json`) on any command emits a stable
  envelope `{"schema":1,"data":…}` on stdout. Logs, prompts and progress go to stderr.
- **Consistent peers:** every `<peer>` accepts `me`/`self`, `@username`, a phone number,
  a `t.me/…` link, or `id:<n>` — the numeric `id` from `tg chats list -o json`, which is
  how you address a chat that has no username. Resolved access-hashes are cached
  locally, so `id:` works for any peer the cache has already seen.
- **Safety:** destructive actions (`delete`, `delete-history`, `unpin-all`) require `--yes`.
- **Shell completion:** `tg completion bash|zsh|fish|powershell`, with dynamic completion
  for peers, accounts, output format and enum flags.

```console
$ tg chats list --output json | jq '.data.chats[].peer.username'
$ tg history @durov --limit 20 -o json
$ tg history id:2201861038 --limit 20     # a chat with no username, by numeric id
$ tg resolve id:2201861038                # what does that id refer to?
```

### Agent skill

This repo ships an installable [Claude Code](https://claude.com/claude-code) skill
(`skills/tg`) that teaches an agent how to drive Telegram with `tg` — setup checks,
the JSON/peers conventions, the `--yes` safety gate, and common recipes. Install it
with [`skills`](https://www.npmjs.com/package/skills):

```console
$ npx skills add https://github.com/gotd/cli --all
# or a single skill:
$ npx skills add https://github.com/gotd/cli --skill tg
```

## What you can do

Run `tg --help` for the full, grouped command list, or `tg <command> --help` for details.

- **Messaging:** `send`, `reply`, `edit`, `delete`, `delete-history`, `forward`,
  `history`, `search` (`--global`), `read`, `pin`/`unpin`/`unpin-all`/`pinned`,
  `react`/`unreact`/`reactions`, `draft`/`drafts`, `schedule`, `poll`, `link`, `context`.
  Anything that reads or writes messages also takes `--topic <id>` to work inside
  a single forum topic.
- **Media:** `upload` (with `--type` video/audio/voice/gif/sticker), `album`, `download`,
  `stickers`.
- **Chats & contacts:** `chats list`, `chat get`/`full`, `mute`/`unmute`,
  `archive`/`unarchive`, `resolve`, `search-public`, `subscribe`, `contacts` (list,
  search, add, delete, block/unblock, blocked, import).
- **Groups & channels:** `create-group`, `create-channel`, `invite`, `leave`,
  `participants`/`admins`/`banned`, `promote`/`demote`, `ban`/`unban`, `slow-mode`,
  `set-title`/`set-about`/`set-photo`, `invite-link`/`join-link`,
  `topics` (list/get/create/edit/close/reopen/hide/pin/reorder/delete/enable),
  `recent-actions`.
- **Profile & folders:** `profile` (update, set/delete photo, status), `folders`
  (list, create, add-chat/remove-chat, delete, reorder).
- **Realtime:** `watch [peer]` streams new messages as JSON lines; `wait [peer]` blocks
  until the next message (with `--timeout`). Both take `--topic` to follow one forum topic.

## Global flags

| Flag | Description |
| --- | --- |
| `-o, --output text\|json` | output format (default `text`) |
| `-a, --account <label>` | select an account, or `all` to fan out across accounts |
| `-c, --config <path>` | config file to use |
| `--proxy <url>` | `socks5://…` or `tg://proxy?…` (MTProxy) |
| `--test` | connect to the Telegram test server (persisted by `tg init --test`) |

## Forum topics

Supergroups with topics enabled are a two-coordinate space: a peer plus a topic
id. `tg topics` manages the topics themselves, and every messaging command takes
`--topic <id>` to work inside one.

```console
$ tg topics list @myforum                  # ids, titles, unread counts, flags
$ tg topics list id:4483395565             # a forum with no username, by numeric id
$ tg topics list @myforum --all            # every topic, however many
$ tg topics create @myforum "Deploys"      # prints the new topic's id
$ tg send --peer @myforum --topic 42 "status update"
$ tg history @myforum --topic 42
$ tg history https://t.me/myforum/42/1337  # a topic link works as the peer
$ tg topics close @myforum 42
```

The General topic is id 1. Without `--topic`, reads span every topic; each
message carries `topic_id` in JSON output. `tg chats list` marks forums with a
`forum` flag. In `topics list` JSON, `count` is the server's total for the query
and excludes the General topic, which is listed but never counted.

## Multiple accounts

Add named accounts and select them per command:

```console
$ tg accounts add work --app-id … --app-hash …
$ tg login --account work
$ tg accounts                       # list accounts + auth status
$ tg chats list --account work
$ tg watch --account all            # stream every account concurrently, labeled
```

## Using the test server

Initialize a config against the Telegram **test server**, then log in with a test
number. Test mode only switches the DC list — it uses the same app credentials as
production (so the same app-id/app-hash requirement applies):

```console
$ tg init --test                # persists test: true
$ tg login --phone 9996621234   # test number for DC 2; the code is 22222
$ tg whoami
```
