// Package store is the backend's SQLite database: accounts, and what it knows of each mirrored event.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaV1 string

// migrations[i] takes the database from version i to i+1 (PRAGMA user_version).
var migrations = []string{schemaV1}

var ErrNotFound = errors.New("store: not found")

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; the backend's passes run one after another anyway
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database version %d is newer than this backend (%d)", version, len(migrations))
	}
	for v := version; v < len(migrations); v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

type Account struct {
	ID          int64
	Kind        string
	Name        string
	Address     string
	Config      string
	Collection  string
	SourceToken string
	LastSync    time.Time
	LastError   string
	Created     time.Time
}

const accountCols = `id, kind, name, address, config, collection, source_token, last_sync, last_error, created`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var lastSync, created string
	err := row.Scan(&a.ID, &a.Kind, &a.Name, &a.Address, &a.Config, &a.Collection, &a.SourceToken, &lastSync,
		&a.LastError, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.LastSync, _ = time.Parse(time.RFC3339, lastSync)
	a.Created, _ = time.Parse(time.RFC3339, created)
	return a, err
}

func (s *Store) AddAccount(ctx context.Context, a Account) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO accounts (kind, name, address, config, collection, created)
		VALUES (?, ?, ?, ?, ?, ?)`, a.Kind, a.Name, a.Address, a.Config, a.Collection,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Account(ctx context.Context, id int64) (Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = ?`, id))
}

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	return err
}

func (s *Store) SetSourceToken(ctx context.Context, id int64, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET source_token = ? WHERE id = ?`, token, id)
	return err
}

// CollectionToken is the sync-collection token of a collection accounts write into (” = never read).
func (s *Store) CollectionToken(ctx context.Context, href string) (string, error) {
	var token string
	err := s.db.QueryRowContext(ctx, `SELECT davis_token FROM collections WHERE href = ?`, href).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return token, err
}

func (s *Store) SetCollectionToken(ctx context.Context, href, token string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO collections (href, davis_token) VALUES (?, ?)
		ON CONFLICT (href) DO UPDATE SET davis_token = excluded.davis_token`, href, token)
	return err
}

// SetSyncResult records how a pass went: err nil = success (last_sync now, error cleared).
func (s *Store) SetSyncResult(ctx context.Context, id int64, passErr error) error {
	if passErr != nil {
		_, err := s.db.ExecContext(ctx, `UPDATE accounts SET last_error = ? WHERE id = ?`, passErr.Error(), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET last_sync = ?, last_error = '' WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

type Item struct {
	AccountID  int64
	UID        string
	SourceID   string
	Invited    string
	SourceData string
	Href       string
	ETag       string
	BaseHash   string
	Partstat   string
	Answer     string
}

const itemCols = `account_id, uid, source_id, invited, source_data, href, etag, base_hash, partstat, answer`

func scanItem(row interface{ Scan(...any) error }) (Item, error) {
	var it Item
	err := row.Scan(&it.AccountID, &it.UID, &it.SourceID, &it.Invited, &it.SourceData, &it.Href, &it.ETag, &it.BaseHash, &it.Partstat, &it.Answer)
	if errors.Is(err, sql.ErrNoRows) {
		return it, ErrNotFound
	}
	return it, err
}

func (s *Store) Item(ctx context.Context, accountID int64, uid string) (Item, error) {
	return scanItem(s.db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE account_id = ? AND uid = ?`, accountID, uid))
}

// ItemByHref finds the mirrored event stored at a Davis href (as the backend wrote it), whichever its account.
func (s *Store) ItemByHref(ctx context.Context, href string) (Item, error) {
	return scanItem(s.db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE href = ?`, href))
}

// ItemsByUID lists the copies of one event, one per account that has it (I may be invited on several addresses).
func (s *Store) ItemsByUID(ctx context.Context, uid string) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemCols+` FROM items WHERE uid = ? ORDER BY account_id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) Items(ctx context.Context, accountID int64) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemCols+` FROM items WHERE account_id = ? ORDER BY uid`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) PutItem(ctx context.Context, it Item) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO items (`+itemCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, uid) DO UPDATE SET source_id = excluded.source_id, invited = excluded.invited,
		source_data = excluded.source_data,
		href = excluded.href, etag = excluded.etag, base_hash = excluded.base_hash, partstat = excluded.partstat,
		answer = excluded.answer`,
		it.AccountID, it.UID, it.SourceID, it.Invited, it.SourceData, it.Href, it.ETag, it.BaseHash, it.Partstat, it.Answer)
	return err
}

func (s *Store) DeleteItem(ctx context.Context, accountID int64, uid string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM items WHERE account_id = ? AND uid = ?`, accountID, uid)
	return err
}
