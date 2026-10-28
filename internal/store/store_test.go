package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backend.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// A database as B1 left it, with one item.
	for _, q := range []string{schemaV1, `PRAGMA user_version = 1`,
		`INSERT INTO accounts (kind, name, address, collection, created) VALUES ('ics', 'School', 'me@school.fr', '/c/', '')`,
		`INSERT INTO items (account_id, uid, source_data) VALUES (1, 'u1', 'BEGIN:VCALENDAR')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	it, err := st.Item(ctx, 1, "u1")
	if err != nil || it.Sent != "" || !it.SentAt.IsZero() {
		t.Fatalf("item after migration: %+v %v", it, err)
	}
}

func TestItemsAndSettings(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "backend.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	id, err := st.AddAccount(ctx, Account{Kind: "google", Name: "Gmail", Address: "me@gmail.com", Config: `{}`, Collection: "/c/"})
	if err != nil {
		t.Fatal(err)
	}
	sent := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	for _, it := range []Item{
		{AccountID: id, UID: "a", SourceID: "ga", SourceData: "x", Answer: "ACCEPTED", Sent: "ACCEPTED", SentAt: sent, RSVPState: "confirmed"},
		{AccountID: id, UID: "b", SourceID: "gb", SourceData: "x", Answer: "DECLINED", Sent: "DECLINED", RSVPState: "failed:gone"},
	} {
		if err := st.PutItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	a, err := st.ItemBySourceID(ctx, id, "ga")
	if err != nil || a.UID != "a" || !a.SentAt.Equal(sent) || a.RSVPState != "confirmed" {
		t.Fatalf("by source id: %+v %v", a, err)
	}
	if _, err := st.ItemBySourceID(ctx, id, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	// Signed in again: the answers given up on are tried again, the ones sent stay sent.
	if err := st.RetryFailedAnswers(ctx, id); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.Item(ctx, id, "a"); a.Sent != "ACCEPTED" {
		t.Fatal("a sent answer was reset")
	}
	if b, _ := st.Item(ctx, id, "b"); b.Sent != "" {
		t.Fatal("a failed answer wasn't reset")
	}

	if err := st.SetAccountConfig(ctx, id, `{"token":1}`); err != nil {
		t.Fatal(err)
	}
	if acct, _ := st.Account(ctx, id); acct.Config != `{"token":1}` {
		t.Fatal(acct.Config)
	}

	if v, err := st.Setting(ctx, "k"); v != "" || err != nil {
		t.Fatal(v, err)
	}
	st.SetSetting(ctx, "k", "1")
	st.SetSetting(ctx, "k", "2")
	if v, _ := st.Setting(ctx, "k"); v != "2" {
		t.Fatal(v)
	}
}
