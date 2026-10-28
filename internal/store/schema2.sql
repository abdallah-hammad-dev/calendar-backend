-- Migration 2 (2026-10-23): the RSVP relay's state on each item, and the backend's own settings. Once deployed,
-- never edit it.

-- What the relay did with my answer (spec §6): the answer it sent, or failed to send for good ('' = none yet), when
-- it was sent, and the X-CALTASKS-RSVP-STATE it last wrote on the Davis copy.
ALTER TABLE items ADD COLUMN sent TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN sent_at TEXT NOT NULL DEFAULT '';   -- RFC 3339, when the provider took the answer
ALTER TABLE items ADD COLUMN rsvp_state TEXT NOT NULL DEFAULT '';

-- An event the provider reports deleted by its own id only (Google, Microsoft).
CREATE INDEX items_source ON items (account_id, source_id);

-- Values the backend makes for itself: the accounts page's session key.
CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
