# agent-whatsapp-actions-api-daemon

**A local HTTP API that makes a personal WhatsApp account scriptable, with the brakes built in.**

WhatsApp's official Cloud API is a business product. It requires a WhatsApp Business Account and a registered number, it is gated behind business verification, and it has no access to a personal account's chats, contacts or groups. If you want an agent, a cron job or a shell script to act on *your own* WhatsApp, the official API is not a worse option. It is not an option.

The protocol that can do it is the WhatsApp Web multi-device protocol, which [whatsmeow](https://github.com/tulir/whatsmeow) speaks by pairing as a linked device, the same way WhatsApp Web on a laptop does. But a raw protocol library hands a caller unguarded control of a real account, which is exactly the wrong thing to give an autonomous process.

This is whatsmeow with an envelope around it.

## Read this before you run it

Linking any unofficial client to a personal account **violates WhatsApp's Terms of Service**. Meta says so directly in [About unofficial apps](https://faq.whatsapp.com/1217634902127718) and [Unauthorized use of automated or bulk messaging](https://faq.whatsapp.com/5957850900902049).

The ban risk is documented, not hypothetical. whatsmeow's own issue tracker carries [#807](https://github.com/tulir/whatsmeow/issues/807) and [#810](https://github.com/tulir/whatsmeow/issues/810), which report account warnings and bans including at low volume. Risk scales with automation volume, which is precisely what a daemon like this enables.

Nothing here reduces that risk, and no gate in this repo changes the ToS position. The gates exist to stop *your own* automation from doing something irreversible. Run it on an account you can afford to lose.

## Why a daemon and not a library call

whatsmeow holds a paired session backed by a SQLite store. Pairing is interactive and the session must persist, so it cannot be spun up per command. Something has to stay alive and own the socket.

Once something is alive, the question is how callers reach it. A Go library is only callable from a compiled Go program. An HTTP endpoint is callable from anything: an agent, `curl`, a cron entry, a Python script, another service. That is the whole design decision.

## Safety model

The interesting part is not the WhatsApp surface. It is what stands between a caller and a real account.

- **Bound to `127.0.0.1` only.** Never a network interface. There is no TLS because there is no network hop to protect.
- **`API_TOKEN` required to boot.** The process refuses to start without one, rather than starting open. Every route checks `X-API-Token`.
- **Three independent gates:** `ACTIONS_ENABLED`, `DRY_RUN_DEFAULT`, `LIVE_MUTATIONS_ENABLED`. The daemon can be running and healthy while still refusing to mutate anything.
- **Dry-run preflight** on destructive operations, so the default answer to a malformed request is "no".
- **Idempotency keys**, so a retried request is not a second send.
- **Every action gets an ID**, queryable at `GET /actions/{id}` after the fact. What happened is a lookup, not an inference from logs.

## Endpoints

```
Base   http://127.0.0.1:8788        (override with WA_ACTIOND_URL)
Auth   X-API-Token: <token>          every route, no exceptions
Body   application/json
```

| | |
| --- | --- |
| Read | `GET /health` `GET /chats` `GET /messages?chat=` `GET /groups` `GET /group` `GET /group/invite` `GET /blocklist` `GET /actions/{id}` |
| Send | `POST /actions/send` `send_image` `send_video` `send_audio` `send_document` |
| Chat | `archive` `unarchive` `mute` `unmute` `mark_read` `mark_unread` `pin` `unpin` `star` `unstar` `delete_chat` `block` `unblock` |
| Message | `edit_message` `delete_message` `react` |
| Group | `group_create` `group_join` `group_leave` `group_add` `group_remove` `group_promote` `group_demote` `group_name` `group_topic` `group_photo` |
| Presence | `typing` `stop_typing` `presence` `follow` |

## The `wa` CLI

`wa` is a zsh wrapper so a caller never handles the token. It reads it from `.env` and shells out to the daemon.

```bash
wa health
wa list [limit]
wa messages <chat> [limit]
wa send <chat> <text...> [--mention <jid|number>]...
wa send-image <chat> <file.jpg> [caption...]
wa send-audio <chat> <file.mp3> [seconds]
```

Destructive operations dry-run unless `--go` is passed.

## Two things that cost real time to learn

**A real @mention needs both halves.** The literal `@<number>` in the message text *and* that person's JID in the mentions array. Text alone renders as plain characters and notifies nobody. Groups are LID-addressed, so a participant has both a `<lid>@lid` and a phone JID; pass both and whichever matches resolves, the other is inert.

**A live process is not a live connection.** launchd's `KeepAlive` restarts a crashed process, but it is blind to a process that is running fine while its WhatsApp websocket is dead: an initial-dial failure, a half-open socket, a keepalive timeout. `watchdog.sh` polls `/health` and kickstarts only when the daemon is *paired but not connected*, with a five-minute cooldown so a network outage does not cause a restart storm. A genuine logout clears the paired flag, so the watchdog never fights an intentional logged-out state.

That second one generalizes past WhatsApp: **a green process is not evidence of a working connection.**

## Run it

```bash
go build -o actiond .
cp .env.example .env          # set API_TOKEN
./start.sh                    # or install the launchd job
wa health
```

First run prints a QR code to pair as a linked device.

## Layout

```
main.go              config, HTTP server, signal handling
actions.go           routes, auth, gates, dry-run, action ledger
client.go            whatsmeow client, send paths, media, mentions
store.go             SQLite session and action persistence
wa                   zsh CLI
watchdog.sh          health-gated restarter
```

## Limitations

- Session is per-account and interactive to establish; it cannot be provisioned headlessly.
- One account should have one automation client. Two linked clients on the same account produce duplicate event handling and conflicting appstate writes.
- No TLS, by design. Localhost only.
- Media is downloaded to `DATA_DIR/media` and never garbage-collected.
