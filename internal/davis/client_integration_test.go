package davis

// Against a real Davis: DAVIS_TEST_URL=http://127.0.0.1:9000/dav/ go test ./internal/davis/
// As the bench user orga (never abd, whose account is on the iPhone), in a throwaway collection deleted after.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func event(uid, summary string) string {
	return strings.ReplaceAll(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:`+uid+`
DTSTAMP:20261020T080000Z
DTSTART;VALUE=DATE:20261124
SUMMARY:`+summary+`
ORGANIZER:mailto:prof@school.test
ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:organisateur@bench.test
END:VEVENT
END:VCALENDAR
`, "\n", "\r\n")
}

func TestClientAgainstDavis(t *testing.T) {
	url := os.Getenv("DAVIS_TEST_URL")
	if url == "" {
		t.Skip("DAVIS_TEST_URL not set")
	}
	ctx := context.Background()
	c, err := New(url, env("DAVIS_TEST_USER", "orga"), env("DAVIS_TEST_PASSWORD", "x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Email() == "" || !strings.HasSuffix(p.Home, "/") {
		t.Fatalf("principal: %+v", p)
	}

	var b [4]byte
	rand.Read(b[:])
	coll := p.Home + "it-client-" + hex.EncodeToString(b[:]) + "/"
	if err := c.MakeCalendar(ctx, coll, "Client test", "#00AA00", "VEVENT"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Delete(context.Background(), coll, ""); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	first, err := c.Sync(ctx, coll, "")
	if err != nil || first.Token == "" || len(first.Changed) != 0 || !first.Full {
		t.Fatalf("empty collection: %+v %v", first, err)
	}

	href := coll + "one.ics"
	etag, err := c.Put(ctx, href, event("one", "One"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put(ctx, href, event("one", "One"), ""); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("create over an existing item: %v", err)
	}
	got, err := c.Get(ctx, href)
	if err != nil {
		t.Fatal(err)
	}
	if etag != "" && got.ETag != etag {
		t.Fatalf("PUT ETag %s, GET ETag %s", etag, got.ETag)
	}
	t.Logf("PUT etag %q, GET etag %q", etag, got.ETag)
	etag2, err := c.Put(ctx, href, event("one", "One, renamed"), got.ETag)
	if err != nil {
		t.Fatalf("PUT If-Match the GET ETag: %v", err)
	}
	if _, err := c.Put(ctx, href, event("one", "stale"), got.ETag); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("PUT with a stale ETag: %v", err)
	}

	ch, err := c.Sync(ctx, coll, first.Token)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Full || len(ch.Changed) != 1 || !SamePath(ch.Changed[0].Href, href) || !strings.Contains(ch.Changed[0].Data, "One, renamed") {
		t.Fatalf("changes: %+v", ch)
	}
	if etag2 != "" && ch.Changed[0].ETag != etag2 {
		t.Fatalf("sync ETag %s, PUT ETag %s", ch.Changed[0].ETag, etag2)
	}

	none, err := c.Sync(ctx, coll, ch.Token)
	if err != nil || none.Full || len(none.Changed)+len(none.Removed) != 0 || none.Token != ch.Token {
		t.Fatalf("no changes: %+v %v", none, err)
	}

	if err := c.Delete(ctx, href, ""); err != nil {
		t.Fatal(err)
	}
	gone, err := c.Sync(ctx, coll, ch.Token)
	if err != nil || len(gone.Removed) != 1 || !SamePath(gone.Removed[0], href) {
		t.Fatalf("removal: %+v %v", gone, err)
	}
	if _, err := c.Get(ctx, href); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GET a deleted item: %v", err)
	}

	// sabre's quirk: an unknown token with its prefix gets "no changes" and itself back; Sync must list everything.
	c.Put(ctx, coll+"two.ics", event("two", "Two"), "")
	bogus := "http://sabre.io/ns/sync/999999"
	full, err := c.Sync(ctx, coll, bogus)
	if err != nil || !full.Full || len(full.Changed) != 1 {
		t.Fatalf("unknown sabre token: %+v %v", full, err)
	}
	// A token from another server altogether.
	full, err = c.Sync(ctx, coll, "urn:other:token")
	if err != nil || !full.Full || len(full.Changed) != 1 {
		t.Fatalf("foreign token: %+v %v", full, err)
	}

	cals, err := c.Calendars(ctx, p.Home)
	if err != nil {
		t.Fatal(err)
	}
	for _, cal := range cals {
		if SamePath(cal.Href, coll) {
			if cal.DisplayName != "Client test" || !strings.EqualFold(cal.Color, "#00AA00") || cal.SyncToken == "" {
				t.Fatalf("calendar: %+v", cal)
			}
			return
		}
	}
	t.Fatalf("collection not listed: %+v", cals)
}
