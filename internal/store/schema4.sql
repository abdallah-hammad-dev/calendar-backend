-- Migration 4 (2026-11-04): the Claude connector's sign-ins (spec §10.4). Once deployed, never edit it.

-- An OAuth client that registered itself (dynamic client registration, RFC 7591): Claude, or another MCP client.
CREATE TABLE oauth_clients (
  id            TEXT PRIMARY KEY,          -- its client_id, random
  secret_hash   TEXT NOT NULL DEFAULT '',  -- SHA-256 of its secret, hex ('' = a public client: PKCE alone)
  name          TEXT NOT NULL DEFAULT '',  -- as it says (client_name), shown on the consent and accounts pages
  redirect_uris TEXT NOT NULL,             -- JSON list, matched exactly
  created       TEXT NOT NULL,             -- RFC 3339
  last_used     TEXT NOT NULL DEFAULT ''   -- RFC 3339, its last call with an access token (to the minute)
);

-- Tokens given to a client. Only their SHA-256 is kept: the database alone can't be used to call the connector.
CREATE TABLE oauth_tokens (
  hash      TEXT PRIMARY KEY,              -- SHA-256 of the token, hex
  kind      TEXT NOT NULL,                 -- 'access' or 'refresh'
  client_id TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
  expires   TEXT NOT NULL,                 -- RFC 3339
  created   TEXT NOT NULL
);
CREATE INDEX oauth_tokens_client ON oauth_tokens(client_id);
