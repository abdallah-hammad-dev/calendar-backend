package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

// Events as Graph gives them (shapes from its reference; ids and contents made up), times in UTC as asked.
const (
	teams = `{"id":"AAS1","type":"singleInstance","iCalUId":"040000008200E00074C5B7101A82E0080000000011",
	"subject":"Réunion projet","body":{"contentType":"text","content":"Bonjour,\r\n\r\n____\r\nRéunion Microsoft Teams\r\nRejoindre : https://teams.microsoft.com/l/meetup-join/19%3ameeting_x\r\n"},
	"start":{"dateTime":"2026-11-23T09:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-11-23T10:00:00.0000000","timeZone":"UTC"},
	"isAllDay":false,"originalStartTimeZone":"Romance Standard Time","location":{"displayName":"Réunion Microsoft Teams"},
	"organizer":{"emailAddress":{"name":"Prof","address":"prof@school.fr"}},
	"attendees":[{"type":"required","status":{"response":"none"},"emailAddress":{"name":"Moi","address":"Me@School.fr"}},
		{"type":"optional","status":{"response":"accepted"},"emailAddress":{"name":"Camille","address":"camille@school.fr"}},
		{"type":"resource","status":{"response":"accepted"},"emailAddress":{"name":"Salle B","address":"salleb@school.fr"}}],
	"isOrganizer":false,"isCancelled":false,"responseStatus":{"response":"notResponded"},"showAs":"tentative",
	"onlineMeeting":{"joinUrl":"https://teams.microsoft.com/l/meetup-join/19%3ameeting_x"},"onlineMeetingProvider":"teamsForBusiness",
	"lastModifiedDateTime":"2026-10-21T09:30:00.1234567Z","createdDateTime":"2026-10-20T08:00:00Z"}`
	viaGroup = `{"id":"AAG1","type":"singleInstance","iCalUId":"group-uid","subject":"Conseil de classe",
	"body":{"contentType":"html","content":"<html><body><p>Ordre du jour</p></body></html>"},
	"start":{"dateTime":"2026-11-25T15:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-11-25T16:00:00.0000000","timeZone":"UTC"},
	"originalStartTimeZone":"Europe/Paris","organizer":{"emailAddress":{"name":"Direction","address":"direction@school.fr"}},
	"attendees":[{"type":"required","status":{"response":"none"},"emailAddress":{"name":"Profs","address":"profs@school.fr"}}],
	"isOrganizer":false,"responseStatus":{"response":"accepted"},"lastModifiedDateTime":"2026-10-21T09:30:00Z"}`
	allDay = `{"id":"AAD1","type":"singleInstance","iCalUId":"allday-uid","subject":"Congé","isAllDay":true,
	"start":{"dateTime":"2026-11-24T00:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-11-25T00:00:00.0000000","timeZone":"UTC"},
	"originalStartTimeZone":"Romance Standard Time","organizer":{"emailAddress":{"address":"me@school.fr"}},"attendees":[],
	"isOrganizer":true,"responseStatus":{"response":"organizer"},"showAs":"free","lastModifiedDateTime":"2026-10-20T08:00:00Z"}`
	occurrence = `{"id":"AAO1","type":"occurrence","seriesMasterId":"AAM1","iCalUId":"occ-uid-1","subject":"Sport",
	"start":{"dateTime":"2026-11-02T13:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-11-02T14:00:00.0000000","timeZone":"UTC"}}`
	exception = `{"id":"AAO2","type":"exception","seriesMasterId":"AAM1","iCalUId":"occ-uid-2","subject":"Sport (tard)",
	"start":{"dateTime":"2026-11-09T15:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-11-09T16:00:00.0000000","timeZone":"UTC"}}`
)

// seriesMaster is the master read by id, with its exceptions and cancelled occurrences.
func seriesMaster(cancelled ...string) string {
	oids, _ := json.Marshal(cancelled)
	return `{"id":"AAM1","type":"seriesMaster","iCalUId":"series-uid","subject":"Sport",
	"start":{"dateTime":"2026-11-02T13:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-11-02T14:00:00.0000000","timeZone":"UTC"},
	"originalStartTimeZone":"Romance Standard Time","isOrganizer":true,"attendees":[],"responseStatus":{"response":"organizer"},
	"organizer":{"emailAddress":{"address":"me@school.fr"}},"lastModifiedDateTime":"2026-10-20T08:00:00Z",
	"recurrence":{"pattern":{"type":"weekly","interval":1,"daysOfWeek":["monday"],"firstDayOfWeek":"monday"},
		"range":{"type":"endDate","startDate":"2026-11-02","endDate":"2026-11-30","recurrenceTimeZone":"Romance Standard Time"}},
	"exceptionOccurrences":[{"id":"AAO2","type":"exception","iCalUId":"occ-uid-2","subject":"Sport (tard)","originalStart":"2026-11-09T13:00:00Z",
		"start":{"dateTime":"2026-11-09T15:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-11-09T16:00:00.0000000","timeZone":"UTC"},
		"originalStartTimeZone":"Romance Standard Time","isOrganizer":true,"attendees":[],"lastModifiedDateTime":"2026-10-20T08:00:00Z"}],
	"cancelledOccurrences":` + string(oids) + `}`
}

type fake struct {
	t        *testing.T
	url      string
	master   string
	rounds   []string // ranges asked for by new rounds
	answered []string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Values("Prefer")[0] != `outlook.timezone="UTC"` {
		f.t.Errorf("times not asked in UTC: %v", r.Header.Values("Prefer"))
	}
	w.Header().Set("Content-Type", "application/json")
	q := r.URL.Query()
	page := func(items []string, next, delta string) {
		var link string
		if next != "" {
			link = `"@odata.nextLink":"` + f.url + `/me/calendarView/delta?$skiptoken=` + next + `",`
		} else {
			link = `"@odata.deltaLink":"` + f.url + `/me/calendarView/delta?$deltatoken=` + delta + `",`
		}
		w.Write([]byte(`{` + link + `"value":[` + strings.Join(items, ",") + `]}`))
	}
	switch {
	case r.URL.Path == "/me/calendarView/delta":
		switch {
		case q.Get("startDateTime") != "":
			f.rounds = append(f.rounds, q.Get("startDateTime")+" "+q.Get("endDateTime"))
			page([]string{teams, viaGroup, occurrence}, "p2", "")
		case q.Get("$skiptoken") == "p2":
			page([]string{exception, allDay}, "", "d1")
		case q.Get("$deltatoken") == "d1":
			// An occurrence of the series deleted, the Teams meeting deleted, something never seen, the all-day
			// event renamed.
			f.master = seriesMaster("OID.AAM1.2026-11-16", "OID.AAM1.2026-11-02")
			page([]string{`{"id":"AAO1","@removed":{"reason":"deleted"}}`, `{"id":"AAS1","@removed":{"reason":"deleted"}}`,
				`{"id":"never-seen","@removed":{"reason":"deleted"}}`, strings.Replace(allDay, "Congé", "Congé (posé)", 1)}, "", "d2")
		case q.Get("$deltatoken") == "lost":
			w.WriteHeader(http.StatusGone)
			w.Write([]byte(`{"error":{"code":"SyncStateNotFound","message":"The sync state is no longer available."}}`))
		default:
			f.t.Errorf("unexpected delta request %s", r.URL.RawQuery)
		}
	case r.URL.Path == "/me/events/AAM1":
		if q.Get("$expand") != "exceptionOccurrences" || !strings.Contains(q.Get("$select"), "cancelledOccurrences") {
			f.t.Errorf("series read without its exceptions: %s", r.URL.RawQuery)
		}
		w.Write([]byte(f.master))
	case r.URL.Path == "/me/calendar":
		w.Write([]byte(`{"owner":{"name":"Moi","address":"me@school.fr"}}`))
	case r.URL.Path == "/me/events/AAS1" && r.Method == http.MethodGet:
		w.Write([]byte(`{"isOrganizer":false,"responseRequested":false}`))
	case r.URL.Path == "/me/events/AAS1/tentativelyAccept":
		b, _ := io.ReadAll(r.Body)
		f.answered = append(f.answered, string(b))
		w.WriteHeader(http.StatusAccepted)
	case r.URL.Path == "/me/events/AAD1":
		w.Write([]byte(`{"isOrganizer":true}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"ErrorItemNotFound","message":"The specified object was not found in the store."}}`))
	}
}

var now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*Calendar, *fake) {
	f := &fake{t: t, master: seriesMaster("OID.AAM1.2026-11-16")}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return &Calendar{HTTP: srv.Client(), Base: srv.URL, Address: "me@school.fr", Now: func() time.Time { return now }}, f
}

func byUID(t *testing.T, page adapter.Page, uid string) adapter.Change {
	t.Helper()
	for _, ch := range page.Changes {
		if ch.UID == uid {
			return ch
		}
	}
	t.Fatalf("no change for %s in %+v", uid, page.Changes)
	return adapter.Change{}
}

func prop(ev *ical.Component, name string) *ical.Prop {
	if p := ev.Props.Get(name); p != nil {
		return p
	}
	return &ical.Prop{Params: ical.Params{}}
}

func TestFullRound(t *testing.T) {
	c, f := setup(t)
	page, err := c.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !page.Full || len(page.Changes) != 4 || len(f.rounds) != 1 {
		t.Fatalf("full %v, %d changes (Teams meeting, group meeting, series, all day), rounds %v", page.Full, len(page.Changes), f.rounds)
	}
	if f.rounds[0] != "2025-10-23T00:00:00Z 2028-04-23T00:00:00Z" {
		t.Fatalf("range %s", f.rounds[0])
	}

	ev := ics.Events(byUID(t, page, "040000008200E00074C5B7101A82E0080000000011").Calendar)[0]
	if p := prop(ev, "DTSTART"); p.Value != "20261123T100000" || p.Params.Get("TZID") != "Europe/Paris" {
		t.Fatalf("DTSTART in its own zone: %+v", p)
	}
	me := ics.Attendee(ev, "me@school.fr")
	if me == nil || ics.Partstat(me) != "NEEDS-ACTION" || me.Params.Get("RSVP") != "TRUE" {
		t.Fatalf("my attendee: %+v", me)
	}
	if a := ics.Attendee(ev, "camille@school.fr"); a == nil || a.Params.Get("ROLE") != "OPT-PARTICIPANT" || ics.Partstat(a) != "ACCEPTED" {
		t.Fatalf("optional attendee: %+v", a)
	}
	if a := ics.Attendee(ev, "salleb@school.fr"); a == nil || a.Params.Get("CUTYPE") != "RESOURCE" {
		t.Fatal("room not a resource")
	}
	if ics.Value(ev, "ORGANIZER") != "mailto:prof@school.fr" || ics.Value(ev, "DTSTAMP") != "20261021T093000Z" {
		t.Fatalf("ORGANIZER / DTSTAMP:\n%s", ics.Encode(byUID(t, page, "040000008200E00074C5B7101A82E0080000000011").Calendar))
	}
	if p := prop(ev, "CONFERENCE"); p.Value != "https://teams.microsoft.com/l/meetup-join/19%3ameeting_x" ||
		p.Params.Get("FEATURE") != "VIDEO" || p.Params.Get("LABEL") != "Microsoft Teams" {
		t.Fatalf("CONFERENCE %+v", p)
	}
	if desc, _ := ev.Props.Text("DESCRIPTION"); strings.Count(desc, "teams.microsoft.com") != 1 {
		t.Fatalf("the join link, already in the notes, added again: %q", desc)
	}

	// Invited through a group: I'm added as an attendee with my answer.
	grp := ics.Events(byUID(t, page, "group-uid").Calendar)[0]
	if me := ics.Attendee(grp, "me@school.fr"); me == nil || ics.Partstat(me) != "ACCEPTED" {
		t.Fatalf("not added through the group:\n%+v", grp.Props["ATTENDEE"])
	}
	if desc, _ := grp.Props.Text("DESCRIPTION"); desc != "Ordre du jour" {
		t.Fatalf("an HTML body: %q", desc)
	}

	series := byUID(t, page, "series-uid")
	evs := ics.Events(series.Calendar)
	if series.SourceID != "AAM1" || len(evs) != 2 {
		t.Fatalf("series:\n%s", ics.Encode(series.Calendar))
	}
	master, moved := evs[0], evs[1]
	if p := prop(master, "DTSTART"); p.Value != "20261102T140000" || p.Params.Get("TZID") != "Europe/Paris" {
		t.Fatalf("master DTSTART %+v", p)
	}
	if got := ics.Value(master, "RRULE"); got != "FREQ=WEEKLY;BYDAY=MO;WKST=MO;UNTIL=20261130T225959Z" {
		t.Fatalf("RRULE %s", got)
	}
	if p := prop(master, "EXDATE"); p.Value != "20261116T140000" || p.Params.Get("TZID") != "Europe/Paris" {
		t.Fatalf("EXDATE %+v", p)
	}
	if master.Props.Get("ORGANIZER") != nil {
		t.Fatal("my own series has an ORGANIZER")
	}
	if p := prop(moved, "RECURRENCE-ID"); p.Value != "20261109T140000" || p.Params.Get("TZID") != "Europe/Paris" ||
		ics.Value(moved, "DTSTART") != "20261109T160000" {
		t.Fatalf("override:\n%s", ics.Encode(series.Calendar))
	}

	day := ics.Events(byUID(t, page, "allday-uid").Calendar)[0]
	if p := prop(day, "DTSTART"); p.Value != "20261124" || p.Params.Get("VALUE") != "DATE" ||
		ics.Value(day, "DTEND") != "20261125" || ics.Value(day, "TRANSP") != "TRANSPARENT" {
		t.Fatalf("all day:\n%+v", day.Props)
	}

	var st state
	json.Unmarshal([]byte(page.Token), &st)
	if !strings.HasSuffix(st.Delta, "$deltatoken=d1") || st.IDs["AAO1"] != "series-uid" || st.IDs["AAO2"] != "series-uid" ||
		st.Series["series-uid"] != "AAM1" || st.IDs["AAS1"] == "" {
		t.Fatalf("token: %+v", st)
	}

	// The next round: only what changed.
	next, err := c.List(context.Background(), page.Token)
	if err != nil {
		t.Fatal(err)
	}
	if next.Full || len(next.Changes) != 3 {
		t.Fatalf("full %v, changes %+v", next.Full, next.Changes)
	}
	if del := byUID(t, next, "040000008200E00074C5B7101A82E0080000000011"); !del.Deleted || del.SourceID != "AAS1" {
		t.Fatalf("Teams meeting deletion: %+v", del)
	}
	if n := len(ics.Events(byUID(t, next, "series-uid").Calendar)[0].Props["EXDATE"]); n != 2 {
		t.Fatalf("the deleted occurrence didn't have the series read again: %d EXDATEs", n)
	}
	if s, _ := ics.Events(byUID(t, next, "allday-uid").Calendar)[0].Props.Text("SUMMARY"); s != "Congé (posé)" {
		t.Fatal(s)
	}

	// Graph lost the round: a new one, same range start.
	json.Unmarshal([]byte(next.Token), &st)
	st.Delta = f.url + "/me/calendarView/delta?$deltatoken=lost"
	lost, _ := json.Marshal(st)
	again, err := c.List(context.Background(), string(lost))
	if err != nil || !again.Full || len(f.rounds) != 2 || f.rounds[1] != f.rounds[0] {
		t.Fatalf("after a lost round: full %v, rounds %v, %v", again.Full, f.rounds, err)
	}

	// Less than a year left ahead: a new round over a range moved ahead, still from the first listing's start.
	c.Now = func() time.Time { return now.AddDate(0, 7, 0) }
	moved2, err := c.List(context.Background(), again.Token)
	if err != nil || !moved2.Full || len(f.rounds) != 3 || !strings.HasPrefix(f.rounds[2], "2025-10-23T00:00:00Z 2028-11-2") {
		t.Fatalf("renewal: full %v, rounds %v, %v", moved2.Full, f.rounds, err)
	}
}

func TestLinksStayOnGraph(t *testing.T) {
	c, _ := setup(t)
	st, _ := json.Marshal(state{Delta: "https://attacker.example/steal", To: now.AddDate(2, 0, 0), From: now})
	if _, err := c.List(context.Background(), string(st)); err == nil || !strings.Contains(err.Error(), "outside Graph") {
		t.Fatalf("followed a link outside Graph: %v", err)
	}
}

func TestOwnerAndRSVP(t *testing.T) {
	c, f := setup(t)
	if addr, err := c.Owner(context.Background()); err != nil || addr != "me@school.fr" {
		t.Fatal(addr, err)
	}
	if err := c.RSVP(context.Background(), "AAS1", "TENTATIVE"); err != nil {
		t.Fatal(err)
	}
	if len(f.answered) != 1 || f.answered[0] != `{"sendResponse":false}` {
		t.Fatalf("answer sent: %v (the organizer asked for no answer)", f.answered)
	}
	var r *adapter.Refused
	for _, c2 := range []struct{ id, partstat string }{{"AAD1", "ACCEPTED"}, {"gone", "DECLINED"}, {"AAS1", "NEEDS-ACTION"}} {
		if err := c.RSVP(context.Background(), c2.id, c2.partstat); !errors.As(err, &r) {
			t.Errorf("%s %s: %v", c2.id, c2.partstat, err)
		}
	}
}

func TestRRule(t *testing.T) {
	paris := ics.Zone("Europe/Paris")
	for _, c := range []struct {
		pattern, rng string
		allDay       bool
		want         string
	}{
		{`{"type":"daily","interval":2}`, `{"type":"numbered","numberOfOccurrences":10}`, false, "FREQ=DAILY;INTERVAL=2;COUNT=10"},
		{`{"type":"absoluteMonthly","interval":1,"dayOfMonth":15}`, `{"type":"noEnd"}`, false, "FREQ=MONTHLY;BYMONTHDAY=15"},
		{`{"type":"relativeMonthly","interval":1,"daysOfWeek":["monday","tuesday","wednesday","thursday","friday"],"index":"last"}`,
			`{"type":"noEnd"}`, false, "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1"},
		{`{"type":"absoluteYearly","interval":1,"month":3,"dayOfMonth":8}`, `{"type":"endDate","endDate":"2030-03-08"}`, true,
			"FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=8;UNTIL=20300308"},
		{`{"type":"relativeYearly","interval":1,"month":11,"daysOfWeek":["thursday"],"index":"fourth"}`, `{"type":"noEnd"}`, false,
			"FREQ=YEARLY;BYMONTH=11;BYDAY=TH;BYSETPOS=4"},
		{`{"type":"weekly","interval":2,"daysOfWeek":["tuesday","thursday"],"firstDayOfWeek":"sunday"}`,
			`{"type":"endDate","endDate":"2027-06-30"}`, false, "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU,TH;WKST=SU;UNTIL=20270630T215959Z"},
	} {
		var r recurrence
		json.Unmarshal([]byte(`{"pattern":`+c.pattern+`,"range":`+c.rng+`}`), &r)
		if got := rrule(r, event{IsAllDay: c.allDay}, paris); got != c.want {
			t.Errorf("%s %s:\n got %s\nwant %s", c.pattern, c.rng, got, c.want)
		}
	}
}

func TestWindowsZones(t *testing.T) {
	if zoneOf("Romance Standard Time", nil).String() != "Europe/Paris" || zoneOf("Europe/Paris", nil).String() != "Europe/Paris" {
		t.Fatal("zone not mapped")
	}
	if zoneOf("UTC", nil) != nil || zoneOf("tzone://Microsoft/Custom", nil) != nil {
		t.Fatal("UTC or a custom zone mapped to a zone")
	}
}
