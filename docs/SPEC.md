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
  - **mirrors**: each external account's calendar copied into its own Davis collection, kept up to date;
  - **RSVP relay**: when I answer an invitation (iPhone or Obsidian), it sends the answer through that account;
  - **accounts page**: a small web page to add / remove accounts;
  - later (part 2, §9): **sending my own invitations** from the right account.

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
  invited one in `X-SOURCE-ATTENDEE`.
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
- **Open: the third, generic kind.** CalDAV (above, the recap's choice) covers accounts that have a calendar. The
  alternative is IMAP + SMTP (read invitation emails, answer by mail from that address): it covers *any* address,
  even without a calendar, but means parsing emails, and changes and cancellations arrive as emails too. To decide
  with the list of my actual addresses (§11).

## 5. Mirrors

### 5.1 Collections

- One Davis collection per account, e.g. "École (mirror)", created when the account is added, removed with it
  (after asking).
- Mirror events are **read-only except my `PARTSTAT`** (extension spec §4.6). If a client changes anything else,
  the provider wins at the next sync.
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
  in `X-SOURCE-ATTENDEE`;
- `STATUS:CANCELLED` / removal when the organizer cancels;
- the **meeting link** as `CONFERENCE;VALUE=URI;FEATURE=VIDEO:` (Google: `conferenceData.entryPoints` of type video,
  else `hangoutLink`, Zoom add-on included; Microsoft: `onlineMeeting.joinUrl`). Phone entries may go in
  `CONFERENCE;FEATURE=PHONE` (the extension ignores `tel:`). The APIs keep the link apart from the description, so
  copying the description alone can miss it (extension spec §4.5, §7.1). Open: whether Apple Calendar's own "Join"
  reads `CONFERENCE` or only links in the place / notes (§11);
- `URL:obsidian://calendar-tasks?vault=<vault>&uid=<UID>`, the link back to the note (extension spec §4.5); the
  vault name is a backend setting;
- `X-SOURCE-ACCOUNT` (which account; it's what makes an event a mirror for the extension), `X-RSVP-STATE` (§6).
- How the backend finds the provider's event id for an answer (`X-SOURCE-ID` on the event, as in the recap, or a
  table in its database): decide when coding.

### 5.3 Declined

Declined invitations stay in the mirror with `PARTSTAT=DECLINED`; the extension hides them (extension spec §4.6),
the iPhone follows its own "show declined events" setting.

## 6. RSVP relay

1. I answer on the iPhone or in Obsidian: my `PARTSTAT` changes in the mirror event on Davis (`PUT` with
   `If-Match`).
2. The backend watches the mirror collections (sync-collection; Davis has no push, so polling), compares my
   `PARTSTAT` with the last one it knows, and calls the adapter's `RSVP`.
3. It writes `X-RSVP-STATE`: `pending`, then `confirmed`, or `failed:<reason>`. The extension shows it under the
   buttons ("Sent to the organizer.", or why it failed) and removes it when I answer again.
4. A small "answers in flight" table keeps a provider sync from putting `NEEDS-ACTION` back before the provider
   confirms.

The provider then notifies the organizer from the invited address, and the next sync brings the state back.

## 7. Accounts page

- "Add account" → Google / Microsoft / Other → sign in (OAuth) or URL + app password → a name and a color → the
  mirror collection is created. The extension's "Accounts page URL" setting opens this page (its "Add account"
  button).
- Lists each account with its state (last sync, last error); remove an account (and its mirror, after asking).
- Not public: behind Cloudflare Access or reachable through Tailscale only.
- One-time setup per provider, done by me: a Google Cloud project (Calendar API on, OAuth consent "External",
  published), an Entra app registration (personal + any organization accounts). The page shows the redirect URI to
  paste. `http://localhost` redirect URIs work for local development.

## 8. Deployment

- **Where**: my existing VPS (next to the Minecraft server and the website); no new VPS needed.
- **Domain** at Cloudflare (or reuse the website's), subdomains such as `cal.` (Davis) and one for the backend;
  HTTPS with Let's Encrypt (also required for OAuth redirect URIs outside localhost). Minecraft stays "DNS only":
  Cloudflare's proxy only handles HTTP.
- **Easy to (re)deploy**: one compose file (Davis, backend, a reverse proxy with automatic HTTPS) and one `.env`
  for secrets; Davis on SQLite in a volume; backups of Davis's and the backend's databases.
- Davis settings as in `davis/davis.env.example` (`INVITE_FROM_ADDRESS` empty). The Davis user's email is the
  address the backend writes as my `ATTENDEE`.
- The iPhone account then points to `https://cal.<domain>/dav/` instead of the Tailscale bench.

## 9. Part 2: sending my own invitations

- The extension writes an event with my `ORGANIZER` and one `ATTENDEE` per guest: plain CalDAV.
- It goes from the account I chose: the event is created in that account's mirror collection, and the backend
  creates it through the provider (Google / Microsoft send the invitations from my real address, collect the
  answers, and the mirror brings the guests' answers back). Not Davis's SMTP (§3).
- The account is pre-filled with the one where the guest already appears in my events (as guest or organizer),
  else from their address's domain (`@school` → the school account); I can change it. Not from my mail (no mail
  access).
- Extension changes then: a "Guests" field; the account choice; mirror collections accept new events and edits to
  the events I organize.
- Same booking idea later (public page, slots): extension spec §11.

## 10. Milestones (in order)

- **B0 — Deploy Davis.** Compose + reverse proxy + domain on the VPS; move my data from the bench; the iPhone and
  Obsidian on the real address. *Needs me: the domain, VPS access.*
- **B1 — Go skeleton.** Config, SQLite, Davis CalDAV client (write mirror events, sync-collection), runs locally
  against the bench Davis.
- **B2 — Google, read.** OAuth (localhost first), mirror with the §5.2 checklist, tests on recorded API answers.
  *Needs me: the Google Cloud project.*
- **B3 — RSVP relay** for Google, `X-RSVP-STATE`, answers in flight.
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
- Deleting a mirrored invitation on the iPhone: decline it, or bring it back at the next sync?
- One backend for several Davis users (the friend), or one per person?
- Polling intervals, and later provider push (Google watch channels, Graph subscriptions need a public HTTPS
  endpoint).
- `X-` property names, fixed together with the extension (extension spec §12).
- Out of scope for now: an MCP endpoint (Claude outside Obsidian), `X-SOURCE-MAIL` (link to the original email).
