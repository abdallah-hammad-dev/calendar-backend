-- Migration 3 (2026-11-04): the trash (spec §5.4). Once deployed, never edit it.

-- A copy I deleted on Davis, from any client: never written again until I restore it from the accounts page.
-- RFC 3339, when it was seen deleted ('' = not in the trash).
ALTER TABLE items ADD COLUMN trashed TEXT NOT NULL DEFAULT '';
-- Dates I deleted from a series on Davis (an EXDATE a client added): a JSON list of {"line": the EXDATE content
-- line, one value, "at": RFC 3339} ('' = none). Written back on every copy until restored.
ALTER TABLE items ADD COLUMN removed TEXT NOT NULL DEFAULT '';
