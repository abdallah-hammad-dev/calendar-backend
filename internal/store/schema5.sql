-- Migration 5 (2026-11-06): a refresh token, once used, stays as kind 'used' until it expires, with when it was
-- used: its second use is a copy someone else holds (security review, 2026-11-05). Once deployed, never edit it.
ALTER TABLE oauth_tokens ADD COLUMN used TEXT NOT NULL DEFAULT '';  -- RFC 3339, for kind 'used'
