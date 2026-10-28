package ics

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-ical"
)

func crlf(s string) string { return strings.ReplaceAll(strings.TrimLeft(s, "\n"), "\n", "\r\n") }

const invitation = `
BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:inv-1
DTSTAMP:20261020T080000Z
SUMMARY:Réunion d'équipe
DTSTART;TZID=Europe/Paris:20261023T110000
DTEND;TZID=Europe/Paris:20261023T120000
ORGANIZER;CN=Orga:mailto:orga@example.com
ATTENDEE;CN=Orga;PARTSTAT=ACCEPTED:mailto:orga@example.com
ATTENDEE;CN="Moi, Abd";PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:Me@Gmail.com
END:VEVENT
END:VCALENDAR
`

func TestRoundTrip(t *testing.T) {
	cal, err := Parse(crlf(invitation))
	if err != nil {
		t.Fatal(err)
	}
	out := Encode(cal)
	again, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if Encode(again) != out {
		t.Fatalf("encoding isn't stable:\n%s\n---\n%s", out, Encode(again))
	}
	if !strings.Contains(out, "\r\nATTENDEE;CN=\"Moi, Abd\";PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:Me@Gmail.com\r\n") {
		t.Fatalf("quoted parameter lost:\n%s", out)
	}
	if got := MyPartstat(cal, "mailto:me@gmail.com"); got != "NEEDS-ACTION" {
		t.Fatalf("MyPartstat = %q", got)
	}
	if got := MyPartstat(cal, "other@example.com"); got != "" {
		t.Fatalf("MyPartstat for a stranger = %q", got)
	}
}

func TestFolding(t *testing.T) {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, ProdID)
	ev := ical.NewEvent()
	long := strings.Repeat("é", 100) + " et une fin"
	ev.Props.SetText(ical.PropDescription, long+"\nligne 2")
	ev.Props.SetText(ical.PropUID, "x")
	cal.Children = append(cal.Children, ev.Component)
	out := Encode(cal)
	for _, line := range strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Fatalf("line of %d octets: %q", len(line), line)
		}
		if !utf8.ValidString(line) {
			t.Fatalf("a UTF-8 sequence was split: %q", line)
		}
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Events(back)[0].Props.Text(ical.PropDescription); got != long+"\nligne 2" {
		t.Fatalf("description after folding = %q", got)
	}
}

func TestParseMalformed(t *testing.T) {
	// go-ical panics on a character after a quoted parameter value; that must be an error.
	if _, err := Parse(crlf("BEGIN:VCALENDAR\nBEGIN:VEVENT\nATTENDEE;CN=\"a\"b:mailto:x@y\nEND:VEVENT\nEND:VCALENDAR\n")); err == nil {
		t.Fatal("no error")
	}
}

func TestAddress(t *testing.T) {
	for in, want := range map[string]string{"mailto:A@B.c": "a@b.c", " MAILTO:x@y ": "x@y", "x@y": "x@y", "/dav/principals/abd/": "/dav/principals/abd/"} {
		if got := Address(in); got != want {
			t.Errorf("Address(%q) = %q, want %q", in, got, want)
		}
	}
}

// The bench's hand-written Europe/Paris (bench/push-fixtures.sh), which iOS shows without "(GMT)".
const parisZone = `
BEGIN:VTIMEZONE
TZID:Europe/Paris
BEGIN:DAYLIGHT
DTSTART:19700329T020000
RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU
TZNAME:CEST
TZOFFSETFROM:+0100
TZOFFSETTO:+0200
END:DAYLIGHT
BEGIN:STANDARD
DTSTART:19701025T030000
RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU
TZNAME:CET
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
END:STANDARD
END:VTIMEZONE
`

func zoneText(t *testing.T, name string, at time.Time) string {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	cal := &ical.Calendar{Component: VTimezone(loc, at)}
	b.WriteString(Encode(cal))
	return b.String()
}

func TestVTimezone(t *testing.T) {
	at := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	if got := zoneText(t, "Europe/Paris", at); got != crlf(parisZone) {
		t.Fatalf("Europe/Paris:\n%s", got)
	}
	ny := zoneText(t, "America/New_York", at)
	for _, want := range []string{"DTSTART:19700308T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU", "DTSTART:19701101T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU"} {
		if !strings.Contains(ny, want) {
			t.Fatalf("America/New_York lacks %q:\n%s", want, ny)
		}
	}
	tokyo := zoneText(t, "Asia/Tokyo", at)
	if !strings.Contains(tokyo, "BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZNAME:JST\r\nTZOFFSETFROM:+0900\r\nTZOFFSETTO:+0900\r\n") || strings.Contains(tokyo, "DAYLIGHT") {
		t.Fatalf("Asia/Tokyo:\n%s", tokyo)
	}
	// Southern hemisphere: DST starts in October.
	syd := zoneText(t, "Australia/Sydney", at)
	if !strings.Contains(syd, "BEGIN:DAYLIGHT\r\nDTSTART:19701004T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=1SU") {
		t.Fatalf("Australia/Sydney:\n%s", syd)
	}
}

func TestLocalize(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	cal, err := Parse(crlf(`
BEGIN:VCALENDAR
VERSION:2.0
PRODID:x
BEGIN:VEVENT
UID:once
DTSTAMP:20261020T080000Z
DTSTART:20261023T090000Z
DTEND:20261023T100000Z
END:VEVENT
BEGIN:VEVENT
UID:series
DTSTAMP:20261020T080000Z
DTSTART:20261023T090000Z
RRULE:FREQ=WEEKLY
END:VEVENT
BEGIN:VEVENT
UID:ny
DTSTAMP:20261020T080000Z
DTSTART;TZID=America/New_York:20261023T090000
END:VEVENT
BEGIN:VEVENT
UID:windows
DTSTAMP:20261020T080000Z
DTSTART;TZID=Romance Standard Time:20261023T090000
END:VEVENT
END:VCALENDAR
`))
	if err != nil {
		t.Fatal(err)
	}
	unknown := Localize(cal, paris)
	if len(unknown) != 1 || unknown[0] != "Romance Standard Time" {
		t.Fatalf("unknown zones = %v", unknown)
	}
	evs := Events(cal)
	if p := evs[0].Props.Get(ical.PropDateTimeStart); p.Value != "20261023T110000" || p.Params.Get("TZID") != "Europe/Paris" {
		t.Fatalf("one-off event: DTSTART %v", p)
	}
	if p := evs[1].Props.Get(ical.PropDateTimeStart); p.Value != "20261023T090000Z" {
		t.Fatalf("a recurring master in UTC must stay in UTC: %v", p)
	}
	var zones []string
	for _, c := range cal.Children {
		if c.Name == ical.CompTimezone {
			zones = append(zones, Value(c, ical.PropTimezoneID))
		}
	}
	if strings.Join(zones, ",") != "Europe/Paris,America/New_York" {
		t.Fatalf("VTIMEZONEs = %v", zones)
	}
	// Run again: nothing more is added.
	Localize(cal, paris)
	n := 0
	for _, c := range cal.Children {
		if c.Name == ical.CompTimezone {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d VTIMEZONEs after a second run", n)
	}
}

// The same event gives the same text every time: the mirror compares hashes to skip rewriting what didn't change.
func TestLocalizeIsStable(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	src := crlf(`
BEGIN:VCALENDAR
VERSION:2.0
PRODID:x
BEGIN:VEVENT
UID:flight
DTSTAMP:20261020T080000Z
DTSTART;TZID=America/New_York:20261023T090000
DTEND;TZID=Asia/Tokyo:20261024T130000
END:VEVENT
BEGIN:VEVENT
UID:series
DTSTAMP:20261020T080000Z
DTSTART;TZID=Africa/Casablanca:20240105T090000
RRULE:FREQ=WEEKLY
EXDATE;TZID=Africa/Casablanca:20261023T090000
END:VEVENT
END:VCALENDAR
`)
	var first string
	for i := 0; i < 50; i++ {
		cal, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		Localize(cal, paris)
		if out := Encode(cal); i == 0 {
			first = out
		} else if out != first {
			t.Fatalf("run %d gave another text:\n%s\nfirst:\n%s", i, out, first)
		}
	}
}
