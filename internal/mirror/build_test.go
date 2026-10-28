package mirror

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

func crlf(s string) string { return strings.ReplaceAll(strings.TrimLeft(s, "\n"), "\n", "\r\n") }

func parse(t *testing.T, s string) *ical.Calendar {
	t.Helper()
	cal, err := ics.Parse(crlf(s))
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

var paris, _ = time.LoadLocation("Europe/Paris")

var target = Target{Account: "google:me@gmail.com", Me: "abd@bench.test", Vault: "Mon coffre", Loc: paris}

// A recurring invitation with one moved occurrence, as an ICS feed or an adapter gives it.
const source = `
BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Google Inc//Google Calendar 70.9054//EN
METHOD:PUBLISH
BEGIN:VEVENT
UID:abc123@google.com
DTSTAMP:20261020T080000Z
SUMMARY:Cours réseau
DTSTART:20261023T090000Z
DTEND:20261023T100000Z
RRULE:FREQ=WEEKLY;COUNT=4
ORGANIZER;CN=Prof:mailto:prof@school.fr
ATTENDEE;CN=Prof;PARTSTAT=ACCEPTED:mailto:prof@school.fr
ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:me@gmail.com
URL:https://meet.google.com/abc-defg-hij
END:VEVENT
BEGIN:VEVENT
UID:abc123@google.com
RECURRENCE-ID:20261030T090000Z
DTSTAMP:20261020T080000Z
SUMMARY:Cours réseau (salle B)
DTSTART:20261030T100000Z
DTEND:20261030T110000Z
ORGANIZER;CN=Prof:mailto:prof@school.fr
ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:me@gmail.com
END:VEVENT
END:VCALENDAR
`

func TestBase(t *testing.T) {
	out := Base(parse(t, source), "me@gmail.com", "", target)
	if out.Props.Get(ical.PropMethod) != nil {
		t.Fatal("METHOD kept: a CalDAV object can't have one")
	}
	if got := ics.Value(out.Component, ical.PropProductID); got != ics.ProdID {
		t.Fatalf("PRODID = %q", got)
	}
	evs := ics.Events(out)
	if len(evs) != 2 {
		t.Fatalf("%d events", len(evs))
	}
	for _, ev := range evs {
		att := ics.Attendee(ev, "abd@bench.test")
		if att == nil {
			t.Fatalf("%s: my ATTENDEE isn't the Davis address", ics.RecurrenceID(ev))
		}
		if att.Params.Get("RSVP") != "TRUE" || ics.Partstat(att) != "NEEDS-ACTION" {
			t.Fatalf("source parameters lost: %v", att.Params)
		}
		if ics.Attendee(ev, "me@gmail.com") != nil {
			t.Fatal("the invited address is still an ATTENDEE")
		}
		if got := ics.Value(ev, PropSourceAttendee); got != "mailto:me@gmail.com" {
			t.Fatalf("X-CALTASKS-SOURCE-ATTENDEE = %q", got)
		}
		if got := ics.Value(ev, PropSourceAccount); got != "google:me@gmail.com" {
			t.Fatalf("X-CALTASKS-SOURCE-ACCOUNT = %q", got)
		}
		if got := ics.Value(ev, ical.PropURL); got != "obsidian://calendar-tasks?uid=abc123%40google.com&vault=Mon%20coffre" {
			t.Fatalf("URL = %q", got)
		}
		if got := ics.Value(ev, ical.PropOrganizer); got != "mailto:prof@school.fr" {
			t.Fatalf("ORGANIZER = %q", got)
		}
	}
	master, moved := evs[0], evs[1]
	if desc, _ := master.Props.Text(ical.PropDescription); desc != "https://meet.google.com/abc-defg-hij" {
		t.Fatalf("the source's URL should go to the notes: %q", desc)
	}
	if p := master.Props.Get(ical.PropDateTimeStart); p.Value != "20261023T090000Z" {
		t.Fatalf("recurring master in UTC changed: %v", p)
	}
	if p := moved.Props.Get(ical.PropDateTimeStart); p.Value != "20261030T110000" || p.Params.Get("TZID") != "Europe/Paris" {
		t.Fatalf("override DTSTART = %v", p)
	}
	if got := ics.RecurrenceID(moved); got != "20261030T090000Z" {
		t.Fatalf("RECURRENCE-ID changed: %q", got)
	}
	if out.Children[0].Name != ical.CompTimezone || ics.Value(out.Children[0], "TZID") != "Europe/Paris" {
		t.Fatal("no VTIMEZONE first")
	}
}

func TestBaseAnswer(t *testing.T) {
	out := Base(parse(t, source), "me@gmail.com", "TENTATIVE", target)
	if got := ics.MyPartstat(out, "abd@bench.test"); got != "TENTATIVE" {
		t.Fatalf("answer not kept: %q", got)
	}
	for _, ev := range ics.Events(out) {
		if got := ics.Partstat(ics.Attendee(ev, "abd@bench.test")); got != "TENTATIVE" {
			t.Fatalf("%q: %s", ics.RecurrenceID(ev), got)
		}
	}
}

func TestBaseOrganizerIsMe(t *testing.T) {
	src := parse(t, `
BEGIN:VCALENDAR
VERSION:2.0
PRODID:x
BEGIN:VEVENT
UID:mine
DTSTAMP:20261020T080000Z
DTSTART;TZID=Europe/Paris:20261023T110000
ORGANIZER:mailto:me@gmail.com
ATTENDEE;PARTSTAT=ACCEPTED:mailto:me@gmail.com
ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:friend@example.com
END:VEVENT
END:VCALENDAR
`)
	ev := ics.Events(Base(src, "me@gmail.com", "", target))[0]
	if ics.Attendee(ev, "abd@bench.test") != nil || ev.Props.Get(PropSourceAttendee) != nil {
		t.Fatal("my own event was turned into an invitation")
	}
	if ics.Value(ev, PropSourceAccount) == "" {
		t.Fatal("still a mirror event")
	}
}

func TestMerge(t *testing.T) {
	existing := Base(parse(t, source), "me@gmail.com", "", target)
	alarm := ical.NewComponent(ical.CompAlarm)
	alarm.Props.SetText(ical.PropAction, "DISPLAY")
	master := ics.Events(existing)[0]
	master.Children = append(master.Children, alarm)
	master.Props.SetText("X-APPLE-TRAVEL-ADVISORY-BEHAVIOR", "AUTOMATIC")
	master.Props.SetText(PropRSVPState, "confirmed")
	master.Props.SetText(ical.PropSummary, "Renamed on the phone")
	existing, err := ics.Parse(ics.Encode(existing))
	if err != nil {
		t.Fatal(err)
	}

	merged := Merge(Base(parse(t, source), "me@gmail.com", "", target), existing)
	m := ics.Events(merged)[0]
	if got := ics.Value(m, ical.PropSummary); got != "Cours réseau" {
		t.Fatalf("the source's SUMMARY must win: %q", got)
	}
	if len(m.Children) != 1 || m.Children[0].Name != ical.CompAlarm {
		t.Fatal("alarm lost")
	}
	if ics.Value(m, "X-APPLE-TRAVEL-ADVISORY-BEHAVIOR") != "AUTOMATIC" {
		t.Fatal("client property lost")
	}
	if m.Props.Get(PropRSVPState) != nil {
		t.Fatal("the relay's state carried from the copy: it is the store's to write")
	}
	if len(ics.Events(merged)[1].Children) != 0 {
		t.Fatal("alarm given to the wrong occurrence")
	}
}

func TestSetRSVPState(t *testing.T) {
	cal := Base(parse(t, source), "me@gmail.com", "", target)
	SetRSVPState(cal, target.Me, "failed:gone")
	for _, ev := range ics.Events(cal) {
		if v, _ := ev.Props.Text(PropRSVPState); v != "failed:gone" {
			t.Fatalf("%s: %q", ics.RecurrenceID(ev), v)
		}
	}
	SetRSVPState(cal, target.Me, "")
	if strings.Contains(ics.Encode(cal), PropRSVPState) {
		t.Fatal("state not removed")
	}
	// Not on an event I'm not invited to.
	own := Base(parse(t, strings.ReplaceAll(source, "ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:me@gmail.com\n", "")), "me@gmail.com", "", target)
	SetRSVPState(own, target.Me, "confirmed")
	if strings.Contains(ics.Encode(own), PropRSVPState) {
		t.Fatal("state on an event I'm not invited to")
	}
}

func TestFileName(t *testing.T) {
	if got := FileName("test-invitation"); got != "test-invitation.ics" {
		t.Fatal(got)
	}
	a, b := FileName("abc@google.com"), FileName("abc_google.com")
	if a == b || !strings.HasPrefix(a, "abc_google.com-") || unsafeName.MatchString(strings.TrimSuffix(a, ".ics")) {
		t.Fatalf("%q / %q", a, b)
	}
}
