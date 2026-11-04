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
	Signed       bool // holds a token that hasn't expired: signed in
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
	EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.client_id = oauth_clients.id AND t.expires > ?)`

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

// PruneOAuth drops expired tokens, and clients registered more than a day before `now` that never got one (anyone
// can register: what nobody signed in with doesn't stay).
func (s *Store) PruneOAuth(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE expires <= ?`, timeText(now)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM oauth_clients WHERE created < ? AND last_used = ''
		AND NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.client_id = oauth_clients.id)`,
		timeText(now.Add(-24*time.Hour)))
	return err
}

func (s *Store) TouchOAuthClient(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE oauth_clients SET last_used = ? WHERE id = ?`, timeText(at), id)
	return err
}

// OAuthToken is a token's record, found by its hash.
type OAuthToken struct {
	Hash     string
	Kind     string // access, refresh
	ClientID string
	Expires  time.Time
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

// TakeOAuthToken finds a token as OAuthToken does and deletes it: a refresh token serves once.
func (s *Store) TakeOAuthToken(ctx context.Context, hash, kind string, now time.Time) (OAuthToken, error) {
	t, err := s.OAuthToken(ctx, hash, kind, now)
	if err != nil {
		return t, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE hash = ?`, hash)
	if err != nil {
		return t, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return t, ErrNotFound // taken by another request meanwhile
	}
	return t, nil
}
