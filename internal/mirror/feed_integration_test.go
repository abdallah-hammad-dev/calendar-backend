package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter/icsfeed"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

// Feeds told apart by their link (2026-11-04): one without my address (a timetable), two sharing it (the school's
// Outlook and its intranet), all in one collection. Each copy says its account, a second pass rewrites nothing,
// and removing one account deletes its copies only.
func TestFeedsWithoutOrSharingAnAddressAgainstDavis(t *testing.T) {
	url := os.Getenv("DAVIS_TEST_URL")
	if url == "" {
		t.Skip("DAVIS_TEST_URL not set")
	}
	ctx := context.Background()
	dc, err := davis.New(url, env("DAVIS_TEST_USER", "orga"), env("DAVIS_TEST_PASSWORD", "x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := dc.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "backend.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &Engine{Store: st, Davis: dc, Me: p.Email(), Home: p.Home, Vault: "test-vault", Loc: paris,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Adapter: func(a store.Account) (adapter.Adapter, error) {
			var c struct{ URL string }
			json.Unmarshal([]byte(a.Config), &c)
			return &icsfeed.Feed{URL: c.URL}, nil
		}}

	suffix := fmt.Sprint(time.Now().UnixNano())
	coll := p.Home + "it-feeds-" + suffix + "/"
	collName := "Integration feeds " + suffix
	if err := dc.MakeCalendar(ctx, coll, collName, "", "VEVENT"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.Delete(context.Background(), coll, "") })

	add := func(name, address, file string, events ...string) store.Account {
		t.Helper()
		path := filepath.Join(dir, file)
		body := feedHead + strings.Join(events, "") + "END:VCALENDAR\n"
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "\n", "\r\n")), 0o600); err != nil {
			t.Fatal(err)
		}
		conf, _ := json.Marshal(map[string]string{"url": "file://" + path})
		a, err := e.AddAccount(ctx, store.Account{Kind: "ics", Name: name, Address: address, Config: string(conf)}, collName)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	other := strings.ReplaceAll(plainEvent, "it-plain", "it-intranet")
	timetable := add("Cours", "", "cours.ics", invitationEvent("Cours", "NEEDS-ACTION"))
	outlook := add("Outlook", "me@school.test", "outlook.ics", plainEvent)
	intranet := add("Intranet", "me@school.test", "intranet.ics", other)

	if err := e.Pass(ctx, true); err != nil {
		t.Fatal(err)
	}
	copies := map[string]davis.Item{}
	for _, c := range []struct {
		a        store.Account
		uid, src string
	}{
		{timetable, "it-invitation@school.test", "ics:Cours"},
		{outlook, "it-plain", "ics:me@school.test"},
		{intranet, "it-intranet", "ics:me@school.test"},
	} {
		it, err := st.Item(ctx, c.a.ID, c.uid)
		if err != nil || it.Href == "" {
			t.Fatalf("%s: not mirrored (%v)", c.uid, err)
		}
		got, err := dc.Get(ctx, it.Href)
		if err != nil {
			t.Fatal(err)
		}
		cal, _ := ics.Parse(got.Data)
		ev := ics.Events(cal)[0]
		if v := ics.Value(ev, PropSourceAccount); v != c.src {
			t.Errorf("%s: source account %q, want %q", c.uid, v, c.src)
		}
		if c.a.Address == "" {
			// No address of mine: no ATTENDEE is me, none is rewritten.
			if ev.Props.Get(PropSourceAttendee) != nil || ics.Attendee(ev, p.Email()) != nil {
				t.Errorf("%s: an attendee was taken for me:\n%s", c.uid, got.Data)
			}
		}
		copies[c.uid] = got
	}

	if err := e.Pass(ctx, true); err != nil {
		t.Fatal(err)
	}
	for uid, before := range copies {
		if got, err := dc.Get(ctx, before.Href); err != nil || got.ETag != before.ETag {
			t.Errorf("%s: rewritten by the second pass (%v)", uid, err)
		}
	}

	if err := e.RemoveAccount(ctx, intranet, true); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.Get(ctx, copies["it-intranet"].Href); err == nil {
		t.Error("the removed account's copy is still there")
	}
	if _, err := dc.Get(ctx, copies["it-plain"].Href); err != nil {
		t.Errorf("the other account with the same address lost its copy: %v", err)
	}
}
