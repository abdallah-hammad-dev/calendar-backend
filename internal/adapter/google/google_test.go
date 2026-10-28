package google

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

// Events as the Calendar API gives them (shapes from its reference; ids and contents made up).
const (
	invitation = `{"id":"evt1","status":"confirmed","created":"2026-10-20T08:00:00.000Z","updated":"2026-10-21T09:30:00.000Z",
	"summary":"Soutenance","description":"Salle B<br>Lien : <a href=\"https://example.com/doc\">doc</a>","location":"Bâtiment A",
	"organizer":{"email":"prof@school.fr","displayName":"Prof"},
	"start":{"dateTime":"2026-11-23T10:00:00+01:00","timeZone":"Europe/Paris"},"end":{"dateTime":"2026-11-23T11:00:00+01:00","timeZone":"Europe/Paris"},
	"iCalUID":"evt1@google.com","sequence":1,
	"attendees":[{"email":"prof@school.fr","displayName":"Prof","organizer":true,"responseStatus":"accepted"},
		{"email":"me@gmail.com","self":true,"responseStatus":"needsAction"},
		{"email":"c_salle@resource.calendar.google.com","displayName":"Salle B","resource":true,"responseStatus":"accepted"}],
	"hangoutLink":"https://meet.google.com/abc-defg-hij",
	"conferenceData":{"entryPoints":[{"entryPointType":"video","uri":"https://meet.google.com/abc-defg-hij","label":"meet.google.com/abc-defg-hij"},
		{"entryPointType":"phone","uri":"tel:+33-1-23-45-67-89","pin":"123456"},{"entryPointType":"more","uri":"https://tel.meet/abc"}],
		"conferenceSolution":{"key":{"type":"hangoutsMeet"},"name":"Google Meet"}},"eventType":"default"}`
	seriesMaster = `{"id":"ser1","status":"confirmed","updated":"2026-10-20T08:00:00.000Z","summary":"Sport",
	"organizer":{"email":"me@gmail.com","self":true},"iCalUID":"ser1@google.com",
	"start":{"dateTime":"2026-11-02T14:00:00+01:00","timeZone":"Europe/Paris"},"end":{"dateTime":"2026-11-02T15:00:00+01:00","timeZone":"Europe/Paris"},
	"recurrence":["RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=4"],"eventType":"default"}`
	seriesMoved = `{"id":"ser1_20261109T130000Z","status":"confirmed","updated":"2026-10-20T08:00:00.000Z","summary":"Sport (tard)",
	"organizer":{"email":"me@gmail.com","self":true},"iCalUID":"ser1@google.com","recurringEventId":"ser1",
	"originalStartTime":{"dateTime":"2026-11-09T14:00:00+01:00","timeZone":"Europe/Paris"},
	"start":{"dateTime":"2026-11-09T16:00:00+01:00","timeZone":"Europe/Paris"},"end":{"dateTime":"2026-11-09T17:00:00+01:00","timeZone":"Europe/Paris"}}`
	seriesCancelled = `{"id":"ser1_20261116T130000Z","status":"cancelled","recurringEventId":"ser1","iCalUID":"ser1@google.com",
	"originalStartTime":{"dateTime":"2026-11-16T14:00:00+01:00","timeZone":"Europe/Paris"}}`
	allDay = `{"id":"allday1","status":"confirmed","updated":"2026-10-20T08:00:00.000Z","summary":"Congé","iCalUID":"allday1@google.com",
	"organizer":{"email":"me@gmail.com","self":true},"start":{"date":"2026-11-24"},"end":{"date":"2026-11-25"},"transparency":"transparent"}`
	office = `{"id":"wl1","status":"confirmed","summary":"Bureau","iCalUID":"wl1@google.com","eventType":"workingLocation",
	"start":{"date":"2026-11-25"},"end":{"date":"2026-11-26"}}`
	deletedLongAgo = `{"id":"old1","status":"cancelled","iCalUID":"old1@google.com"}`
)

type fake struct {
	t       *testing.T
	pages   map[string]string // query key ("full", "full-2", "sync-1", "uid:…") → items JSON
	events  map[string]string // id → event JSON
	patched []string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/calendars/primary/events" {
		if q.Get("showDeleted") != "true" || q.Get("singleEvents") != "" {
			f.t.Errorf("listing without cancelled events, or expanded: %s", r.URL.RawQuery)
		}
		key := "full"
		switch {
		case q.Get("syncToken") == "expired":
			w.WriteHeader(http.StatusGone)
			w.Write([]byte(`{"error":{"code":410,"message":"Sync token is no longer valid, a full sync is required.","errors":[{"reason":"fullSyncRequired"}]}}`))
			return
		case q.Get("syncToken") != "":
			key = q.Get("syncToken")
		case q.Get("iCalUID") != "":
			key = "uid:" + q.Get("iCalUID")
		case q.Get("pageToken") != "":
			key = q.Get("pageToken")
		}
		body, ok := f.pages[key]
		if !ok {
			f.t.Errorf("unexpected listing %s", r.URL.RawQuery)
		}
		w.Write([]byte(body))
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/calendars/primary/events/")
	ev, ok := f.events[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":404,"message":"Not Found"}}`))
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Write([]byte(ev))
	case http.MethodPatch:
		if q.Get("sendUpdates") != "all" {
			f.t.Error("answer sent without telling the organizer")
		}
		b, _ := io.ReadAll(r.Body)
		f.patched = append(f.patched, id+" "+string(b))
		w.Write([]byte(ev))
	}
}

func setup(t *testing.T) (*Calendar, *fake) {
	f := &fake{t: t, events: map[string]string{"evt1": invitation, "ser1": seriesMaster}, pages: map[string]string{
		"full":   `{"timeZone":"Europe/Paris","nextPageToken":"full-2","items":[` + invitation + `,` + seriesMaster + `,` + seriesMoved + `]}`,
		"full-2": `{"timeZone":"Europe/Paris","nextSyncToken":"sync-1","items":[` + seriesCancelled + `,` + allDay + `,` + office + `,` + deletedLongAgo + `]}`,
		// Then: the invitation renamed, another occurrence of the series cancelled (given by id only), the all-day
		// event deleted (by id only).
		"sync-1": `{"timeZone":"Europe/Paris","nextSyncToken":"sync-2","items":[` + strings.Replace(invitation, "Soutenance", "Soutenance (salle 12)", 1) +
			`,{"id":"ser1_20261123T130000Z","status":"cancelled","recurringEventId":"ser1"},{"id":"allday1","status":"cancelled"}]}`,
		"uid:ser1@google.com": `{"items":[` + seriesMaster + `,` + seriesMoved + `,` + seriesCancelled +
			`,{"id":"ser1_20261123T130000Z","status":"cancelled","recurringEventId":"ser1","iCalUID":"ser1@google.com","originalStartTime":{"dateTime":"2026-11-23T14:00:00+01:00","timeZone":"Europe/Paris"}}]}`,
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Calendar{HTTP: srv.Client(), Base: srv.URL, Address: "me@gmail.com"}, f
}

func byUID(t *testing.T, page adapter.Page, uid string) adapter.Change {
	t.Helper()
	for _, ch := range page.Changes {
		if ch.UID == uid {
			return ch
		}
	}
	t.Fatalf("no change for %s in %+v", uid, page)
	return adapter.Change{}
}

func prop(ev *ical.Component, name string) *ical.Prop {
	if p := ev.Props.Get(name); p != nil {
		return p
	}
	return &ical.Prop{Params: ical.Params{}}
}

func TestFullListing(t *testing.T) {
	c, _ := setup(t)
	page, err := c.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !page.Full || page.Token != "sync-1" || len(page.Changes) != 3 {
		t.Fatalf("full %v, token %q, %d changes (want the invitation, the series, the all-day event)", page.Full, page.Token, len(page.Changes))
	}

	inv := byUID(t, page, "evt1@google.com")
	if inv.SourceID != "evt1" || inv.Me != "" {
		t.Fatalf("%+v", inv)
	}
	ev := ics.Events(inv.Calendar)[0]
	if p := prop(ev, "DTSTART"); p.Value != "20261123T100000" || p.Params.Get("TZID") != "Europe/Paris" {
		t.Fatalf("DTSTART %+v", p)
	}
	me := ics.Attendee(ev, "me@gmail.com")
	if me == nil || ics.Partstat(me) != "NEEDS-ACTION" || me.Params.Get("RSVP") != "TRUE" {
		t.Fatalf("my attendee: %+v", me)
	}
	if room := ics.Attendee(ev, "c_salle@resource.calendar.google.com"); room == nil || room.Params.Get("CUTYPE") != "RESOURCE" {
		t.Fatal("room not marked as a resource")
	}
	if prof := ics.Attendee(ev, "prof@school.fr"); prof == nil || prof.Params.Get("ROLE") != "CHAIR" || prof.Params.Get("CN") != "Prof" {
		t.Fatalf("organizer's attendee: %+v", prof)
	}
	if ics.Value(ev, "ORGANIZER") != "mailto:prof@school.fr" || ics.Value(ev, "SEQUENCE") != "1" ||
		ics.Value(ev, "DTSTAMP") != "20261021T093000Z" {
		t.Fatalf("ORGANIZER / SEQUENCE / DTSTAMP:\n%s", ics.Encode(inv.Calendar))
	}
	var video, phone string
	for _, p := range ev.Props["CONFERENCE"] {
		switch p.Params.Get("FEATURE") {
		case "VIDEO":
			video = p.Value + " " + p.Params.Get("LABEL")
		case "PHONE":
			phone = p.Value
		}
	}
	if video != "https://meet.google.com/abc-defg-hij Google Meet" || phone != "tel:+33-1-23-45-67-89" || len(ev.Props["CONFERENCE"]) != 2 {
		t.Fatalf("CONFERENCE: %+v", ev.Props["CONFERENCE"])
	}
	desc, _ := ev.Props.Text("DESCRIPTION")
	if desc != "Salle B\nLien : doc (https://example.com/doc)\n\nGoogle Meet: https://meet.google.com/abc-defg-hij" {
		t.Fatalf("DESCRIPTION %q", desc)
	}
	if loc, _ := ev.Props.Text("LOCATION"); loc != "Bâtiment A" {
		t.Fatal(loc)
	}

	series := byUID(t, page, "ser1@google.com")
	evs := ics.Events(series.Calendar)
	if series.SourceID != "ser1" || len(evs) != 2 {
		t.Fatalf("series: %s\n%s", series.SourceID, ics.Encode(series.Calendar))
	}
	master, moved := evs[0], evs[1]
	if ics.Value(master, "RRULE") != "FREQ=WEEKLY;BYDAY=MO;COUNT=4" || ev.Props.Get("ORGANIZER") == nil || master.Props.Get("ORGANIZER") != nil {
		t.Fatalf("master (no attendees: no ORGANIZER):\n%s", ics.Encode(series.Calendar))
	}
	if ex := prop(master, "EXDATE"); ex.Value != "20261116T140000" || ex.Params.Get("TZID") != "Europe/Paris" {
		t.Fatalf("EXDATE %+v", ex)
	}
	if rid := prop(moved, "RECURRENCE-ID"); rid.Value != "20261109T140000" || rid.Params.Get("TZID") != "Europe/Paris" {
		t.Fatalf("RECURRENCE-ID %+v", rid)
	}
	if ics.Value(moved, "DTSTART") != "20261109T160000" || ics.Value(moved, "SUMMARY") != "Sport (tard)" {
		t.Fatalf("override:\n%s", ics.Encode(series.Calendar))
	}

	day := ics.Events(byUID(t, page, "allday1@google.com").Calendar)[0]
	if p := prop(day, "DTSTART"); p.Value != "20261124" || p.Params.Get("VALUE") != "DATE" || ics.Value(day, "TRANSP") != "TRANSPARENT" {
		t.Fatalf("all day:\n%+v", day.Props)
	}

	// What the mirror gets reads back as iCalendar.
	for _, ch := range page.Changes {
		if _, err := ics.Parse(ics.Encode(ch.Calendar)); err != nil {
			t.Fatalf("%s: %v", ch.UID, err)
		}
	}
}

func TestIncrementalListing(t *testing.T) {
	c, _ := setup(t)
	page, err := c.List(context.Background(), "sync-1")
	if err != nil {
		t.Fatal(err)
	}
	if page.Full || page.Token != "sync-2" || len(page.Changes) != 3 {
		t.Fatalf("full %v, token %q, changes %+v", page.Full, page.Token, page.Changes)
	}
	if s, _ := ics.Events(byUID(t, page, "evt1@google.com").Calendar)[0].Props.Text("SUMMARY"); s != "Soutenance (salle 12)" {
		t.Fatal(s)
	}
	// The cancelled occurrence came by id only: the series is read again, whole.
	series := byUID(t, page, "ser1@google.com")
	if n := len(ics.Events(series.Calendar)[0].Props["EXDATE"]); n != 2 || len(ics.Events(series.Calendar)) != 2 {
		t.Fatalf("%d EXDATEs:\n%s", n, ics.Encode(series.Calendar))
	}
	// The all-day event was deleted by id only: the mirror finds it by its source id.
	if del := page.Changes[1]; !del.Deleted || del.UID != "" || del.SourceID != "allday1" {
		t.Fatalf("deletion: %+v", page.Changes)
	}
}

func TestExpiredToken(t *testing.T) {
	c, _ := setup(t)
	page, err := c.List(context.Background(), "expired")
	if err != nil || !page.Full || page.Token != "sync-1" {
		t.Fatalf("%+v %v", page, err)
	}
}

func TestRSVP(t *testing.T) {
	c, f := setup(t)
	if err := c.RSVP(context.Background(), "evt1", "ACCEPTED"); err != nil {
		t.Fatal(err)
	}
	if len(f.patched) != 1 {
		t.Fatal(f.patched)
	}
	id, body, _ := strings.Cut(f.patched[0], " ")
	var got map[string]any
	json.Unmarshal([]byte(body), &got)
	want := map[string]any{"attendeesOmitted": true, "attendees": []any{map[string]any{"email": "me@gmail.com", "responseStatus": "accepted"}}}
	if id != "evt1" || !jsonEqual(got, want) {
		t.Fatalf("%s %s", id, body)
	}

	var refused *adapter.Refused
	if err := c.RSVP(context.Background(), "gone", "DECLINED"); !errors.As(err, &refused) {
		t.Fatalf("an event gone: %v", err)
	}
	// My own event: I'm not an attendee.
	if err := c.RSVP(context.Background(), "ser1", "ACCEPTED"); !errors.As(err, &refused) {
		t.Fatalf("not invited: %v", err)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestRefused(t *testing.T) {
	var r *adapter.Refused
	for _, c := range []struct {
		err     error
		refused bool
	}{
		{&apiError{Status: 404}, true},
		{&apiError{Status: 403, Message: "Forbidden"}, true},
		{&apiError{Status: 403, Reasons: []string{"rateLimitExceeded"}}, false},
		{&apiError{Status: 503}, false},
		{errors.New("network down"), false},
	} {
		if got := errors.As(refused(c.err), &r); got != c.refused {
			t.Errorf("%v: refused %v", c.err, got)
		}
	}
}

// A Meet without a place: the link is the place (the iPhone's "Location or Video Call"), not added to the notes.
func TestMeetWithoutPlace(t *testing.T) {
	var e event
	json.Unmarshal([]byte(strings.Replace(invitation, `"location":"Bâtiment A",`, "", 1)), &e)
	ev := (&Calendar{Address: "me@gmail.com"}).vevent(e, "Europe/Paris")
	place, _ := ev.Props.Text("LOCATION")
	desc, _ := ev.Props.Text("DESCRIPTION")
	if place != "https://meet.google.com/abc-defg-hij" || strings.Contains(desc, "meet.google.com") {
		t.Fatalf("place %q, notes %q", place, desc)
	}
}
