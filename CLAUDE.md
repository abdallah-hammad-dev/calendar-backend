# calendar-backend — notes for Claude

Server side of the Obsidian extension calendar-tasks: Davis (the CalDAV server), its test bench, and the Go backend
(external accounts mirrored into Davis, RSVP relay, accounts page) still to write. **Spec: `docs/SPEC.md` (read it
first)**; tested facts: `bench/TESTS.md`. The extension lives in `../calendar-tasks` (public repo
https://github.com/abdallah-hammad-dev/calendar-tasks); its `docs/SPEC.md` §4.5–4.6 and §7.1 define what the
backend must write. This repo is private.

## State (2026-10-22)

- Davis tested with the iPhone and the extension; runs locally only (bench). No Go code yet, no deployment yet.
- Next: milestones in spec §10, starting with B0 (deploy Davis on the VPS) and B1 (Go skeleton). Open questions in
  spec §11 are the user's to answer; ask them when a milestone needs one.

## New machine

1. Clone both repos side by side (`~/organisation/calendar-backend`, `~/organisation/calendar-tasks`).
2. `cp davis/davis.env.example davis/davis.env`, fill `APP_SECRET` (`openssl rand -hex 16`) and `ADMIN_PASSWORD`.
3. `davis/run.sh` (rootless Podman, `127.0.0.1:9000`), then create the bench users in the dashboard
   (`http://127.0.0.1:9000/dashboard`): `abd` (abd@bench.test) and `orga` (organisateur@bench.test), password `x`.
4. The iPhone reaches the bench through Tailscale Serve (HTTPS required by iOS), see `bench/TESTS.md`.

## Commands

- `start.sh` — starts Davis and Radicale after a reboot (neither is a service).
- `bench/push-fixtures.sh [--reset]` / `bench/check.sh` — push and read test items over HTTP.

## Hard rules

- Secrets never committed: `davis/davis.env`, `davis/data/`, OAuth tokens, `.env` files (see `.gitignore`).
- Davis's `INVITE_FROM_ADDRESS` stays empty: the backend relays answers, Davis must not send a second one.
- Write times with `TZID` + `VTIMEZONE`, never UTC (iOS shows a second "(GMT)" time).
- Compare only `PARTSTAT` on my `ATTENDEE` (iOS rewrites the other parameters).
- Bench tests write only to bench users (`abd`, `orga`) and delete what they create; the user's real collections on
  the bench ("Général") are used by their iPhone: a test invitation there notifies the phone.
- Record every new fact about Davis, iOS or a provider in `bench/TESTS.md`, and carry decisions into `docs/SPEC.md`.
- Commit and push only when the user says so.
