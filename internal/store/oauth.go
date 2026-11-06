package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// OAuthClient is a client of the Claude connector's sign-in (spec §10.4).
type OAuthClient struct {
	ID           string
	SecretHash   string // "" = a public client
	Name         string
	RedirectURIs []string
	Created      time.Time
	LastUsed     time.Time
	Signed       bool // holds a token that hasn't expired and wasn't used up: signed in
}

func (s *Store) AddOAuthClient(ctx context.Context, c OAuthClient) error {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO oauth_clients (id, secret_hash, name, redirect_uris, created)
		VALUES (?, ?, ?, ?, ?)`, c.ID, c.SecretHash, c.Name, string(uris), timeText(c.Created))
	return err
}

const oauthClientCols = `id, secret_hash, name, redirect_uris, created, last_used,
	EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.client_id = oauth_clients.id AND t.kind != 'used' AND t.expires > ?)`

func scanOAuthClient(row interface{ Scan(...any) error }) (OAuthClient, error) {
	var c OAuthClient
	var uris, created, used string
	err := row.Scan(&c.ID, &c.SecretHash, &c.Name, &uris, &created, &used, &c.Signed)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.Created, _ = time.Parse(time.RFC3339, created)
	c.LastUsed, _ = time.Parse(time.RFC3339, used)
	return c, json.Unmarshal([]byte(uris), &c.RedirectURIs)
}

func (s *Store) OAuthClient(ctx context.Context, id string) (OAuthClient, error) {
	return scanOAuthClient(s.db.QueryRowContext(ctx, `SELECT `+oauthClientCols+` FROM oauth_clients WHERE id = ?`,
		timeText(time.Now()), id))
}

// OAuthClients lists the clients, the last used first.
func (s *Store) OAuthClients(ctx context.Context) ([]OAuthClient, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+oauthClientCols+` FROM oauth_clients ORDER BY last_used DESC, created DESC`,
		timeText(time.Now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OAuthClient
	for rows.Next() {
		c, err := scanOAuthClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteOAuthClient removes a client and its tokens (signed out for good: it has to register again).
func (s *Store) DeleteOAuthClient(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oauth_clients WHERE id = ?`, id)
	return err
}

// unsignedLife: a registration nobody signs in with goes after this. Anyone can register, and registrations are
// capped: a day let a script fill the cap with 20 a day, and no Claude could connect (security review, 2026-11-05).
// Signing in takes a minute.
const unsignedLife = time.Hour

// PruneOAuth drops expired tokens, and clients registered more than unsignedLife before `now` that never got one
// (anyone can register: what nobody signed in with doesn't stay).
func (s *Store) PruneOAuth(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE expires <= ?`, timeText(now)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM oauth_clients WHERE created < ? AND last_used = ''
		AND NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.client_id = oauth_clients.id)`,
		timeText(now.Add(-unsignedLife)))
	return err
}

func (s *Store) TouchOAuthClient(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE oauth_clients SET last_used = ? WHERE id = ?`, timeText(at), id)
	return err
}

// OAuthToken is a token's record, found by its hash.
type OAuthToken struct {
	Hash     string
	Kind     string // access, refresh, used (a refresh token used up)
	ClientID string
	Expires  time.Time
	Used     time.Time // kind used: when
}

func (s *Store) AddOAuthToken(ctx context.Context, t OAuthToken) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oauth_tokens (hash, kind, client_id, expires, created) VALUES (?, ?, ?, ?, ?)`,
		t.Hash, t.Kind, t.ClientID, timeText(t.Expires), timeText(time.Now()))
	return err
}

// OAuthToken finds a token that hasn't expired by its hash and kind (ErrNotFound otherwise).
func (s *Store) OAuthToken(ctx context.Context, hash, kind string, now time.Time) (OAuthToken, error) {
	t := OAuthToken{Hash: hash, Kind: kind}
	var expires string
	err := s.db.QueryRowContext(ctx, `SELECT client_id, expires FROM oauth_tokens WHERE hash = ? AND kind = ? AND expires > ?`,
		hash, kind, timeText(now)).Scan(&t.ClientID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	t.Expires, _ = time.Parse(time.RFC3339, expires)
	return t, err
}

// ErrUsed: a refresh token already used (OAuthToken.Used says when).
var ErrUsed = errors.New("store: refresh token already used")

// UseRefreshToken uses up a client's refresh token that hasn't expired: it serves once, and is kept as 'used' until
// it would have expired. Already used: ErrUsed, with the time. Unknown, expired, or another client's: ErrNotFound.
func (s *Store) UseRefreshToken(ctx context.Context, hash, clientID string, now time.Time) (OAuthToken, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE oauth_tokens SET kind = 'used', used = ?
		WHERE hash = ? AND kind = 'refresh' AND client_id = ? AND expires > ?`, timeText(now), hash, clientID, timeText(now))
	if err != nil {
		return OAuthToken{}, err
	}
	t := OAuthToken{Hash: hash, ClientID: clientID}
	var kind, expires, used string
	err = s.db.QueryRowContext(ctx, `SELECT kind, expires, used FROM oauth_tokens WHERE hash = ? AND client_id = ?`,
		hash, clientID).Scan(&kind, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	} else if err != nil {
		return t, err
	}
	t.Kind = kind
	t.Expires, _ = time.Parse(time.RFC3339, expires)
	t.Used, _ = time.Parse(time.RFC3339, used)
	switch n, _ := res.RowsAffected(); {
	case n == 1:
		return t, nil
	case kind == "used":
		return t, ErrUsed // by an earlier request, maybe one under way
	}
	return t, ErrNotFound
}

// DeleteOAuthTokens signs a client out: every token it holds goes (it may sign in again).
func (s *Store) DeleteOAuthTokens(ctx context.Context, clientID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE client_id = ?`, clientID)
	return err
}
