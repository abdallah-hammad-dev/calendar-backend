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
  - later (part 2, §9): **organizing meetings** from the right account, with its video link.

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
  the plan B above, and what B1 runs with.
- **Open: the third, generic kind.** CalDAV (above, the recap's choice) covers accounts that have a calendar. The
  alternative is IMAP + SMTP (read invitation emails, answer by mail from that address): it covers *any* address,
  even without a calendar, but means parsing emails, and changes and cancellations arrive as emails too. To decide
  with the list of my actual addresses (§11).

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
- Mirror events are **read-only except my `PARTSTAT`** (extension spec §4.6), wherever they are: the extension
  tells them by `X-CALTASKS-SOURCE-ACCOUNT` on the event, not by the collection. If a client changes anything else, the
  provider wins at the next sync.
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
  copying the description alone can miss it (extension spec §4.5, §7.1). Open: whether Apple Calendar's own "Join"
  reads `CONFERENCE` or only links in the place / notes (§11);
- `URL:obsidian://calendar-tasks?vault=<vault>&uid=<UID>`, the link back to the note (extension spec §4.5); the
  vault name is a backend setting;
- `X-CALTASKS-SOURCE-ACCOUNT` (which account; it's what makes an event a mirror for the extension),
  `X-CALTASKS-RSVP-STATE` (§6). Its value is `<kind>:<address>` (`ics:me@school.fr`); the extension only checks that
  it's there.
- The provider's event id for an answer: **a table in the backend's database** (`items.source_id`, decided in B1),
  no `X-SOURCE-ID` on the event.

Rules the mirror applies on top (B1, `internal/mirror/build.go`):

- **The `ORGANIZER` is never rewritten**, and when it is me (an event I organize on that account) my `ATTENDEE` is
  left alone: making me an attendee would show Accept / Decline on my own event, and a Davis-address `ORGANIZER`
  would make Davis send invitations itself (part 2).
- **What a client adds is kept** across rewrites: `VALARM`s (once a copy exists, its alarms are the client's),
  `X-APPLE-*` / `X-MOZ-*` properties, and `X-CALTASKS-RSVP-STATE`. Everything else is the source's (the source wins).
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

## 6. RSVP relay

1. I answer on the iPhone or in Obsidian: my `PARTSTAT` changes in the mirror event on Davis (`PUT` with
   `If-Match`).
2. The backend watches the collections accounts write into (sync-collection; Davis has no push, so polling),
   compares my `PARTSTAT` with the last one it knows, and calls the adapter's `RSVP`.
3. It writes `X-CALTASKS-RSVP-STATE`: `pending`, then `confirmed`, or `failed:<reason>`. The extension shows it under
   the buttons ("Sent to the organizer.", or why it failed) and removes it when I answer again.
4. A small "answers in flight" table keeps a provider sync from putting `NEEDS-ACTION` back before the provider
   confirms. (B1: `items.answer`, set when my `PARTSTAT` on Davis differs from the one the backend wrote, used
   instead of the source's until the source shows the same answer. The relay call itself is B3.)

The provider then notifies the organizer from the invited address, and the next sync brings the state back.

## 7. Accounts page

- "Add account" → Google / Microsoft / Other → sign in (OAuth) or URL + app password → a name and the collection
  its events go into (one of mine). The extension's "Accounts page URL" setting opens this page (its "Add account"
  button).
- Lists each account with its state (last sync, last error); remove an account (and its events, after asking).
- Not public: behind Cloudflare Access or reachable through Tailscale only.
- One-time setup per provider, done by me: a Google Cloud project (Calendar API on, OAuth consent "External",
  published), an Entra app registration (personal + any organization accounts). The page shows the redirect URI to
  paste. `http://localhost` redirect URIs work for local development.

## 8. Deployment

- **Where**: my existing VPS (next to the Minecraft server and the website); no new VPS needed.
- **Domain** at Cloudflare (or reuse the website's), subdomains such as `cal.` (Davis) and one for the backend;
  HTTPS with Let's Encrypt (also required for OAuth redirect URIs outside localhost). Minecraft stays "DNS only":
  Cloudflare's proxy only handles HTTP.
- **No compression on `/dav/`** in the reverse proxy: Caddy then appends `-gzip` to `ETag` headers and `If-Match`
  fails (412, `bench/TESTS.md`). Davis's own Caddy already does it when asked; the backend asks for none.
- **Easy to (re)deploy**: one compose file (Davis, backend, a reverse proxy with automatic HTTPS) and one `.env`
  for secrets; Davis on SQLite in a volume; backups of Davis's and the backend's databases.
- Davis settings as in `davis/davis.env.example` (`INVITE_FROM_ADDRESS` empty). The Davis user's email is the
  address the backend writes as my `ATTENDEE`.
- The iPhone account then points to `https://cal.<domain>/dav/` instead of the Tailscale bench.

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

## 10. Milestones (in order)

- **B0 — Deploy Davis.** Compose + reverse proxy + domain on the VPS; move my data from the bench; the iPhone and
  Obsidian on the real address. *Needs me: the domain, VPS access.*
- **B1 — Go skeleton.** Config, SQLite, Davis CalDAV client (write mirror events, sync-collection), runs locally
  against the bench Davis. *Done 2026-10-22*, with the ICS feed adapter as its first source (§4).
- **B2 — Google, read.** OAuth (localhost first), mirror with the §5.2 checklist, tests on recorded API answers.
  *Needs me: the Google Cloud project.*
- **B3 — RSVP relay** for Google, `X-CALTASKS-RSVP-STATE`, answers in flight.
- **B4 — Accounts page.**
- **B5 — Microsoft** (personal, then school: consent test first). *Needs me: the Entra app, the school sign-in.*
- **B6 — The generic adapter** (§4, once decided).
- **B7 — Backend in production** on the VPS.
- Then part 2 (§9).

## 11. Open questions

- My actual addresses (Gmail? Outlook personal? school Microsoft? others?): sets the order and the generic adapter.
- Generic adapter: CalDAV or IMAP + SMTP (§4).
- Does Apple Calendar show its "Join" button for a `CONFERENCE`-only event? (bench: one event with `CONFERENCE`
  only, one with the link in the notes.)
- Deleting a mirrored invitation on the iPhone: decline it, or bring it back at the next sync? (B1, provisional:
  forgotten on Davis, brought back only when the source changes the event.)
- `SCHEDULE-AGENT=CLIENT` on the `ORGANIZER` of mirror events would stop Davis's own scheduling on them (the
  `SCHEDULE-STATUS=3.7` attempts, and a REPLY *delivered* if the organizer happens to be a user of the same Davis).
  Bench test needed: does the iPhone still show Accept / Maybe / Decline then?
- One backend for several Davis users (the friend), or one per person?
- Polling intervals, and later provider push (Google watch channels, Graph subscriptions need a public HTTPS
  endpoint).
- `X-` property names, fixed together with the extension (extension spec §4.7): all prefixed `X-CALTASKS-` (decided
  2026-10-22, RFC 5545's vendor prefix: no clash with another app's `X-SOURCE-ACCOUNT`); part 2's
  (`X-CALTASKS-SEND-ACCOUNT`, `X-CALTASKS-MEETING-TOOL`, `X-CALTASKS-SEND-STATE`) are still proposals.
- Out of scope for now: an MCP endpoint (Claude outside Obsidian), `X-CALTASKS-SOURCE-MAIL` (link to the original
  email).
