package mirror

// End-to-end against a real Davis: DAVIS_TEST_URL=http://127.0.0.1:9000/dav/ go test ./internal/mirror/
// Runs as the bench user orga (DAVIS_TEST_USER / DAVIS_TEST_PASSWORD to change), never abd: abd's account is on
// the iPhone, and an invitation there notifies it. Makes a throwaway mirror collection and deletes it.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter/icsfeed"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const feedHead = "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//feed//EN\nMETHOD:PUBLISH\n"

func invitationEvent(summary, partstat string) string {
	return `BEGIN:VEVENT
UID:it-invitation@school.test
DTSTAMP:20261020T080000Z
SEQUENCE:0
SUMMARY:` + summary + `
DTSTART:20261123T090000Z
DTEND:20261123T100000Z
ORGANIZER;CN=Prof:mailto:prof@school.test
ATTENDEE;CN=Prof;PARTSTAT=ACCEPTED;ROLE=CHAIR:mailto:prof@school.test
ATTENDEE;PARTSTAT=` + partstat + `;RSVP=TRUE;ROLE=REQ-PARTICIPANT:mailto:me@school.test
END:VEVENT
`
}

const plainEvent = `BEGIN:VEVENT
UID:it-plain
DTSTAMP:20261020T080000Z
SUMMARY:Plain
DTSTART;VALUE=DATE:20261124
DTEND;VALUE=DATE:20261125
END:VEVENT
`

const seriesEvent = `BEGIN:VEVENT
UID:it-series
DTSTAMP:20261020T080000Z
SUMMARY:Series
DTSTART;TZID=Europe/Paris:20261102T140000
DTEND;TZID=Europe/Paris:20261102T150000
RRULE:FREQ=WEEKLY;COUNT=3
END:VEVENT
BEGIN:VEVENT
UID:it-series
RECURRENCE-ID;TZID=Europe/Paris:20261109T140000
DTSTAMP:20261020T080000Z
SUMMARY:Series (moved)
DTSTART;TZID=Europe/Paris:20261109T160000
DTEND;TZID=Europe/Paris:20261109T170000
END:VEVENT
`

func TestEngineAgainstDavis(t *testing.T) {
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
	if p.Email() == "" {
		t.Fatal("the test user has no email on Davis")
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "backend.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) // after the account's cleanup (cleanups run last first)
	feed := filepath.Join(dir, "feed.ics")
	writeFeed := func(events ...string) {
		t.Helper()
		body := feedHead + strings.Join(events, "") + "END:VCALENDAR\n"
		if err := os.WriteFile(feed, []byte(strings.ReplaceAll(body, "\n", "\r\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := &Engine{Store: st, Davis: dc, Me: p.Email(), Home: p.Home, Vault: "test-vault", Loc: paris,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Adapter: func(a store.Account) (adapter.Adapter, error) {
			var c struct{ URL string }
			json.Unmarshal([]byte(a.Config), &c)
			return &icsfeed.Feed{URL: c.URL}, nil
		}}
	if testing.Verbose() {
		e.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	conf, _ := json.Marshal(map[string]string{"url": "file://" + feed})
	a, err := e.AddAccount(ctx, store.Account{Kind: "ics", Name: "Integration test (mirror)", Color: "#FF8800",
		Address: "me@school.test", Config: string(conf)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.RemoveAccount(context.Background(), a, true); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	pass := func() {
		t.Helper()
		if err := e.Pass(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	item := func(uid string) store.Item {
		t.Helper()
		it, err := st.Item(ctx, a.ID, uid)
		if err != nil {
			t.Fatalf("%s: %v", uid, err)
		}
		return it
	}
	davisCopy := func(uid string) (davis.Item, *ical.Calendar) {
		t.Helper()
		got, err := dc.Get(ctx, item(uid).Href)
		if err != nil {
			t.Fatalf("%s: %v", uid, err)
		}
		cal, err := ics.Parse(got.Data)
		if err != nil {
			t.Fatal(err)
		}
		return got, cal
	}
	// edit changes the Davis copy the way a client would (PUT with If-Match).
	edit := func(uid string, change func(ev *ical.Component)) {
		t.Helper()
		got, cal := davisCopy(uid)
		for _, ev := range ics.Events(cal) {
			change(ev)
		}
		if _, err := dc.Put(ctx, got.Href, ics.Encode(cal), got.ETag); err != nil {
			t.Fatal(err)
		}
	}

	// 1. First pass: three events mirrored.
	writeFeed(invitationEvent("Soutenance", "NEEDS-ACTION"), plainEvent, seriesEvent)
	pass()
	cols, err := dc.Calendars(ctx, p.Home)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cols {
		if davis.SamePath(c.Href, a.Collection) {
			found = c.DisplayName == "Integration test (mirror)" && strings.EqualFold(c.Color, "#FF8800") && strings.Join(c.Components, ",") == "VEVENT"
		}
	}
	if !found {
		t.Fatalf("mirror collection not made as asked: %+v", cols)
	}
	got0, inv := davisCopy("it-invitation@school.test")
	ev := ics.Events(inv)[0]
	if ics.Attendee(ev, e.Me) == nil || ics.Value(ev, PropSourceAttendee) != "mailto:me@school.test" ||
		!strings.Contains(got0.Data, "\r\nX-SOURCE-ACCOUNT:ics:me@school.test\r\n") {
		t.Fatalf("my attendee not rewritten:\n%s", ics.Encode(inv))
	}
	if p := ev.Props.Get(ical.PropDateTimeStart); p.Params.Get("TZID") != "Europe/Paris" || p.Value != "20261123T100000" {
		t.Fatalf("DTSTART %v", p)
	}
	if !strings.HasPrefix(ics.Value(ev, ical.PropURL), "obsidian://calendar-tasks?uid=it-invitation%40school.test&vault=test-vault") {
		t.Fatalf("URL %q", ics.Value(ev, ical.PropURL))
	}
	if _, series := davisCopy("it-series"); len(ics.Events(series)) != 2 {
		t.Fatal("override lost")
	}

	// 2. Nothing changed: nothing written.
	before := item("it-invitation@school.test").ETag
	pass()
	if item("it-invitation@school.test").ETag != before {
		t.Fatal("rewritten with nothing new")
	}

	// 3. I accept on the "phone": PARTSTAT changed, attendee parameters rewritten as iOS does, an alarm added.
	edit("it-invitation@school.test", func(ev *ical.Component) {
		att := ics.Attendee(ev, e.Me)
		att.Params.Set("PARTSTAT", "ACCEPTED")
		att.Params.Set("CUTYPE", "INDIVIDUAL")
		att.Params.Del("RSVP")
		alarm := ical.NewComponent(ical.CompAlarm)
		alarm.Props.SetText(ical.PropAction, "DISPLAY")
		alarm.Props.SetText(ical.PropDescription, "Reminder")
		alarm.Props.Set(&ical.Prop{Name: ical.PropTrigger, Params: ical.Params{}, Value: "-PT15M"})
		ev.Children = append(ev.Children, alarm)
	})
	pass()
	if got := item("it-invitation@school.test").Answer; got != "ACCEPTED" {
		t.Fatalf("answer not seen: %q", got)
	}
	_, inv = davisCopy("it-invitation@school.test")
	ev = ics.Events(inv)[0]
	if ics.Partstat(ics.Attendee(ev, e.Me)) != "ACCEPTED" || len(ev.Children) != 1 {
		t.Fatalf("answer or alarm lost:\n%s", ics.Encode(inv))
	}

	// 4. The organizer renames it; the source still says NEEDS-ACTION: my answer stays (in flight).
	writeFeed(invitationEvent("Soutenance (salle 12)", "NEEDS-ACTION"), plainEvent, seriesEvent)
	pass()
	_, inv = davisCopy("it-invitation@school.test")
	ev = ics.Events(inv)[0]
	if ics.Value(ev, ical.PropSummary) != "Soutenance (salle 12)" || ics.Partstat(ics.Attendee(ev, e.Me)) != "ACCEPTED" || len(ev.Children) != 1 {
		t.Fatalf("after the source's change:\n%s", ics.Encode(inv))
	}

	// 5. The source shows my answer: no longer in flight.
	writeFeed(invitationEvent("Soutenance (salle 12)", "ACCEPTED"), plainEvent, seriesEvent)
	pass()
	if got := item("it-invitation@school.test").Answer; got != "" {
		t.Fatalf("answer still in flight: %q", got)
	}

	// 6. A client renames a mirror event: the source wins.
	edit("it-plain", func(ev *ical.Component) { ev.Props.SetText(ical.PropSummary, "Renamed on the phone") })
	pass()
	if _, plain := davisCopy("it-plain"); ics.Value(ics.Events(plain)[0], ical.PropSummary) != "Plain" {
		t.Fatal("client edit kept")
	}

	// 7. Gone from the source: deleted on Davis.
	plainHref := item("it-plain").Href
	writeFeed(invitationEvent("Soutenance (salle 12)", "ACCEPTED"), seriesEvent)
	pass()
	if _, err := dc.Get(ctx, plainHref); !errors.Is(err, davis.ErrNotFound) {
		t.Fatalf("still on Davis: %v", err)
	}
	if _, err := st.Item(ctx, a.ID, "it-plain"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("still in the store")
	}

	// 8. Deleted on Davis: forgotten there, not brought back while the source doesn't change it.
	if err := dc.Delete(ctx, item("it-series").Href, ""); err != nil {
		t.Fatal(err)
	}
	pass()
	if it := item("it-series"); it.Href != "" {
		t.Fatalf("still known on Davis: %+v", it)
	}

	// 9. An empty feed is believed only the second time.
	writeFeed()
	pass()
	if _, err := st.Item(ctx, a.ID, "it-invitation@school.test"); err != nil {
		t.Fatal("deleted on the first empty listing")
	}
	pass()
	if _, err := st.Item(ctx, a.ID, "it-invitation@school.test"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("kept after a second empty listing")
	}

	// 10. The Davis sync token survives a restart of the passes (no full listing needed).
	acct, _ := st.Account(ctx, a.ID)
	if acct.DavisToken == "" || acct.SourceToken == "" || acct.LastError != "" || time.Since(acct.LastSync) > time.Minute {
		t.Fatalf("account state: %+v", acct)
	}
}
