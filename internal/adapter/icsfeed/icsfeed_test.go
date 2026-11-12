package icsfeed

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

const feed = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Microsoft Corporation//Outlook 16.0 MIMEDIR//EN
METHOD:PUBLISH
BEGIN:VTIMEZONE
TZID:Romance Standard Time
BEGIN:STANDARD
DTSTART:16010101T030000
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:16010101T020000
TZOFFSETFROM:+0100
TZOFFSETTO:+0200
RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=3
END:DAYLIGHT
END:VTIMEZONE
BEGIN:VEVENT
UID:series
DTSTAMP:20261020T080000Z
DTSTART;TZID=Romance Standard Time:20261102T140000
RRULE:FREQ=WEEKLY;COUNT=3
END:VEVENT
BEGIN:VTODO
UID:a-task
DTSTAMP:20261020T080000Z
END:VTODO
BEGIN:VEVENT
UID:once
DTSTAMP:20261020T080000Z
DTSTART;VALUE=DATE:20261124
END:VEVENT
BEGIN:VEVENT
UID:series
RECURRENCE-ID;TZID=Romance Standard Time:20261109T140000
DTSTAMP:20261020T080000Z
DTSTART;TZID=Romance Standard Time:20261109T160000
END:VEVENT
END:VCALENDAR
`

func TestList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.ics")
	os.WriteFile(path, []byte(strings.ReplaceAll(feed, "\n", "\r\n")), 0o600)
	f := &Feed{URL: "file://" + path, Files: true}
	page, err := f.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !page.Full || page.Token == "" || len(page.Changes) != 2 {
		t.Fatalf("page: %+v", page)
	}
	series, once := page.Changes[0], page.Changes[1]
	if series.UID != "series" || len(ics.Events(series.Calendar)) != 2 || series.Calendar.Children[0].Name != "VTIMEZONE" {
		t.Fatalf("series: master, override and its zone expected:\n%s", ics.Encode(series.Calendar))
	}
	if once.UID != "once" || len(once.Calendar.Children) != 1 {
		t.Fatalf("once: no zone expected:\n%s", ics.Encode(once.Calendar))
	}

	again, err := f.List(context.Background(), page.Token)
	if err != nil || len(again.Changes) != 0 || again.Full || again.Token != page.Token {
		t.Fatalf("unchanged feed: %+v %v", again, err)
	}
}

func TestRedacted(t *testing.T) {
	f := &Feed{URL: "https://outlook.office365.com/owa/calendar/abc/SECRET/calendar.ics"}
	if got := f.redacted(); strings.Contains(got, "SECRET") {
		t.Fatal(got)
	}
}

func TestErrorsKeepTheSecret(t *testing.T) {
	for _, u := range []string{
		"https://127.0.0.1:1/owa/calendar/SECRET/calendar.ics?k=SECRET", // nothing listens: Go's error names the URL
		"https://host/SECRET/%zz.ics",                                   // unreadable
	} {
		_, err := (&Feed{URL: u}).List(context.Background(), "")
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("%s: %v", u, err)
		}
	}
}

// A feed's link is typed on the accounts page and fetched from inside the VPS (security review, 2026-11-05).
func TestOnlyPublicHTTPS(t *testing.T) {
	for _, link := range []string{"https://outlook.office365.com/a.ics", "webcal://p01-calendars.icloud.com/x"} {
		if err := CheckURL(link); err != nil {
			t.Errorf("%s: %v", link, err)
		}
	}
	for _, link := range []string{"http://example.com/a.ics", "file:///etc/passwd", "gopher://x", "https:///a.ics", "davis:80"} {
		if CheckURL(link) == nil {
			t.Errorf("%s accepted", link)
		}
	}

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "feed.ics")
	os.WriteFile(path, []byte(feed), 0o600)
	for _, link := range []string{"file://" + path, "http://example.com/a.ics"} {
		if _, err := (&Feed{URL: link}).List(ctx, ""); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("%s: %v", link, err)
		}
	}
	for _, link := range []string{"https://127.0.0.1:1/a.ics", "https://localhost:1/a.ics", "webcal://[::1]:1/a.ics",
		"https://169.254.169.254/latest/meta-data", "https://10.0.0.1:1/a.ics"} {
		if _, err := (&Feed{URL: link}).List(ctx, ""); !errors.Is(err, errNotPublic) {
			t.Errorf("%s: %v", link, err)
		}
	}

	for addr, public := range map[string]bool{
		"8.8.8.8": true, "2a00:1450:4007:80e::200e": true, "52.97.1.1": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.18.0.4": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.101.102.103": false, "0.0.0.0": false, "::1": false, "fd7a:115c:a1e0::1": false, "fe80::1": false,
		"::ffff:127.0.0.1": false, "64:ff9b::a00:1": false, "2002:a00:1::": false, "224.0.0.1": false, "255.255.255.255": false,
	} {
		if got := isPublic(netip.MustParseAddr(addr)); got != public {
			t.Errorf("%s public = %v", addr, got)
		}
	}

	to := func(link string) error {
		req, _ := http.NewRequest(http.MethodGet, link, nil)
		return httpsOnly(req, []*http.Request{req})
	}
	if to("https://other.example/a.ics") != nil || to("http://other.example/a.ics") == nil {
		t.Fatal("redirects: https only")
	}
}

// Outlook's feed writes an all-day event Exchange keeps in UTC (Luma's invitation, 2026-11-12) from midnight UTC to
// midnight UTC in the publisher's zone: it becomes all-day again. Only that.
func TestOutlookAllDay(t *testing.T) {
	ev := func(uid, lines string) string {
		return "BEGIN:VEVENT\nUID:" + uid + "\nDTSTAMP:20261112T223241Z\n" + lines + "\nEND:VEVENT\n"
	}
	const romance = "BEGIN:VTIMEZONE\nTZID:Romance Standard Time\nBEGIN:STANDARD\nDTSTART:16010101T030000\n" +
		"TZOFFSETFROM:+0200\nTZOFFSETTO:+0100\nEND:STANDARD\nEND:VTIMEZONE\n"
	cal, err := ics.Parse(strings.ReplaceAll("BEGIN:VCALENDAR\nVERSION:2.0\n"+romance+
		ev("luma", "DTSTART;TZID=Romance Standard Time:20261120T010000\nDTEND;TZID=Romance Standard Time:20261123T010000\nX-MICROSOFT-CDO-ALLDAYEVENT:FALSE")+
		ev("summer", "DTSTART;TZID=Romance Standard Time:20260701T020000\nDTEND;TZID=Romance Standard Time:20260702T020000\nX-MICROSOFT-CDO-ALLDAYEVENT:FALSE")+
		ev("utc", "DTSTART:20261120T000000Z\nDTEND:20261121T000000Z\nX-MICROSOFT-CDO-ALLDAYEVENT:FALSE")+
		ev("paris-midnight", "DTSTART;TZID=Romance Standard Time:20261120T000000\nDTEND;TZID=Romance Standard Time:20261121T000000\nX-MICROSOFT-CDO-ALLDAYEVENT:FALSE")+
		ev("at-one", "DTSTART;TZID=Romance Standard Time:20261120T010000\nDTEND;TZID=Romance Standard Time:20261120T020000\nX-MICROSOFT-CDO-ALLDAYEVENT:FALSE")+
		ev("not-outlook", "DTSTART:20261120T000000Z\nDTEND:20261121T000000Z")+
		ev("series", "DTSTART;TZID=Romance Standard Time:20261120T010000\nDTEND;TZID=Romance Standard Time:20261121T010000\nRRULE:FREQ=WEEKLY;COUNT=2\nX-MICROSOFT-CDO-ALLDAYEVENT:FALSE")+
		"END:VCALENDAR\n", "\n", "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"luma":           {"DTSTART;VALUE=DATE:20261120", "DTEND;VALUE=DATE:20261123"},
		"summer":         {"DTSTART;VALUE=DATE:20260701", "DTEND;VALUE=DATE:20260702"},
		"utc":            {"DTSTART;VALUE=DATE:20261120", "DTEND;VALUE=DATE:20261121"},
		"paris-midnight": {"DTSTART;TZID=Romance Standard Time:20261120T000000", "DTEND;TZID=Romance Standard Time:20261121T000000"},
		"at-one":         {"DTSTART;TZID=Romance Standard Time:20261120T010000", "DTEND;TZID=Romance Standard Time:20261120T020000"},
		"not-outlook":    {"DTSTART:20261120T000000Z", "DTEND:20261121T000000Z"},
		"series":         {"DTSTART;TZID=Romance Standard Time:20261120T010000", "DTEND;TZID=Romance Standard Time:20261121T010000"},
	}
	for _, c := range Split(cal) {
		text := ics.Encode(c.Calendar)
		w := want[c.UID]
		if !strings.Contains(text, "\r\n"+w[0]+"\r\n") || !strings.Contains(text, "\r\n"+w[1]+"\r\n") {
			t.Errorf("%s: want %s / %s:\n%s", c.UID, w[0], w[1], text)
		}
		allDay := strings.HasPrefix(w[0], "DTSTART;VALUE=DATE")
		if allDay != strings.Contains(text, "X-MICROSOFT-CDO-ALLDAYEVENT:TRUE") && c.UID != "not-outlook" {
			t.Errorf("%s: X-MICROSOFT-CDO-ALLDAYEVENT not %v:\n%s", c.UID, allDay, text)
		}
		if allDay && strings.Contains(text, "VTIMEZONE") {
			t.Errorf("%s: a zone no time uses:\n%s", c.UID, text)
		}
		delete(want, c.UID)
	}
	if len(want) != 0 {
		t.Fatalf("missing: %v", want)
	}
}
