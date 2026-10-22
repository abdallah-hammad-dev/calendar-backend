-- Migration 1. Once deployed, never edit it: later changes are new entries in `migrations` (store.go).

-- An external account mirrored into one Davis collection (spec §4, §5.1).
CREATE TABLE accounts (
  id           INTEGER PRIMARY KEY,
  kind         TEXT NOT NULL,              -- ics (google, microsoft, caldav later)
  name         TEXT NOT NULL,              -- shown on the accounts page, and the collection's name
  color        TEXT NOT NULL DEFAULT '',
  address      TEXT NOT NULL,              -- the address invited on that account (written as X-SOURCE-ATTENDEE)
  config       TEXT NOT NULL DEFAULT '{}', -- kind-specific JSON (ics: the feed URL; later OAuth tokens)
  collection   TEXT NOT NULL,              -- the mirror collection's href on Davis
  source_token TEXT NOT NULL DEFAULT '',   -- the provider's sync token ('' = list everything next time)
  davis_token  TEXT NOT NULL DEFAULT '',   -- sync-collection token of the mirror collection
  last_sync    TEXT NOT NULL DEFAULT '',   -- RFC 3339, last successful pass
  last_error   TEXT NOT NULL DEFAULT '',
  created      TEXT NOT NULL
);

-- One mirrored event (master and overrides together, as one Davis item).
CREATE TABLE items (
  account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  uid         TEXT NOT NULL,
  source_id   TEXT NOT NULL DEFAULT '',  -- the provider's event id, to answer through its API
  invited     TEXT NOT NULL DEFAULT '',  -- my address invited in this event, if not the account's ('' = the account's)
  source_data TEXT NOT NULL,             -- the event as the source last gave it (iCalendar); the mirror is built from it
  href        TEXT NOT NULL DEFAULT '',  -- the Davis copy ('' = not written, or deleted on Davis)
  etag        TEXT NOT NULL DEFAULT '',  -- ETag of the version the backend wrote or last read
  base_hash   TEXT NOT NULL DEFAULT '',  -- hash of the copy built from the source when last written (skip if same)
  partstat    TEXT NOT NULL DEFAULT '',  -- my PARTSTAT in the version the backend wrote
  answer      TEXT NOT NULL DEFAULT '',  -- my answer read on Davis that the source doesn't show yet ('' = none)
  PRIMARY KEY (account_id, uid)
);
CREATE INDEX items_href ON items (href);
