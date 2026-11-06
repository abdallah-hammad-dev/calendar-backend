# calendar-backend — notes for Claude

Server side of the Obsidian extension calendar-tasks: Davis (the CalDAV server), its test bench, and the Go backend
(external accounts mirrored into Davis, RSVP relay, accounts page), started at B1. **Spec: `docs/SPEC.md` (read it
first)**; tested facts: `bench/TESTS.md`. The extension lives in `../calendar-tasks` (public repo
https://github.com/abdallah-hammad-dev/calendar-tasks); its `docs/SPEC.md` §4.5–4.6 and §7.1 define what the
backend must write. This repo is public: my VPS, domain and addresses live in `deploy/.env` (git-ignored), never in
tracked files.

## State (2026-10-28)

- Davis tested with the iPhone and the extension; runs locally only (bench) so far.
- B1 done (2026-10-22): Go skeleton, mirrors an ICS feed into Davis, sees my answers.
- B2–B5 coded (2026-10-23): Google and Microsoft adapters, the RSVP relay, the accounts page (`run` serves it).
  Tested on API answers shaped from the references (adapters) and against the bench Davis (relay, page); **not yet
  on real accounts**: spec §12 lists what to check first.
- **Deployed 2026-10-29** (spec §8) on my VPS (hosts nothing else; its SSH host and domain: `deploy/.env`):
  `cal.` / `sync.` / `accounts.<domain>`, Let's Encrypt certificates. Each push on `main`
  deploys the backend (CI, ~2 min). The VPS's sudo is sudo-rs (Ubuntu 26.04).
- Next: the iPhone, Obsidian and LiveSync on the real addresses; the Google Cloud project and the Entra app
  (`docs/comptes.md`) into `deploy/.env` then `./deploy.sh`; test on real accounts and fix. Not done: backups,
  moving my data from the bench. Open questions in spec §12 are the user's to answer.
- B8, the Claude connector (spec §10), coded 2026-11-04: an MCP endpoint in the backend (`/mcp`, its own OAuth)
  writes tasks / events into Davis and notes into the vault livesync-bridge mirrors on the VPS. **Deployed the same
  day**: the bridge on my LiveSync database (after a copy), the mirror checked against the PC's vault. Next: the
  connector added on claude.ai, then tried from the phone's app.

## New machine

1. Clone both repos side by side (`~/organisation/calendar-backend`, `~/organisation/calendar-tasks`).
2. `cp davis/davis.env.example davis/davis.env`, fill `APP_SECRET` (`openssl rand -hex 16`) and `ADMIN_PASSWORD`.
3. `davis/run.sh` (rootless Podman, `127.0.0.1:9000`), then create the bench users in the dashboard
   (`http://127.0.0.1:9000/dashboard`): `abd` (abd@bench.test) and `orga` (organisateur@bench.test), password `x`.
4. The iPhone needs HTTPS, so it uses the deployed server, not the bench (`bench/TESTS.md`).

## Commands

- `start.sh` — starts Davis and Radicale after a reboot (neither is a service).
- `bench/push-fixtures.sh [--reset]` / `bench/check.sh` — push and read test items over HTTP.
- `go test ./...` — unit tests. `DAVIS_TEST_URL=http://127.0.0.1:9000/dav/ go test ./...` adds the tests against
  the bench Davis (as `orga`, throwaway collections). Run them after touching `internal/davis/` or `internal/mirror/`.
- `go run ./cmd/calendar-backend [-env backend.env] run|once|accounts|add-feed|remove` — settings in `backend.env`
  (copy `backend.env.example`), database in `data/` (both git-ignored). `run` serves the accounts page on `LISTEN`
  (login: the Davis user).
- `DAVIS_TEST_URL=… go test ./internal/web/` runs the whole sign-in (fake Google) against the bench Davis.
- `DAVIS_TEST_URL=… go test ./internal/connector/` runs the connector's tools against the bench Davis (as `orga`,
  throwaway collections "IT Général / IT École …", a temporary vault). ~40 s: each Davis request costs ~200 ms.
- `podman build -t calendar-backend .` — the image (static binary on distroless, zones embedded).
- `./deploy.sh [--new-key]` — installs or updates the VPS from `deploy/` (settings and secrets in `deploy/.env`,
  git-ignored), checks before and after, sets CI's secrets. A push on `main` deploys the backend
  (`.github/workflows/deploy.yml`: tests, image to ghcr.io, `deploy/update` on the VPS).
- Go module lookups fail with Go's own DNS resolver on this machine: `GODEBUG=netdns=cgo go get …`.

## Layout

- `cmd/calendar-backend` — CLI: settings, discovery of my principal (its email = my `ATTENDEE`), commands.
- `internal/davis` — hand-written CalDAV client for Davis: discovery, `MKCALENDAR`, conditional `PUT`/`DELETE`,
  `sync-collection` (with sabre's unknown-token quirk, as in the extension). Always `Accept-Encoding: identity`.
  HTTP calls (Davis and feeds) give up after 2 minutes: one server that never answers stopped every pass.
- `internal/ics` — go-ical's decoder, our encoder (folding at 75 octets, sorted properties: two encodings of the
  same content compare equal), `Localize` (UTC → TZID, generated `VTIMEZONE`s from Go's zone database). Never let
  Go's map order reach the output: `Localize` read properties in map order and gave the same event another text from
  pass to pass (2026-10-28, `TestLocalizeIsStable`).
- `internal/adapter` — the `Adapter` interface (`List(token)`, `RSVP`; `Refused` = an answer that won't go
  through, said as `failed:<reason>`), `icsfeed` (read-only ICS feed), `google` (Calendar API, sync tokens, a series
  read whole by `iCalUID`), `microsoft` (Graph `calendarView/delta` over a range, series read again from their
  master, the token is JSON with the Graph id → UID map; `windowszones.go` is generated from CLDR).
- `internal/oauth` — sign-in with PKCE, tokens refreshed and saved back (Microsoft rotates refresh tokens),
  `SignInAgain(err)`.
- `internal/accounts` — each kind: its adapter (signed in from the account's config), its address after sign-in.
  `providers.go`: the Google / Microsoft OAuth clients, from the environment (wins) or from the accounts page
  (`settings` row `oauth_client_<kind>`), swapped at run time under a lock.
- `internal/htmltext` — HTML descriptions to plain text, keeping link targets.
- `internal/web` — the accounts page (templates embedded, French): login, add / sign in again / remove, sync now,
  the trash and its Restaurer, the OAuth clients (`/settings/{kind}`), the public `/privacy` page Google asks for.
  `authorize.go`: the connector's OAuth server (metadata, registration limited to Claude's callbacks and loopback,
  consent behind the Davis login, tokens, revoke from the page) in front of `/mcp`.
- `internal/connector` — the MCP server (official Go SDK, stateless streamable HTTP): `calendar.go` (collections,
  events, free slots, rsvp; `calendar-query` with `expand`), `tasks.go` (VTODOs as the extension writes them, the
  note name it will give), `notes.go` (the vault tools, task / event notes' Davis-owned properties guarded),
  `ical.go`. Logs: tool, outcome, ms, UIDs; never contents, inputs, outputs or note paths. Nothing waits for an
  Obsidian to open (2026-11-05): a task's text before its note exists, a repeating task moved on (its note's
  `completions` too, `vault.EditProps`), a task deleted with its note.
- `internal/vault` — the vault on disk (`VAULT_DIR`: `notes/` = the bridge's folder, `history/`, `trash/`,
  `tmp/`), all through `os.Root`: atomic writes, versions, history (30 days) and trash, link rewriting on a move
  (`links.go`), frontmatter (`meta.go`). A file in a new folder, and every moved file, is written again 3 s later:
  the bridge's watcher misses them otherwise (tested).
- `internal/mirror` — `Base` (spec §5.2's rules common to every account), `Merge` (keeps what clients add),
  `trash.go` (spec §5.4: dates a client deleted, written back on every copy until restored; `Engine.Restore`), the
  `Engine` (pass = each collection accounts write into read once, then each source; answers in flight; one copy
  of a meeting two accounts are invited to). Accounts write into a collection the user chose, shared with their
  own items: the engine touches only the hrefs in `items`.
- `internal/store` — SQLite (modernc, no cgo): `accounts` (with their collection), `collections` (sync tokens),
  `items` (source data, Davis href/etag, partstat written, answer in flight, what the relay sent and wrote, the
  trash: `trashed`, `removed` dates), `settings` (the page's session key and `session_gen`). Migrations by
  `PRAGMA user_version` (`schema.sql` … `schema5.sql`; 4 and 5: the connector's OAuth clients and tokens, used
  refresh tokens kept to see a second use); deployed: never edit one, add the next.
- `deploy/` — the VPS's `compose.yaml` (Caddy, Davis, backend, CouchDB, livesync-bridge under the profile `vault`,
  the one-shot `owner`), `Caddyfile`, `couchdb.ini` (LiveSync's settings; owned by CouchDB's uid 5984 on the VPS, or
  its entrypoint fails), `update` (CI's only command). The backend runs as the bridge's uid 1993 (both write the
  vault); the bridge's config (`/opt/calendar/bridge/`) is made by `deploy.sh`.

## Hard rules

- Secrets never committed: `davis/davis.env`, `davis/data/`, OAuth tokens, `.env` files (see `.gitignore`).
- Davis's `INVITE_FROM_ADDRESS` stays empty: the backend relays answers, Davis must not send a second one.
- Davis's `LOG_FILE_PATH` stays `/dev/null`: its error log copies the request's headers, the password included.
- Errors that may carry a feed URL go through `scrub` (`internal/adapter/icsfeed`): Go's `*url.Error` repeats the
  whole URL, secret included.
- Feeds are fetched by https only and from public addresses only (`icsfeed.defaultHTTP`, `CheckURL`): never give a
  production `Feed` another client or `Files`. Sources never write the backend's `X-CALTASKS-*` nor alarms (`Base`).
- The page's login counts tries before comparing the password (`web/limit.go`); sessions end with the password or
  `session_gen`. Connector tools: anything but adding is `destructive`; events written by others carry `fromOthers`.
- Write times with `TZID` + `VTIMEZONE`, never UTC (iOS shows a second "(GMT)" time).
- Compare only `PARTSTAT` on my `ATTENDEE` (iOS rewrites the other parameters).
- Bench tests write only to bench users (`abd`, `orga`) and delete what they create; the user's real collections on
  the bench ("Général") are used by their iPhone: a test invitation there notifies the phone.
- Record every new fact about Davis, iOS or a provider in `bench/TESTS.md`, and carry decisions into `docs/SPEC.md`.
- Never log event contents (summaries, descriptions, attendees): UIDs and hrefs only. Never log tokens. The
  connector logs no note paths either (they are note titles), nor its inputs and outputs.
- LiveSync's passphrase and recovery code live in `deploy/.env` and the bridge's config on the VPS only, never in
  the VPS's `.env`, a log or the chat.
- `X-CALTASKS-RSVP-STATE` is the store's (`items.rsvp_state`), written by `SetRSVPState` in every build; `Merge`
  doesn't carry it from the Davis copy (a client's new answer clears it).
- The Graph adapter follows next / delta links only under its base URL: the OAuth client adds the account's token
  to any URL it calls.
- Public repo: no personal host, domain, address or secret in tracked files (`deploy.sh` asks for them and keeps
  them in `deploy/.env`).
- Commit and push only when the user says so; a push on `main` also deploys the backend.
- CI's key on the VPS runs `deploy/update` and nothing else: keep that script checking its input, never give the
  `deploy` user more. Davis's dashboard and CouchDB's `/_utils` stay off the internet.
