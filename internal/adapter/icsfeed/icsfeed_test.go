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
