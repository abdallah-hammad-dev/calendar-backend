# calendar-backend — spec

The server side of the Obsidian extension **calendar-tasks**
(https://github.com/abdallah-hammad-dev/calendar-tasks, its spec: `docs/SPEC.md` there, "the extension spec" below).
Decisions come from the design discussions of 2026-10-15 → 2026-10-22; tested facts are in `bench/TESTS.md`.

## 1. What it is

Two things running on my VPS, behind HTTPS on my own domain:

- **Davis** (sabre/dav): the CalDAV server, source of truth for my events and tasks. Apple Calendar / Reminders on
  the iPhone and the Obsidian extension both talk to it, nothing else. Already chosen and tested (§3).
- **The backend** (Go, to write): a robot next to Davis that brings my **other accounts** into it (Gmail,
  Microsoft — personal and school —, any CalDAV account) and carries my answers back:
  - **mirrors**: each external account's calendar copied into the Davis collection I choose for it, kept up to
    date;
  - **RSVP relay**: when I answer an invitation (iPhone or Obsidian), it sends the answer through that account;
  - **accounts page**: a small web page to add / remove accounts;
  - later (part 2, §9): **organizing meetings** from the right account, with its video link;
  - **the Claude connector** (part 3, §10): Claude's own chat (web, desktop, the phone's app) reads my vault and
    calendar, and writes notes, tasks and events.

The goal: every invitation reaching any of my addresses shows on the iPhone (Accept / Maybe / Decline + a
notification) and in Obsidian, and my answer leaves from the address that was invited.

## 2. Principles

1. **The extension never calls the backend** (extension spec §2.3). Everything goes through CalDAV data in Davis.
   Without the backend, everything works except external accounts.
2. **Read calendars, not mail.** Gmail and Outlook put the invitations they receive in the account's calendar
   themselves; reading the calendar gives each invitation up to date (changes, cancellations) with no email parsing.
3. **Provider-specific code lives here, and copies everything right.** One adapter per kind of account, behind one
   interface. Whatever the extension or the iPhone reads on an event, the backend writes it (§5.2).
4. **The extension stays a generic CalDAV client**, usable with any CalDAV server (iCloud, Fastmail…) without this
   backend. The backend exists only to gather several addresses into one Davis, because no standard links a CalDAV
   server to accounts it doesn't own.
5. **Secrets stay on the server**: OAuth tokens in the backend's database, never in the vault, never in git.

## 3. Facts that shape the design (tested or read, see `bench/TESTS.md`)

- **Davis shows Accept / Maybe / Decline and a notification on the iPhone** for an invitation written with a plain
  `PUT`, as long as my `ATTENDEE` address is the Davis user's email (`calendar-user-address-set`). Radicale doesn't
  (no CalDAV scheduling), hence Davis.
- **Davis holds one email per user**, so the backend writes my `ATTENDEE` with the Davis address and keeps the
  invited one in `X-CALTASKS-SOURCE-ATTENDEE`.
- **Davis sends no mail** while `INVITE_FROM_ADDRESS` is empty: answering an external organizer ends with
  `SCHEDULE-STATUS=3.7` on the `ORGANIZER`, nothing sent. Keep it empty in prod, or Davis would send a second
  (wrong) reply next to the backend's.
- **Davis reads no mailbox.** It only *sends* iMIP mail (one sender address for the whole server). Its "IMAP"
  option is only for checking login passwords.
- **Between users of the same Davis, scheduling works without the backend** (E2, E3): invitation delivered,
  answer back to the organizer (`SCHEDULE-STATUS=2.0`, REPLY in their inbox). A local invitation lands in one of
  the invitee's event calendars chosen by Davis (it was "Tests", not "Général").
- **When iOS answers, it rewrites the `ATTENDEE` parameters** (adds `CUTYPE`, `EMAIL`, drops `RSVP`): compare
  `PARTSTAT` only.
- **iOS needs HTTPS** (it sends no password over plain HTTP). **Times in UTC show a second "(GMT)" time on the
  iPhone**: write `TZID=` + `VTIMEZONE`.
- **Google's CalDAV is not usable as a generic server**: OAuth only (no password), no `VTODO`, scheduling "partly
  implemented". **Microsoft has no CalDAV at all.** Hence their own APIs.
- **SMTP for invitations was set aside**: SMTP takes OAuth2 (XOAUTH2), but Gmail then needs the full-mailbox scope
  (`https://mail.google.com/`, restricted); Davis's mailer is a fixed user/password and can't refresh an OAuth
  token; Exchange Online disables password SMTP by default at the end of December 2026, and schools often block
  SMTP entirely. And replies to an email invitation come back by email, which Davis can't read.
- **Google OAuth apps left in "Testing" get refresh tokens that expire after 7 days**: publish the app ("In
  production", unverified is fine for personal use: a warning screen at consent).

## 4. Accounts and adapters

| Kind | Read (mirror) | Answer (RSVP) | Sign-in |
|---|---|---|---|
| Google | Calendar API, `syncToken` | Calendar API: patch my attendee's `responseStatus`, `sendUpdates=all` | OAuth, scope `calendar.events` |
| Microsoft (personal, school) | Graph, delta queries | Graph: `accept` / `tentativelyAccept` / `decline`, `sendResponse: true` | OAuth (Entra app, personal + any organization), `Calendars.ReadWrite`, `offline_access` |
| Other (iCloud, Fastmail, Nextcloud…) | CalDAV (discovered through `/.well-known/caldav`), sync-collection | CalDAV: `PUT` my `PARTSTAT` on their server, which sends the reply itself | URL + app password |

- One Go interface for all three, e.g. `List(syncToken) → changes, nextToken` and `RSVP(eventID, partstat)`;
  part 2 adds create / update / delete (§9).
- The write scopes are needed anyway to answer, and they also cover sending invitations later: one sign-in per
  account, nothing to ask again for part 2.
- **The school account** may need an admin's consent for an unknown app (`AADSTS90094` / `AADSTS65001`), even if
  Thunderbird passes. Test early. Plan B: publish the school calendar as an ICS link (Outlook web → Calendar →
  Shared calendars → Publish) read by a read-only "ICS feed" adapter, answers kept locally and not sent; or forward
  to a personal Microsoft account if the school allows external forwarding.
- **ICS feed** (B1, `internal/adapter/icsfeed`): a published `https://` / `webcal://` link (or a `file://` for
  tests), read whole at each pass (its hash is the "sync token"), read-only: answers stay on Davis, not sent. It is
  the plan B above, and what B1 runs with. **My address in it is optional** (2026-11-04, the user adding the
  intranet's timetable with the school address already used by the school's Outlook feed: refused, "faudrait
  pouvoir en rajouter sans mail, ou alors pouvoir mettre le même mail"): it only says which `ATTENDEE` is me, and a
  timetable invites nobody. Feeds are told apart by their link (`webcal://` = `https://`), not by that address:
  several may share it.
- **Google** (B2, 2026-10-23, `internal/adapter/google`): the account's main calendar, `events.list` with
  `showDeleted` (a cancelled occurrence is how Google says it was taken out of its series) and recurring events not
  expanded; `410` on a sync token → a full listing. A series comes whole: when any of its events changed, it is
  listed again by `iCalUID` (master and changed occurrences share it), cancelled occurrences becoming `EXDATE`s.
  An occurrence I'm invited to without its series is written alone. Working-location entries ("Bureau",
  "Domicile") are not mirrored. My address: the ID token's verified email; an alias I'm invited on is the
  attendee marked `self`. Answer: a patch of my attendee only (`attendeesOmitted`), `sendUpdates=all`.
- **Microsoft** (B5, 2026-10-23, `internal/adapter/microsoft`): Graph v1.0 tracks changes only over a date range
  (`calendarView/delta`) and gives each occurrence apart with an id (and `iCalUId`) of its own. The range: a
  year back from the first listing (kept), a year and a half ahead, renewed by a full round when less than a year
  is left. Any change to an occurrence has the series read again from its master
  (`$select=…,exceptionOccurrences,cancelledOccurrences&$expand=exceptionOccurrences`): `RRULE` from Outlook's
  pattern, exceptions as overrides (`RECURRENCE-ID` from `originalStart`), cancelled occurrences
  (`OID.<id>.<day>`) as `EXDATE`s at the series' time of day. A deletion comes as an id alone, so the token (JSON)
  also maps each Graph id to its event's UID. Times asked in UTC (`Prefer: outlook.timezone`), written in the
  event's zone: Outlook's Windows names mapped to IANA with CLDR's `windowsZones.xml`. My answer is the event's
  `responseStatus` (I'm added as an attendee when invited through a group). My address: the mailbox's
  (`/me/calendar` owner), not the sign-in name. Answer: `accept` / `tentativelyAccept` / `decline`, with
  `sendResponse` only when the organizer asked for answers.
- **Open: the third, generic kind.** CalDAV (above, the recap's choice) covers accounts that have a calendar. The
  alternative is IMAP + SMTP (read invitation emails, answer by mail from that address): it covers *any* address,
  even without a calendar, but means parsing emails, and changes and cancellations arrive as emails too. To decide
  with the list of my actual addresses (§12).

## 5. Mirrors

### 5.1 Collections

- **Each account writes into a collection I choose** (2026-10-22, the user: "c'est à l'utilisateur de choisir où va
  quoi"): one of mine that holds events, e.g. "École" for the school account, "Général" for Gmail. The backend
  makes no collection: one only for an account is one I made for it. The collection keeps holding my own events
  and tasks, and maybe other accounts' events; the backend touches only the items it wrote (the `items` table knows
  their hrefs), and reads each collection once per pass, whatever the number of accounts writing into it.
- Removing an account deletes its events from Davis, or leaves them as they are (still marked as mirrored, no
  longer updated), as I say; the collection stays.
- What I give up by mixing: a mirrored event has its collection's color, and an account can't be hidden alone
  (hiding the collection hides everything in it). A dedicated collection keeps both.
- Mirror events are **read-only except my `PARTSTAT` and deleting them** (extension spec §4.6), wherever they are:
  the extension tells them by `X-CALTASKS-SOURCE-ACCOUNT` on the event, not by the collection. If a client changes
  anything else, the provider wins at the next sync. Deleted, they go to the trash (§5.4).
- **One copy of a meeting I'm invited to on two addresses** whose accounts write into the same collection: Davis
  takes two items with the same `UID` in one collection (tested) and would show both. The first account keeps its
  copy; the other's waits and is written if that copy goes. An item already at the href that isn't the account's
  (same `UID`, made by me or another client) is left alone.
- `UID` = the provider's iCalendar UID (`iCalUID` for Google): no duplicates, stable note links.
- Do not add the Google / Microsoft accounts on the iPhone as well: everything would show twice.

### 5.2 What the backend writes on each event

Every field the extension or the iPhone reads must be copied, or it's missing on mirrored events. Checklist:

- `SUMMARY`; `DTSTART` / `DTEND` with `TZID` + `VTIMEZONE` (all-day as `VALUE=DATE`);
- recurrence: `RRULE`, `EXDATE`, and each changed occurrence as an override with `RECURRENCE-ID` (an override is a
  whole component: copy every field into it);
- `LOCATION` as is (Microsoft: `location.displayName`; "Réunion Microsoft Teams" is shown as a place, on purpose);
- `DESCRIPTION` as **plain text**, keeping link targets: Google's `description` is HTML (convert, decode `&amp;`,
  keep each `href`); Microsoft: ask for text with `Prefer: outlook.body-content-type="text"`;
- `ORGANIZER`, every `ATTENDEE` with its `PARTSTAT`; **mine rewritten to the Davis address**, the invited address
  in `X-CALTASKS-SOURCE-ATTENDEE`;
- `STATUS:CANCELLED` / removal when the organizer cancels;
- the **meeting link** as `CONFERENCE;VALUE=URI;FEATURE=VIDEO:` (Google: `conferenceData.entryPoints` of type video,
  else `hangoutLink`, Zoom add-on included; Microsoft: `onlineMeeting.joinUrl`). Phone entries may go in
  `CONFERENCE;FEATURE=PHONE` (the extension ignores `tel:`). The APIs keep the link apart from the description, so
  copying the description alone can miss it (extension spec §4.5, §7.1). For the iPhone, whose Calendar may not
  read `CONFERENCE` (§12), the link also goes in `LOCATION` when the source has none (the iPhone's "Location or
  Video Call" field: one field for both), else in `DESCRIPTION` when it isn't there ("Google Meet: <link>"): a room
  is never replaced (2026-10-23, `adapter.Meeting`). The extension shows no place that is only the link, and one
  "Join" whatever the link's place. If the iPhone test shows `CONFERENCE` is enough, these copies go;
- `URL:obsidian://calendar-tasks?vault=<vault>&uid=<UID>`, the link back to the note (extension spec §4.5); the
  vault name is a backend setting;
- `X-CALTASKS-SOURCE-ACCOUNT` (which account; it's what makes an event a mirror for the extension),
  `X-CALTASKS-RSVP-STATE` (§6). Its value is `<kind>:<address>` (`ics:me@school.fr`), or `<kind>:<name>` for a feed
  without my address (`ics:Cours`); feeds sharing an address share it (the engine tells their copies apart by its
  `items`, not by this value; tested against the bench Davis). The extension only checks that it's there.
- The provider's event id for an answer: **a table in the backend's database** (`items.source_id`, decided in B1),
  no `X-SOURCE-ID` on the event.

Rules the mirror applies on top (B1, `internal/mirror/build.go`):

- **The `ORGANIZER` is never rewritten**, and when it is me (an event I organize on that account) my `ATTENDEE` is
  left alone: making me an attendee would show Accept / Decline on my own event, and a Davis-address `ORGANIZER`
  would make Davis send invitations itself (part 2).
- **What a client adds is kept** across rewrites: `VALARM`s (once a copy exists, its alarms are the client's),
  `X-APPLE-*` / `X-MOZ-*` properties. Everything else is the source's (the source wins). `X-CALTASKS-RSVP-STATE`
  is the relay's own record (`items.rsvp_state`), written on each component I'm invited in; a new answer clears it
  (B3).
- A source's own `URL` goes into `DESCRIPTION` (the extension still finds a meeting link there); `URL` is the
  `obsidian://` link. `METHOD` is dropped (a CalDAV object can't have one), `PRODID` is the backend's.
- Times in UTC become `TZID=<TIMEZONE>` wall-clock times, **except on a recurring master** (UTC there means the same
  instant every week, which a TZID would shift across DST); `RECURRENCE-ID`, `EXDATE`, `RDATE`, `UNTIL` are never
  touched. A `VTIMEZONE` is added for every TZID without one, generated from Go's zone database (same rules as
  the bench's hand-written Europe/Paris, tested).
- An item is written only when the copy built from the source changed (hash) or a client changed the Davis copy.
- An empty full listing from a source is believed only the second time in a row (as the extension does), so one bad
  answer can't delete every mirrored event.

### 5.3 Declined

Declined invitations stay in the mirror with `PARTSTAT=DECLINED`; the extension hides them (extension spec §4.6),
the iPhone follows its own "show declined events" setting.

### 5.4 Trash

(2026-11-04, the user: delete an account's event and no longer see it, "plus masqué que supprimé", with a way to
see what was deleted; no two-way sync, "c'est peut-être une bourbe"; no trash calendar showing in Apple Calendar, KDE
or any client; the extension asks first.) What the extension spec §4.6 asks of any backend that sets
`X-CALTASKS-SOURCE-ACCOUNT`: a deleted copy stays deleted and isn't passed on. The trash is how this one does it; the
extension says nothing of it (its popup stays generic).

- **Deleting is a plain CalDAV delete, from any client** (iPhone, the extension, KDE…): the copy leaves Davis, so
  every device. The backend sees it gone (sync-collection) and puts the item in the trash (`items.trashed`): never
  written again, whatever the source changes, until restored. The source's changes are still kept in the store, so
  a restore writes it up to date.
- **One date of a series**: a client deletes it as an `EXDATE` on the master (iOS and the extension both, the date's
  override removed). The backend takes an `EXDATE` on the Davis copy that the build from the source lacks for a
  date I deleted (`items.removed`, the line as the client wrote it) and writes it back on every copy, without that
  date's override, until restored. Compared as instants (`ics.Instant`): the same date in UTC or in a zone is one.
  Read in the Davis watch, before the source can change in the same pass.
- **Nothing goes to the provider**: deleting hides, it doesn't decline (to say no, decline). The relay sends nothing
  for an item in the trash.
- **Seen and restored on the accounts page** ("Corbeille"), not in a calendar: CalDAV can't hide a collection from
  some clients. Each row: title, date, account, when deleted, Restaurer. An item leaves the trash when its source
  deletes it (or cancels that date: nothing left to restore).
- A meeting invited on two addresses whose accounts write into one collection (§5.1): the copy held back stays
  held back while the other one is in the trash, and takes its place there if that one's source drops it.
- Not kept: "this and all following" from the iPhone (a changed `RRULE`, the source wins), and a copy moved to another
  calendar (the moved copy stays, no longer updated; the original goes to the trash).

## 6. RSVP relay

1. I answer on the iPhone or in Obsidian: my `PARTSTAT` changes in the mirror event on Davis (`PUT` with
   `If-Match`).
2. The backend watches the collections accounts write into (sync-collection; Davis has no push, so polling),
   compares my `PARTSTAT` with the last one it knows, and calls the adapter's `RSVP`.
3. It writes `X-CALTASKS-RSVP-STATE`: `pending`, then `confirmed`, or `failed:<reason>`. The extension shows it under
   the buttons ("Sent to the organizer.", or why it failed) and removes it when I answer again.
   As built (B3, 2026-10-23, `Engine.Relay`, each Davis pass): `confirmed` as soon as the provider's API takes the
   answer (one write in the usual case); `pending` only while it can't be reached (tried again at each pass);
   `failed:<reason>` when it refuses for good (`adapter.Refused`: event gone, account read-only, sign-in over…),
   not tried again until I answer again, or the account is signed in again. Reasons are in French, shown as is.
4. A small "answers in flight" table keeps a provider sync from putting `NEEDS-ACTION` back before the provider
   confirms. (B1: `items.answer`, set when my `PARTSTAT` on Davis differs from the one the backend wrote, used
   instead of the source's until the source shows the same answer.) Once the provider took it, a source still
   showing another answer more than 10 minutes later is a change made there since (in Gmail, Outlook): the source
   wins (B3).

The provider then notifies the organizer from the invited address, and the next sync brings the state back.

## 7. Accounts page

- "Add account" → Google / Microsoft / Other → sign in (OAuth) or URL + app password → a name and the collection
  its events go into (one of mine). I open the page myself: the extension has no link to it (2026-10-24, the user:
  a link made the extension depend on the backend, and accounts are added rarely).
- Lists each account with its state (last sync, last error); remove an account (and its events, after asking).
- **Corbeille**: the events and dates I deleted, the last first, each with Restaurer (§5.4).
- **Login with the Davis user** (2026-10-23, B4, `internal/web`), instead of Cloudflare Access or Tailscale: one
  backend per person, so one login, the one on the iPhone. Signed session cookie (30 days, key made once and kept
  in the database), a second's wait per failed login, `CrossOriginProtection` against other sites' posts, no
  framing, no caching, no referrer. OAuth with PKCE and a state good once for 15 minutes. "Sync now" starts a whole
  pass; adding an account starts one too. Signing an account in again must give the same address.
- One-time setup per provider, done by me: a Google Cloud project (Calendar API on, OAuth consent "External",
  published), an Entra app registration (personal + any organization accounts). The page shows the redirect URI to
  paste. `http://localhost` redirect URIs work for local development.
- **The OAuth clients are entered on the page or in the environment** (2026-11-03, the user: Google, Microsoft and
  ICS always on the page, with fields for the IDs and secrets, "tout en gardant la capacité de deploy avec l'env").
  The environment wins (`GOOGLE_CLIENT_ID`…, `deploy/.env`): the page shows that client, read-only. Otherwise what is
  typed there is kept in `settings` (one JSON row per kind) and put in place at once (`Kinds.SetProvider`; the
  next pass uses it). The secret is never shown back; a blank one keeps it. A new client ID or directory says to
  sign the kind's accounts in again. Shapes are checked (a Microsoft secret's ID instead of its value is refused).

## 8. Deployment

- **Where**: my VPS (`my-vps`, Ubuntu 26.04, 2 CPU, 3.7 GB), which hosts nothing else (2026-10-28, the user:
  no Minecraft server nor website there any more); only SSH listened when checked, Docker not installed yet.
- **Domain** at Cloudflare, subdomains such as `cal.` (Davis) and one for the backend; HTTPS with Let's Encrypt
  (also required for OAuth redirect URIs outside localhost).
- **No compression on `/dav/`** in the reverse proxy: Caddy then appends `-gzip` to `ETag` headers and `If-Match`
  fails (412, `bench/TESTS.md`). Davis's own Caddy already does it when asked; the backend asks for none.
- **Easy to (re)deploy**: one compose file (Davis, backend, a reverse proxy with automatic HTTPS) and one `.env`
  for secrets; Davis on SQLite in a volume; backups of Davis's and the backend's databases.
- Davis settings as in `davis/davis.env.example` (`INVITE_FROM_ADDRESS` empty). The Davis user's email is the
  address the backend writes as my `ATTENDEE`.
- **No Davis log file** (`LOG_FILE_PATH=/dev/null`): on every error it writes the whole request, the Basic password
  included (`bench/TESTS.md`, 2026-10-28). Its database keeps only an MD5-based hash, so long random passwords.
- The iPhone account then points to `https://cal.<domain>/dav/` instead of the Tailscale bench.
- **Decided 2026-10-23, for the deployment step:**
  - Everything needed for the calendar *and* the Obsidian vault's sync lives in this repo (the user: "tout ce qu'il
    faut pour avoir le calendrier, le sync obsidian"): the vault syncs with the **Self-hosted LiveSync** plugin
    (it runs inside the normal Obsidian app, iPhone included; end-to-end encrypted) through **CouchDB** in the same
    compose, at `sync.<domain>`. Syncthing was set aside (no real iPhone client). Since part 3 the VPS also holds
    the vault's key, for the Claude connector (§10.3).
  - Docker on the VPS (its service starts the containers again at boot: `restart: unless-stopped`); only the
    reverse proxy (Caddy) publishes ports, 80 and 443 (Docker's ports go around ufw).
  - `deploy.sh` asks for what's missing in `.env` (domain, Let's Encrypt email, my user and password; it makes
    the other secrets), checks it all before touching the VPS (values, SSH and sudo, DNS pointing at the VPS and
    not at Cloudflare's proxy), then after (real certificates, a Davis login, no compression on `/dav/`, CouchDB's
    settings for LiveSync). Run again safely.
  - CI/CD (the user, to test and fix quickly): a push on `main` runs the tests, builds the backend's image, and the
    VPS takes it; GitHub's key can only start that update, not open a shell. BRAT does the same for the extension.
  - `cal.` and `sync.` "DNS only" at Cloudflare: its proxy may weaken ETags when compressing (the 412s of the bench).
- **Built 2026-10-28, deployed 2026-10-29** (`deploy/`, `deploy.sh`, `.github/workflows/deploy.yml`; the whole
  stack first tested locally with Caddy's own certificates, `bench/TESTS.md`):
  - Addresses under one root (the user: one root, English names): `cal.my.example.com` (Davis),
    `sync.…` (CouchDB), `accounts.…` (the accounts page), all "DNS only". Paths under one host (`/my/cal`…) were
    set aside: one origin for the accounts page and two admin UIs we didn't write, Davis's absolute hrefs, and
    `/.well-known/caldav` wanted at the host's root.
  - Public: `/dav/` and `/.well-known/caldav` on `cal.` (Davis's dashboard and API answer 404: the dashboard is
    on the VPS's `127.0.0.1:9000`, through an SSH tunnel), CouchDB's API without `/_utils`, the accounts page.
  - Caddy drops `Accept-Encoding` toward Davis and asks for no compression itself (Go's transport would ask for
    gzip on its own, unzip it and keep the `-gzip` ETag).
  - My Davis user is made once by hand in the dashboard (Davis has no command or API for it): `deploy.sh` opens
    the tunnel and waits until the user logs in with its address.
  - CI: tests, the image into GitHub's registry (private), then SSH as `deploy` on the VPS, whose key's forced
    command is `sudo /opt/calendar/update <digest>` and nothing else (no shell, no forwarding; sudo for that
    script only). The workflow hands its own token (`packages: read`, dead once the run ends) on stdin: no
    registry secret on the VPS. `update` pulls by digest, restarts the backend, and puts the previous image back
    if `/healthz` doesn't answer within a minute. Changes to the rest (compose, Caddy, Davis, CouchDB) go through
    `deploy.sh`, run by hand.
  - Docker from Docker's own repository (the user, 2026-10-29, after weighing Ubuntu's `docker.io`), added to
    `unattended-upgrades` by `deploy.sh` so it updates by itself like the system.
  - Not done yet: backups of the databases, moving my data from the bench.
- **Part 3, written 2026-11-04** (§10): livesync-bridge in the compose (profile `vault`, built from a pinned
  commit), the backend running as its user, the vault volume shared by both. `deploy.sh` asks once for the vault's
  LiveSync database, passphrase and recovery code (empty database: no vault on the VPS, the connector has the
  calendar and tasks), runs the bridge on a copy first, then on the real database when I say so, and waits for its
  heartbeat. The connector is at `https://accounts.<domain>/mcp`.

## 9. Part 2: organizing meetings

Asked 2026-10-22: from Obsidian, select a slot, choose the account it's sent from (its video link with it), and
send the invitations, or just get a link to share. **No public booking page** (dropped the same day: not needed).

### 9.1 What I do

- In the calendar I select a slot. The form asks the **account** it's sent from (my connected accounts), the
  **video link** (the account's own tool, or none for a meeting in person: a menu of two, not a switch), and
  **guests**, which are optional: with none, it's just a meeting link to send myself (the card gets "Copy link"
  next to "Join").
- The account is pre-filled with the one where the guest already appears in my events (as guest or organizer),
  else from their address's domain (`@school` → the school account); I can change it. Not from my mail (no mail
  access). The video link is pre-filled with the account's tool.
- Later edits work the same way: moving, renaming, adding a guest or deleting an event I organize goes to the
  provider, which sends the update or the cancellation.

### 9.2 Video tools

**The tool follows the account** (2026-10-22, the user: "si g google je mets meet, si g teams je mets teams et
basta"): Meet from Google, Teams from Microsoft, nothing else. The provider attaches the meeting to the event, and
guests get a real "Join" button in Gmail / Outlook.

| Account it's sent from | Its own tool | How |
|---|---|---|
| Google | Google Meet | `conferenceData.createRequest` (`hangoutsMeet`), `conferenceDataVersion=1`; same `calendar.events` scope |
| Microsoft, school | Teams | `isOnlineMeeting: true`, `onlineMeetingProvider: teamsForBusiness`, if the calendar's `allowedOnlineMeetingProviders` has it (tenant policy); same `Calendars.ReadWrite` |
| Microsoft, personal | probably none | Graph's providers don't seem to include consumer Teams: to test in B5 |
| Other (CalDAV) | none | |

- **No Zoom for now** (2026-10-22): it would need its own integration (an app on Zoom's developer site, one more
  sign-in, the backend creating the meeting). Zoom invitations I *receive* already work: the extension finds the
  link and shows "Join". If needed one day, the cheapest is a fixed personal room link entered once, offered as a
  tool, with no API.
- The scopes asked for reading and answering already cover Meet and Teams links (to confirm in B2 / B5): one
  sign-in per account.

### 9.3 How it travels (the extension never calls the backend)

1. The extension writes the event into the collection I picked (where it shows, like any event), with its own `UID`, the
   slot, `ORGANIZER` = me and one `ATTENDEE` per guest **with `SCHEDULE-AGENT=CLIENT`** (RFC 6638: Davis must not send
   the invitations itself, to test on the bench), the account it's sent from
   (`X-CALTASKS-SEND-ACCOUNT:<kind>:<address>`, as `X-CALTASKS-SOURCE-ACCOUNT`) and the tool asked
   (`X-CALTASKS-MEETING-TOOL:google-meet | teams | none`). The account is the event's, not the collection's: where it
   shows and who sends it are two choices.
2. The backend's Davis watch sees it (it then reads every collection of mine, not only those accounts write into:
   sync tokens keep that cheap), creates it through the adapter with the tool asked, and writes `X-CALTASKS-SEND-STATE`:
   `pending`, then `sent` or
   `failed:<reason>` (shown by the extension like `X-CALTASKS-RSVP-STATE`). The provider makes the link and sends the
   invitations from my real address.
3. The next pass writes the provider's version over it (§5.2: `CONFERENCE` with the link, the guests' answers as
   they come). **The event keeps the extension's `UID`** (note links stay valid): the `items` table maps the
   provider's id to it. Creation is idempotent: Google's event `id` derived from the `UID`, Graph's `transactionId`
   = the `UID`, so a crash between creating and recording can't make two. The event stays in the collection where
   I made it (its href is recorded), even if the account writes its other events elsewhere.
4. Edits to an event I organize go to the provider (`sendUpdates=all` on Google). Other edits to mirrored events
   stay refused (§5.1): the provider's version wins.

To decide when coding: how the extension knows my accounts (address, tools) without asking the backend.
Proposal: the backend writes them as a property of my calendar home (`PROPPATCH`; to test whether Davis keeps
properties it doesn't know); without it, the form has no account choice.

### 9.4 Extension changes then

A "Guests" field, the account and tool choices in the new event form, edits allowed on the mirrored events I
organize, the `X-CALTASKS-SEND-STATE` line, "Copy link" on the card.

## 10. Part 3: the Claude connector

Asked 2026-11-04 (the user: "i really want a connector for claude main chat, there is a lot of disscutions there
[…] push information to my vault, and also create tasks and events"). Obsidian on the phone has no AI and Claude
Workspace is desktop-only (it runs Claude Code); the user: "if there is no ai and i can use the claude chat, it is
okey". Agreed the same day ("yeah do it"): keep the vault's encryption and mirror it on the VPS with livesync-bridge
(§10.3); tasks and events go straight into Davis (§10.2). **What it can do, the same day** (the user, after a
first draft that only created and appended: "le but c vrm d'avoir un truc similaire a claude code dans claude
workspace, pouvoir modifie en profondeur mes notes, pareil qu'il puisse choisir de cree un event to general ou ecole
sa depend de ce que je lui dit, cree des taches, les faire reference dans d'autres note fin logique quoi"): what
Claude Code does in Claude Workspace, from Claude's chat.

### 10.1 What I do

- In Claude's chat (web, desktop or the phone's app) I talk as I would to Claude Code in Claude Workspace: it reads
  and reworks my notes (writes, edits, moves, renames, deletes them), makes tasks and events in the collection that
  fits what I say ("Général", "École"…), changes them, and links them from other notes. It does it through a
  **custom connector**: an MCP server on my VPS, added once on claude.ai or in Claude Desktop (the phone's app can't
  add one, but uses those added there).
- What it writes reaches my devices the usual ways: tasks and events through Davis (the iPhone at its next fetch,
  Obsidian at its next sync), notes through LiveSync. **Principle 1 holds**: nothing in Obsidian talks to the
  connector; the extension sees CalDAV data and notes, as from any other client.
- **The one difference with Claude Workspace**: Claude works on the VPS's copy of the vault, not inside Obsidian.
  Every change reaches my devices *through sync*, never as a change made there: Obsidian updates no links on a
  rename it didn't make, and the extension converts no task line it didn't see typed. So the connector does
  itself what Obsidian and the extension do on a device (§10.2, §10.3).
- Set aside that day:
  - **The official Obsidian CLI on the VPS**: it drives a running Obsidian desktop app (IPC). The VPS would run
    Obsidian with no screen (Electron, a virtual display, LiveSync and the extension inside, a keychain for the
    CalDAV password): heavy and fragile. And the extension's logic already runs on my devices when they sync.
  - **Turning LiveSync's encryption off**: every device rebuilt, the vault in clear in CouchDB and its backups, for
    what the bridge gives with encryption on.
  - **An AI inside Obsidian on the phone, through Claude Code on the VPS** (the user: "maybe not worth the hassle cuz
    too unclean": the answer would come back through sync). Claude's own app does it.
  - **An inbox that keeps the key off the VPS** (the connector queues notes, a plugin on a device writes them):
    Claude couldn't read or search the vault, and a note would land only once an Obsidian opens.

### 10.2 Tasks and events: through Davis

The connector is one more CalDAV client, as the iPhone is, and **a task or an event has one writer: Davis**. Their
notes' properties, names and existence follow from Davis on each device (the extension's own rules); only a note's
body is the vault's (§10.3). Same rules as the mirror (§5.2): times in my zone (`TIMEZONE`, `TZID` + `VTIMEZONE`),
`PUT` with `If-None-Match: *` for a new item and `If-Match` for a change, items edited in place (extension spec
§6.2: `VALARM`, `X-APPLE-*` and unknown properties kept), contents never logged.

- **The collection is Claude's choice**, from what I say: every tool that makes an item takes one, among those
  `collections_list` gives (names, what each holds, its items' counts). A task or event moved to another collection
  is created there, then deleted from the old one, same `UID` (as the extension does).
- **A task is a `VTODO`**: `SUMMARY`, `DUE` (timed, or `VALUE=DATE`), `RRULE`, `PRIORITY` 1 / 5 / 9, `CATEGORIES`
  (its tags), `RELATED-TO;RELTYPE=PARENT` for a sub-task, `STATUS` / `COMPLETED`, `CREATED`. No `URL`: the extension
  adds its link.
  - **Made**: the extension makes the task note at its next pull, as for a reminder made on the iPhone, and **the
    reminder's notes (`DESCRIPTION`) become the note's body** (`TaskSync.applyItem`, only when it makes the note:
    afterwards the body is the vault's). One note whatever the number of devices (extension spec §6.2). It shows on
    the iPhone even with no Obsidian open.
  - **Never as a note file**, nor as a `- [ ] … @…` line: a note from the bridge and one made by the extension from
    the reminder could reach a device in either order (two notes, or a LiveSync conflict), and a synced line is never
    converted.
  - **Linked from other notes**: `task_create` returns the name the note will have (the extension's: `SUMMARY` with
    `/\:*?"<>|#^[]` turned into spaces, in its tasks folder) after checking it's free in the vault, else says so
    (the extension would add " (2)"). Claude then writes `[[Title]]` wherever it fits (a `- [[Title]]` line is a
    task link with its checkbox); the link resolves once an Obsidian has made the note. A sub-task's line in its
    parent's note is the extension's (it adds it unless the body already links to it).
  - **Changed** in Davis: title (the extension renames the note through Obsidian, links follow), deadline,
    repetition, priority, tags, parent, collection, done (a repeating task moves on as when ticked on the phone).
  - **Deleted** in Davis: the extension leaves the note `done` and unlinked (extension spec §6.4). Its file can then go
    through `vault_delete`, once it has no `caldav_uid` any more.
- **An event is a `VEVENT`** of mine (no `X-CALTASKS-SOURCE-ACCOUNT`, so editable everywhere): `SUMMARY`, `DTSTART`
  / `DTEND` (`VALUE=DATE` all day), `LOCATION`, `DESCRIPTION`, `RRULE`; changed and deleted in Davis, one occurrence of
  a series as an exception or an `EXDATE`. A **work session** on a task is a `VEVENT` with `RELATED-TO:<task UID>`
  (extension spec §4.4). **Mirrored events** are read-only but for my answer (`rsvp`: my `PARTSTAT`, which the relay
  sends, §6) and deleting them (the trash, §5.4).
- **An event's note**: `event_note` finds it (`caldav_uid`, and `recurrence_id` for an occurrence) or makes it as the
  extension would (`type: event`, `start`, `end`, `caldav_uid`, `recurrence_id`; named `<day> <Title>` in its events
  folder, extension spec §4.3). Claude then writes in it like any note; its `start` / `end` stay CalDAV's.
- **The extension's folders and date format** (tasks folder, events folder, `eventDateFormat`) are its settings,
  which don't sync here: the backend has them too, `TASKS_FOLDER` (`Tasks`), `EVENTS_FOLDER` (`Calendar`),
  `EVENT_DATE_FORMAT` (`DD-MM-YYYY`), the defaults being mine (read 2026-11-04).
- **Reading**: events in a range (mirrored ones marked, with their account; declined ones hidden unless asked; open
  tasks' deadlines), tasks (by state, tag, deadline, collection, words), free slots (09:00–18:00 by default). Davis's
  `calendar-query` with `expand`: sabre/dav expands recurrences itself (checked on the bench, `bench/TESTS.md`).

### 10.3 Notes: the vault mirrored on the VPS

- **livesync-bridge** (https://github.com/vrtmrz/livesync-bridge, by LiveSync's author, maintained: last commit
  2026-11-04) as one more service in the compose, with two peers: my LiveSync database (CouchDB over the compose
  network, not `sync.`) and a folder in a volume. It keeps both identical, both ways. The backend reads and writes
  that folder as plain markdown files. **No image is published**: compose builds it from a pinned commit
  (`1c25230`, 2026-11-04), under the profile `vault` (it runs only once configured).
- **The bridge holds the passphrase and the vault's ID key** (LiveSync: "Show current recovery code"): `deploy.sh`
  asks for them once, with the database's name, into `deploy/.env` (the passphrase in base64: `.env` can't hold
  every character), and writes the bridge's config from them (`/opt/calendar/bridge/config.json`, root's, readable
  by the bridge's group only). They don't go into the VPS's `.env`. **Encryption then protects the vault in
  CouchDB's data and backups, not from someone inside the VPS**, which already holds my calendar and the accounts'
  OAuth tokens in clear.
- **My LiveSync settings fit** (read 2026-11-04 on the PC): legacy path identity (`pathNormalizationVersion: 0`, the
  only one the bridge supports), E2EE v2, path obfuscation (the same passphrase), an ID key (v1). Every device must
  keep legacy path identity. A move to the other one means rebuilding the database on every device.
- Hidden files (`.obsidian/`, `.claude/`) aren't mirrored: that's the bridge's default, and LiveSync doesn't sync them
  here either.
- **The vault tools are Claude Code's file tools**: list (folders, a glob), search (names and contents, a regex),
  read, write (a new note, or a whole note again), edit (exact replacements, as Claude Code's Edit), move / rename,
  delete. Any folder: Claude picks where a note goes from the vault's layout, as in Claude Workspace.
- **Nothing lost to a change it didn't see**: an edit applies only if the text it replaces is still there; writing a
  whole note over one that exists needs the version `vault_read` gave (refused if the note changed since). A note
  I'm editing on a device at the same moment can still conflict in LiveSync, as between two of my devices.
- **Rename and move rewrite the links** to the note in every other note (`[[Name]]`, `[[Name|alias]]`,
  `[[Name#heading]]`, embeds, as Obsidian does): a device only sees a note gone and another one made, and updates
  nothing.
- **Delete goes to a trash on the VPS** (beside the vault folder, not synced): gone from every device, kept until
  restored. **The connector keeps the previous version** of each note it changes the same way, for 30 days (links
  rewritten by a move included), so a deep rework can be undone. `vault_restore` lists both, shows one, restores it.
- **A task note's properties, name and existence are Davis's** (§10.2): the vault tools change its body only, and send
  a rename, a delete, or a property change to the task tools. An event note's `start`, `end`, `caldav_uid` and
  `recurrence_id` stay as they are.
- **The bridge's watcher misses a file that arrives in a folder just made** (its README says so; tested
  2026-11-04, `bench/TESTS.md`): a note written into a new folder, and every file moved into a new folder or under a
  renamed one. The devices then lose the old path and never get the new one. So the connector writes such a file
  again 3 seconds later, same content (by then the watcher has the folder). Checking CouchDB instead was set aside:
  the document IDs are obfuscated with the passphrase, and the rewrite is enough (tested). A restart of the backend
  within those 3 seconds loses the rewrite: the file stays on the VPS until it changes again.
- The vault volume holds `notes/` (the bridge's folder: the vault), and beside it the connector's `history/`,
  `trash/` and `tmp/`. Writes are atomic: a temporary file in `tmp/` (same volume), then renamed into `notes/` (a
  temporary file inside the folder would be synced).
- File ownership: the bridge runs as uid 1993 (its image's user, which owns its Deno cache). **The backend runs as
  that user too** (compose `user: "1993:1993"`); `deploy.sh` owns the backend's data volume and the vault volume to
  it on every run (a one-shot `owner` service in Caddy's image: distroless has no `chown`), with the backend stopped
  meanwhile. It starts in `/data` (`working_dir`): distroless's `/home/nonroot` is its own user's only.
- **First on a copy of the database** (`deploy.sh` asks at the first setup): `<database>-bridge-copy`, replicated by
  CouchDB, with LiveSync's `_local` documents copied by hand (CouchDB replicates none: the milestone, without which
  the bridge never watches the database, and the sync parameters, which hold E2EE's salt). There, check that the
  mirror has every note, readable, and, from a second Obsidian vault on the PC pointed at the copy (optional): a
  note the connector writes opens there, an edit made there comes back, a rename and a delete reach it, and no
  conflict appears. Then `deploy.sh` moves the bridge to the real database: the mirror is made again from nothing
  (its files, the connector's history and trash, the bridge's resume points) and the copy is deleted. **Done
  2026-11-04**: on the copy, then on the real database; the bridge wrote nothing back to either (sequences
  unchanged), and the mirror read through the connector matched the PC's vault file for file (`bench/TESTS.md`).
- At start the bridge sends only the files present and changed, never a deletion: an empty folder doesn't empty
  the vault (tested). Its logs name the notes it syncs (debug level, no setting): Docker's `local` driver, rotated,
  on the VPS only.
- **LiveSync hands a rename to other devices as a create then a delete** (read in its code, 1.0.43,
  2026-11-04: `renameFileInDBCore` stores the new path, then deletes the old one; another device applies only a
  change of case as a rename). The extension there then sees a task note deleted, and `TaskSync.noteDeleted`
  deletes its reminder: a task note renamed on the PC (or by the extension, after a title changed on the iPhone)
  while another Obsidian is open would lose its reminder, then show as done. An extension bug, there already
  without the connector: reproduced in its two-device tests and fixed the same day (extension spec §6.4: the reminder
  goes 10 seconds after its note, unless another note holds its `caldav_uid` by then).

### 10.4 The MCP endpoint

- **In the backend**, which already has the Davis client, `TIMEZONE`, a public HTTPS host and the Davis login.
  Proposal: `accounts.…/mcp` rather than a new host (same login, no DNS), MCP over streamable HTTP, with the official
  Go SDK (`github.com/modelcontextprotocol/go-sdk`, to check).
- **Sign-in: OAuth**, which claude.ai asks of a custom connector. The backend is its own authorization server
  (protected resource metadata, authorization server metadata, authorization code + PKCE S256, access tokens of an
  hour, refresh tokens of 90 days that rotate, codes of 10 minutes; only hashes stored, migration 4). Claude
  registers itself (dynamic client registration), only with its callback: `https://claude.ai/api/mcp/auth_callback`,
  and `https://claude.com/api/mcp/auth_callback`, which Anthropic says may replace it; loopback addresses
  (`http://localhost`, `127.0.0.1`, `[::1]`) too, for an MCP client on my own computer (Claude Desktop's, Claude
  Code's), which the consent still asks me for. The consent page asks for the Davis user, like the accounts page.
  That page shows the connector's address and lists the connected clients (name, since when, last used) with a
  Déconnecter button that revokes their tokens.
- **Tools**: `collections_list`; `calendar_list`, `calendar_free_slots`, `event_create` / `event_update` /
  `event_delete`, `rsvp`, `event_note`; `tasks_list`, `task_create` / `task_update` / `task_delete`,
  `task_schedule` (a work session); `vault_list`, `vault_search`, `vault_read`, `vault_write`, `vault_edit`,
  `vault_move`, `vault_delete`, `vault_restore` (21, built 2026-11-04 in `internal/connector`, the files in
  `internal/vault`). Names and inputs close to the extension's Claude module (extension spec §8) and to Claude
  Code's file tools, so Claude handles them alike. Each tool's description says the rules above that concern it (a
  task's note is made by the extension, links resolve once it exists…), and the server's instructions the rules
  common to all. Without a vault (`VAULT_DIR` empty: the bridge not configured) the note tools say so; the calendar
  and task tools work.
- **It is the strongest access on the server**: it reads and rewrites the whole vault and the calendar. Logs keep the
  tool's name and UIDs only: no contents, no inputs or outputs, no note paths (they are note titles).

## 11. Milestones (in order)

- **B0 — Deploy Davis.** Compose + reverse proxy + domain on the VPS; move my data from the bench; the iPhone and
  Obsidian on the real address. *Needs me: the domain, VPS access.* *Written 2026-10-28* (`deploy.sh`, §8),
  tested locally; to run on the VPS.
- **B1 — Go skeleton.** Config, SQLite, Davis CalDAV client (write mirror events, sync-collection), runs locally
  against the bench Davis. *Done 2026-10-22*, with the ICS feed adapter as its first source (§4).
- **B2 — Google, read.** OAuth (localhost first), mirror with the §5.2 checklist, tests on recorded API answers.
  *Needs me: the Google Cloud project.* *Coded 2026-10-23*, tested on answers shaped from the API reference; to
  test on my account.
- **B3 — RSVP relay** for Google, `X-CALTASKS-RSVP-STATE`, answers in flight. *Coded 2026-10-23* (every kind),
  tested against the bench Davis.
- **B4 — Accounts page.** *Coded 2026-10-23*, tested against the bench Davis with a fake provider.
- **B5 — Microsoft** (personal, then school: consent test first). *Needs me: the Entra app, the school sign-in.*
  *Coded 2026-10-23* with B2; to test on my accounts.
- **Order changed 2026-10-23** (the user: test directly on a real domain rather than localhost and Tailscale):
  B2–B5 coded first, then B0 and B7 together (Davis, the backend, then the vault sync, on the VPS), then tests on
  my accounts and fixes.
- **B6 — The generic adapter** (§4, once decided).
- **B7 — Backend in production** on the VPS. *Written 2026-10-28* with B0: CI deploys it on each push to `main`.
- **B8 — The Claude connector** (§10, the next one since 2026-11-04). The bridge on the VPS against a copy of the
  LiveSync database, then the real one; the endpoint and its OAuth; the calendar tools, then the vault's; added on
  claude.ai, tried from the phone's app. *Needs me: the passphrase and the recovery code (`deploy.sh` asks), the
  connector added on claude.ai.* *Coded 2026-11-04*: the endpoint, its OAuth and the 21 tools, tested against the
  bench Davis and over HTTP; the bridge's compose service and `deploy.sh`'s steps, the bridge tried locally with
  the backend's vault code (`bench/TESTS.md`). To run on the VPS.
- Then part 2 (§9).

## 12. Open questions

- My actual addresses: Gmail and a school Microsoft account, 2026-11-01. Others? Sets the generic adapter.
- The school's Entra route: a multi-tenant app without a verified publisher is likely stepped up to an admin, so try
  an app registered in the school's own directory (`docs/comptes.md`), then the ICS link (`bench/TESTS.md`).
- Generic adapter: CalDAV or IMAP + SMTP (§4).
- Does Apple Calendar show its "Join" button for a `CONFERENCE`-only event? (on the VPS: one event with
  `CONFERENCE` only, one with the link in the notes, one with it in the place; and whether two of them make two
  "Join".)
- Deleting an invitation on the iPhone: does iOS send a decline first (which the relay would pass on)? Deleting is
  meant to hide only (§5.4, 2026-11-04). To test on the phone.
- `SCHEDULE-AGENT=CLIENT` on the `ORGANIZER` of mirror events would stop Davis's own scheduling on them (the
  `SCHEDULE-STATUS=3.7` attempts, and a REPLY *delivered* if the organizer happens to be a user of the same Davis).
  Bench test needed: does the iPhone still show Accept / Maybe / Decline then?
- ~~One backend for several Davis users, or one per person?~~ One per person, each on their own VPS (2026-10-23,
  the user: "mes amis ils auront leurs propres VPS, c'est un utilisateur par VPS").
- To check at the first test on real accounts (read in the API references, not tried): Google lists a series'
  master and changed occurrences by `iCalUID`; Graph's delta gives single events whole (the docs say so); a
  cancelled occurrence's day (`OID.<id>.<day>`) is the series' local day; an all-day event asked in UTC comes at
  midnight; `sendResponse: false` when the organizer asked for no answer; Graph lists series masters in a delta
  round or only their occurrences (both handled).
- Does Gmail / Outlook keep a meeting an emailed invitation *declares* (RFC 7986 `CONFERENCE`, or
  `X-GOOGLE-CONFERENCE` / `X-MICROSOFT-SKYPETEAMSMEETINGURL`) from a tool it doesn't know? The backend reads the
  event the provider made of the email, never the email (§2): if the provider drops the declaration, only the link
  the tool also wrote in the text (or the place) is left. Test: send myself such an invitation, read both APIs.
- Polling intervals, and later provider push (Google watch channels, Graph subscriptions need a public HTTPS
  endpoint).
- `X-` property names, fixed together with the extension (extension spec §4.7): all prefixed `X-CALTASKS-` (decided
  2026-10-22, RFC 5545's vendor prefix: no clash with another app's `X-SOURCE-ACCOUNT`); part 2's
  (`X-CALTASKS-SEND-ACCOUNT`, `X-CALTASKS-MEETING-TOOL`, `X-CALTASKS-SEND-STATE`) are still proposals.
- Out of scope for now: `X-CALTASKS-SOURCE-MAIL` (link to the original email).
