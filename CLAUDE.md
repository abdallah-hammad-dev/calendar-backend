# calendar-backend — notes for Claude

Server side of the Obsidian extension calendar-tasks: Davis (the CalDAV server), its test bench, and the Go backend
(external accounts mirrored into Davis, RSVP relay, accounts page), started at B1. **Spec: `docs/SPEC.md` (read it
first)**; tested facts: `bench/TESTS.md`. The extension lives in `../calendar-tasks` (public repo
https://github.com/abdallah-hammad-dev/calendar-tasks); its `docs/SPEC.md` §4.5–4.6 and §7.1 define what the
backend must write. This repo is private.

## State (2026-10-23)

- Davis tested with the iPhone and the extension; runs locally only (bench), no deployment yet.
- B1 done (2026-10-22): Go skeleton, mirrors an ICS feed into Davis, sees my answers.
- B2–B5 coded (2026-10-23): Google and Microsoft adapters, the RSVP relay, the accounts page (`run` serves it).
  Tested on API answers shaped from the references (adapters) and against the bench Davis (relay, page); **not yet
  on real accounts**: spec §11 lists what to check first.
- Next (the user's order): deploy Davis, the backend and CouchDB (vault sync) on the VPS with `deploy.sh` and
  CI/CD (spec §8), then test on real accounts and fix. Needs the user's domain (at Cloudflare), the Google Cloud
  project and the Entra app (`docs/comptes.md`). Open questions in spec §11 are the user's to answer.

## New machine

1. Clone both repos side by side (`~/organisation/calendar-backend`, `~/organisation/calendar-tasks`).
2. `cp davis/davis.env.example davis/davis.env`, fill `APP_SECRET` (`openssl rand -hex 16`) and `ADMIN_PASSWORD`.
3. `davis/run.sh` (rootless Podman, `127.0.0.1:9000`), then create the bench users in the dashboard
   (`http://127.0.0.1:9000/dashboard`): `abd` (abd@bench.test) and `orga` (organisateur@bench.test), password `x`.
4. The iPhone reaches the bench through Tailscale Serve (HTTPS required by iOS), see `bench/TESTS.md`.

## Commands

- `start.sh` — starts Davis and Radicale after a reboot (neither is a service).
- `bench/push-fixtures.sh [--reset]` / `bench/check.sh` — push and read test items over HTTP.
- `go test ./...` — unit tests. `DAVIS_TEST_URL=http://127.0.0.1:9000/dav/ go test ./...` adds the tests against
  the bench Davis (as `orga`, throwaway collections). Run them after touching `internal/davis/` or `internal/mirror/`.
- `go run ./cmd/calendar-backend [-env backend.env] run|once|accounts|add-feed|remove` — settings in `backend.env`
  (copy `backend.env.example`), database in `data/` (both git-ignored). `run` serves the accounts page on `LISTEN`
  (login: the Davis user).
- `DAVIS_TEST_URL=… go test ./internal/web/` runs the whole sign-in (fake Google) against the bench Davis.
- `podman build -t calendar-backend .` — the image (static binary on distroless, zones embedded).
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
- `internal/htmltext` — HTML descriptions to plain text, keeping link targets.
- `internal/web` — the accounts page (templates embedded, French): login, add / sign in again / remove, sync now.
- `internal/mirror` — `Base` (spec §5.2's rules common to every account), `Merge` (keeps what clients add), the
  `Engine` (pass = each collection accounts write into read once, then each source; answers in flight; one copy
  of a meeting two accounts are invited to). Accounts write into a collection the user chose, shared with their
  own items: the engine touches only the hrefs in `items`.
- `internal/store` — SQLite (modernc, no cgo): `accounts` (with their collection), `collections` (sync tokens),
  `items` (source data, Davis href/etag, partstat written, answer in flight, what the relay sent and wrote),
  `settings` (the page's session key). Migrations by `PRAGMA user_version` (`schema.sql`, `schema2.sql`); never
  edit one once deployed (not deployed yet).

## Hard rules

- Secrets never committed: `davis/davis.env`, `davis/data/`, OAuth tokens, `.env` files (see `.gitignore`).
- Davis's `INVITE_FROM_ADDRESS` stays empty: the backend relays answers, Davis must not send a second one.
- Davis's `LOG_FILE_PATH` stays `/dev/null`: its error log copies the request's headers, the password included.
- Errors that may carry a feed URL go through `scrub` (`internal/adapter/icsfeed`): Go's `*url.Error` repeats the
  whole URL, secret included.
- Write times with `TZID` + `VTIMEZONE`, never UTC (iOS shows a second "(GMT)" time).
- Compare only `PARTSTAT` on my `ATTENDEE` (iOS rewrites the other parameters).
- Bench tests write only to bench users (`abd`, `orga`) and delete what they create; the user's real collections on
  the bench ("Général") are used by their iPhone: a test invitation there notifies the phone.
- Record every new fact about Davis, iOS or a provider in `bench/TESTS.md`, and carry decisions into `docs/SPEC.md`.
- Never log event contents (summaries, descriptions, attendees): UIDs and hrefs only. Never log tokens.
- `X-CALTASKS-RSVP-STATE` is the store's (`items.rsvp_state`), written by `SetRSVPState` in every build; `Merge`
  doesn't carry it from the Davis copy (a client's new answer clears it).
- The Graph adapter follows next / delta links only under its base URL: the OAuth client adds the account's token
  to any URL it calls.
- Commit and push only when the user says so.
