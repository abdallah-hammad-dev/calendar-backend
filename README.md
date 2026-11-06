# Calendar & Tasks backend

The server side of [Calendar & Tasks for Obsidian](https://github.com/abdallah-hammad-dev/calendar-tasks). It runs
on your own server, one person per server, and puts all your calendars in one place:

- a **CalDAV server** ([Davis](https://github.com/tchapi/davis)) holds your events and tasks. Your phone and
  Obsidian both use it;
- your **other accounts** (Google, Microsoft, published calendars) are copied into it, and your **answers to
  invitations** go back out from the address that was invited;
- your **Obsidian vault** syncs through CouchDB ([Self-hosted LiveSync](https://github.com/vrtmrz/obsidian-livesync));
- a **Claude connector** lets Claude's chat (web, desktop, phone app) read and edit your notes, tasks and events.

> [!WARNING]
> **Vibe-coded. Use at your own risk.**
> I built this by describing what I wanted to Claude Code and testing the result, not by writing the code myself.
> It's a personal tool that runs my own calendar and vault, shared as is. Don't expect a deep, low-level
> understanding of the code from me, quick fixes, or support. It holds your calendar accounts' sign-ins and, with
> the connector, the key to your vault. Read [Security](#security) before you put it on the internet.

## What it does

- **One calendar server for your phone and Obsidian.** Davis speaks CalDAV with scheduling: an invitation shows
  *Accept / Maybe / Decline* and a notification on the iPhone, and in the Calendar & Tasks calendar view.
- **Your other accounts, copied in.** Each account's calendar is copied into the collection you choose for it
  ("Work", "School"…), next to your own events and tasks, and kept up to date. Supported:
  - **Google** (Calendar API);
  - **Microsoft**, personal, work or school (Microsoft Graph);
  - **any published calendar** (an ICS link), read-only.
- **Answers go back from the right address.** Accept an invitation on your phone or in Obsidian and the backend
  sends the answer through the account that was invited, so the organizer sees it come from that address.
- **One copy per meeting.** A meeting you're invited to on two accounts shows once.
- **Deleting is yours.** Delete a copied event on your phone or in Obsidian and it stays gone, even though it's
  still on its account. It goes to a trash on the accounts page, where you can restore it. The organizer isn't
  told.
- **Accounts page.** A small web page, behind your CalDAV login: add, reconnect or remove an account, sync now,
  the trash, the Google and Microsoft sign-in settings, and the Claude clients you connected. (The page is in
  French for now.)
- **Vault sync.** CouchDB for the Self-hosted LiveSync plugin, end-to-end encrypted, on mobile too.
- **Claude connector.** An MCP server at `/mcp` with its own sign-in. See [Claude connector](#claude-connector).
- **One-command deployment.** `deploy.sh` installs or updates the whole server, and each push on `main` deploys
  the backend.

## How it works

```
Google, Microsoft, ICS links ⇄ backend ⇄ Davis (CalDAV) ⇄ iPhone (Calendar, Reminders), Obsidian (Calendar & Tasks)
                                  │
     Claude's chat ─── /mcp ──────┤
                                  └── vault files ⇄ livesync-bridge ⇄ CouchDB ⇄ Obsidian (LiveSync)
```

- **The Obsidian plugin never calls the backend.** Everything goes through CalDAV data in Davis. Without the
  backend, everything works except your other accounts. The plugin stays a plain CalDAV client that works with any
  server.
- **It reads calendars, not mail.** Gmail and Outlook already turn the invitations they receive into calendar
  events, with their changes and cancellations. No email is parsed.
- **Copies are marked.** Each copied event carries `X-CALTASKS-*` properties (its account, the invited address,
  where your answer stands) that the plugin reads. They're described in the plugin's
  [`docs/SPEC.md`](https://github.com/abdallah-hammad-dev/calendar-tasks/blob/main/docs/SPEC.md) §4.6.
- **Your address on each invitation is your Davis address,** which is what makes the iPhone show its buttons. The
  address that was really invited is kept beside it, for the answer.
- **Times are written with their time zone,** never in UTC (the iPhone shows a second "(GMT)" time otherwise).

The full design, with the facts tested on Davis and iOS that shaped it, is in [`docs/SPEC.md`](docs/SPEC.md).

## Requirements

**To run it on a server:**

- A VPS running Ubuntu (`deploy.sh` installs Docker from Docker's repository).
- A domain with three names pointing at the VPS: `cal.` (Davis), `sync.` (CouchDB) and `accounts.` (the accounts
  page and the connector). At Cloudflare, keep them **DNS only**: its proxy weakens ETags when it compresses, and
  CalDAV writes then fail.
- On your computer: `ssh`, `scp`, `ssh-keygen`, `dig`, `curl`, `openssl`, and the GitHub CLI (`gh`) signed in.
- For Google or Microsoft accounts: a Google Cloud project and/or a Microsoft Entra app registration (see
  [Connecting Google and Microsoft](#connecting-google-and-microsoft)).

**To develop:** Go 1.27, and Podman to run Davis locally.

## Deploy

```sh
./deploy.sh
```

It asks for what it needs and stores it in `deploy/.env`, git-ignored: your GitHub repository (this one or your
fork, for CI), the VPS's SSH host, your domain, an email for Let's Encrypt, your Davis user, email and password. It
makes the other secrets itself. **Keep a copy of `deploy/.env` somewhere safe:** without it, the next run makes new
secrets. Then it:

1. checks everything before touching the VPS: the values, SSH and sudo, DNS pointing at the VPS;
2. installs Docker and starts Caddy (HTTPS), Davis, CouchDB and the backend (`deploy/compose.yaml`);
3. waits while you create your Davis user in Davis's dashboard (through an SSH tunnel: the dashboard is never
   public);
4. checks everything again: real certificates, a Davis login, no compression on `/dav/`, CouchDB's settings for
   LiveSync, the backend's health;
5. sets up CI: a push on `main` runs the tests, builds the image, and the VPS takes it. CI's SSH key can only run
   that update, and the VPS rolls back if the new version doesn't answer.

It's safe to run again after any change to `deploy/` or `deploy/.env`. Its prompts are in French.

**Then point your devices at it:**

| What | Address |
| --- | --- |
| iPhone (Settings → Calendar → Calendar Accounts → Add Account → Other → CalDAV), Calendar & Tasks in Obsidian | `https://cal.<domain>/dav/` |
| Self-hosted LiveSync in Obsidian | `https://sync.<domain>` |
| Accounts page | `https://accounts.<domain>` |
| Claude connector | `https://accounts.<domain>/mcp` |

Don't also add your Google or Microsoft accounts on the phone, or everything shows twice.

## Connecting Google and Microsoft

Done once per server, before adding accounts. Each person creates their own clients: nothing is shared.

- **Redirect URI** for both: `https://accounts.<domain>/oauth/callback` (the accounts page shows it). For local
  tests, add `http://localhost:8080/oauth/callback` too.
- **Google**: a Google Cloud project with the Calendar API on, an "External" consent screen with the scopes
  `openid`, `userinfo.email` and `calendar.events`, a privacy policy at `https://accounts.<domain>/privacy` (the
  page serves one), and an OAuth client of type *Web application*. **Publish the app** ("In production"): an app
  left in "Testing" loses its sign-ins after 7 days. Unverified is fine for personal use: Google shows a warning
  at sign-in.
- **Microsoft**: an Entra app registration for "accounts in any organizational directory and personal Microsoft
  accounts", a client secret (copy its *Value*), and the delegated Graph permissions `Calendars.ReadWrite`,
  `offline_access`, `openid` and `email`. Leave the tenant as `common`.
- **School or work accounts** often need an administrator to approve an unknown app. Try registering the app in
  that organization's own directory (and set the tenant to its ID). If that's blocked too, publish the calendar as
  an ICS link from Outlook on the web and add it as a published calendar (read-only: your answers won't go out).

Enter the client IDs and secrets on the accounts page (*Réglages*), or in `deploy/.env` (`GOOGLE_CLIENT_ID`,
`GOOGLE_CLIENT_SECRET`, `MICROSOFT_CLIENT_ID`, `MICROSOFT_CLIENT_SECRET`, `MICROSOFT_TENANT`) and run `./deploy.sh`
again. The environment wins over the page. Step-by-step instructions (in French): [`docs/comptes.md`](docs/comptes.md).

## Claude connector

Add `https://accounts.<domain>/mcp` as a custom connector on claude.ai (or in Claude Desktop). Claude's phone app
then uses it too. You sign in with your Davis user.

- **Calendar and tasks**: list your collections and events, find free time, create, change or delete events (with
  a video meeting link), answer invitations, list, create, change and delete tasks, and plan a work session for a
  task. Tasks and events are written straight into Davis, the way the Obsidian plugin writes them.
- **Notes** (needs the vault, below): list, search, read, write, edit, move and delete notes, and restore an
  earlier version. Moving a note updates the links to it. Every change keeps the version it replaced for 30 days,
  and deleted notes go to a trash.
- **The vault on the server.** [livesync-bridge](https://github.com/vrtmrz/livesync-bridge) keeps a decrypted copy
  of your vault on the VPS, synced both ways with LiveSync's database. `deploy.sh` asks once for the LiveSync
  database, passphrase and recovery code, and tries the bridge on a copy first. Without it, the connector has your
  calendar and tasks only.

## Security

- **Your login is your Davis user** (the one on your phone). It opens the accounts page and the connector's
  consent. Failed logins are limited per address and overall. Changing the password, or *Se déconnecter partout*,
  ends every session.
- **Secrets stay on the server.** Account sign-ins live in the backend's SQLite database. They never go into the
  vault or git, and they're never logged.
- **Logs keep identifiers only:** no event contents, no tokens, no note paths, and none of the connector's inputs
  or outputs. Davis's own log is turned off, because it copies each failed request with its password.
- **Only Caddy is on the internet** (ports 80 and 443). Davis's dashboard and CouchDB's admin UI aren't published.
- **The connector only registers Claude's callbacks** and local addresses. You can disconnect each client from the
  accounts page. A refresh token used twice signs its client out.
- **Others' text is marked.** Anyone can send you an invitation, so events written by someone else are marked
  `fromOthers`, and Claude is told that their text is data, not instructions. Every tool that changes or deletes
  something says so, so Claude can ask you first. This narrows prompt injection; it doesn't stop it.
- **With the vault on the server, whoever controls the server can read your vault.** Keep the VPS for this alone
  and keep it updated (`deploy.sh` turns on Docker's automatic updates).

## Known limitations

- One person per server.
- Not all of it has been tried on real Google and Microsoft accounts yet. What's left to check is listed in
  [`docs/SPEC.md`](docs/SPEC.md) §12.
- Accounts other than Google, Microsoft and published calendars (iCloud, Fastmail, Nextcloud…) can't be added yet.
- Sending your own invitations from one of your accounts isn't built yet (spec §9).
- No database backups yet.
- The accounts page, `deploy.sh`'s prompts, `docs/comptes.md` and the test notes in `bench/` are in French.

## Develop

Run Davis locally (rootless Podman, on `127.0.0.1:9000`):

```sh
cp davis/davis.env.example davis/davis.env   # fill APP_SECRET (openssl rand -hex 16) and ADMIN_PASSWORD
davis/run.sh
```

Create a user in its dashboard (`http://127.0.0.1:9000/dashboard`), with an email. Then the backend:

```sh
cp backend.env.example backend.env             # your Davis user and password, and the other settings
go run ./cmd/calendar-backend run              # accounts page on http://localhost:8080, and syncs continuously
go run ./cmd/calendar-backend once             # one pass
go run ./cmd/calendar-backend accounts         # accounts, their last pass and last error
go run ./cmd/calendar-backend add-feed …       # add a published calendar (-name, -address, -url, -collection)
go run ./cmd/calendar-backend remove …         # remove an account (-id, -delete-events or -keep-events)
```

Tests:

```sh
go test ./...                                                  # unit tests
DAVIS_TEST_URL=http://127.0.0.1:9000/dav/ go test ./...        # plus the tests against your local Davis
```

The tests against Davis use a user named `orga` and throwaway collections, and delete what they create.

Build the image with `podman build -t calendar-backend .` (a static binary on distroless).

| Folder | What's in it |
| --- | --- |
| `cmd/calendar-backend` | The command line |
| `internal/davis` | CalDAV client for Davis |
| `internal/adapter` | One adapter per kind of account: `google`, `microsoft`, `icsfeed` |
| `internal/mirror` | Copying accounts into Davis, the answers relay, the trash |
| `internal/web` | The accounts page and the connector's OAuth server |
| `internal/connector` | The MCP server and its tools |
| `internal/vault` | The vault on disk: writes, history, trash, links |
| `internal/store` | SQLite database and its migrations |
| `internal/ics`, `internal/oauth`, `internal/htmltext` | iCalendar encoding, sign-in, HTML to text |
| `deploy/`, `deploy.sh` | The server: Compose file, Caddy, CouchDB settings, CI's update script |
| `davis/`, `radicale/`, `bench/` | The local test bench (Radicale is kept for reference: it has no scheduling) |

`CLAUDE.md` has the notes and rules followed while building it.

## Credits

Built on [Davis](https://github.com/tchapi/davis) ([sabre/dav](https://sabre.io/dav/)),
[Caddy](https://caddyserver.com), [CouchDB](https://couchdb.apache.org),
[Self-hosted LiveSync](https://github.com/vrtmrz/obsidian-livesync) and
[livesync-bridge](https://github.com/vrtmrz/livesync-bridge), [go-ical](https://github.com/emersion/go-ical),
[rrule-go](https://github.com/teambition/rrule-go), the
[MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) and [modernc SQLite](https://modernc.org/sqlite).
