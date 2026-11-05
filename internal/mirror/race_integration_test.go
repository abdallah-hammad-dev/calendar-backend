package mirror

// A client's change landing between the backend's write and its read of the new ETag, against a real Davis:
// DAVIS_TEST_URL=http://127.0.0.1:9000/dav/ go test ./internal/mirror/ -run Between
// As orga, in a throwaway collection (see engine_integration_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

// betweenPutAndRead runs `run` once, inside the backend's next PUT: after Davis took it, before the backend hears
// back, and without the ETag (as when Davis changed what it stored), so that the backend reads the item again.
type betweenPutAndRead struct {
	next http.RoundTripper
	run  func()
}

func (b *betweenPutAndRead) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := b.next.RoundTrip(req)
	if err == nil && req.Method == http.MethodPut && b.run != nil {
		run := b.run
		b.run = nil
		run()
		resp.Header.Del("ETag")
	}
	return resp, err
}

func TestClientWriteBetweenPutAndRead(t *testing.T) {
	url := os.Getenv("DAVIS_TEST_URL")
	if url == "" {
		t.Skip("DAVIS_TEST_URL not set")
	}
	ctx := context.Background()
	user, password := env("DAVIS_TEST_USER", "orga"), env("DAVIS_TEST_PASSWORD", "x")
	hook := &betweenPutAndRead{next: http.DefaultTransport}
	dc, err := davis.New(url, user, password, &http.Client{Timeout: time.Minute, Transport: hook})
	if err != nil {
		t.Fatal(err)
	}
	phone, err := davis.New(url, user, password, nil) // another device, outside the hook
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
	feed := filepath.Join(dir, "feed.ics")
	writeFeed := func(summary string) {
		t.Helper()
		body := feedHead + invitationEvent(summary, "NEEDS-ACTION") + "END:VCALENDAR\n"
		if err := os.WriteFile(feed, []byte(strings.ReplaceAll(body, "\n", "\r\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFeed("Soutenance")

	var calls []string
	var next error
	e := &Engine{Store: st, Davis: dc, Me: p.Email(), Home: p.Home, Loc: paris,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Adapter: func(a store.Account) (adapter.Adapter, error) {
			return answering{Feed: &icsfeed.Feed{URL: "file://" + feed}, calls: &calls, next: &next}, nil
		}}
	if testing.Verbose() {
		e.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	coll := p.Home + "it-race-" + suffix + "/"
	if err := dc.MakeCalendar(ctx, coll, "Race test "+suffix, "", "VEVENT"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.Delete(context.Background(), coll, "") })
	conf, _ := json.Marshal(map[string]string{"url": "file://" + feed})
	a, err := e.AddAccount(ctx, store.Account{Kind: "ics", Name: "School", Address: "me@school.test", Config: string(conf)}, coll)
	if err != nil {
		t.Fatal(err)
	}
	const uid = "it-invitation@school.test"
	onDavis := func() string {
		t.Helper()
		it, err := st.Item(ctx, a.ID, uid)
		if err != nil {
			t.Fatal(err)
		}
		got, err := phone.Get(ctx, it.Href)
		if err != nil {
			t.Fatal(err)
		}
		cal, err := ics.Parse(got.Data)
		if err != nil {
			t.Fatal(err)
		}
		return ics.MyPartstat(cal, e.Me)
	}

	if err := e.Pass(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := onDavis(); got != "NEEDS-ACTION" {
		t.Fatalf("copy written with %q", got)
	}

	// The source changes (a new room): the backend writes the copy again, and the phone declines right then.
	writeFeed("Soutenance (salle B)")
	hook.run = func() {
		it, err := st.Item(ctx, a.ID, uid)
		if err != nil {
			t.Error(err)
			return
		}
		got, err := phone.Get(ctx, it.Href)
		if err != nil {
			t.Error(err)
			return
		}
		cal, err := ics.Parse(got.Data)
		if err != nil {
			t.Error(err)
			return
		}
		ics.Attendee(ics.Events(cal)[0], e.Me).Params.Set(ical.ParamParticipationStatus, "DECLINED")
		if _, err := phone.Put(ctx, got.Href, ics.Encode(cal), got.ETag); err != nil {
			t.Error(err)
		}
	}
	if err := e.Pass(ctx, true); err != nil {
		t.Fatal(err)
	}
	if hook.run != nil {
		t.Fatal("the backend didn't write the copy again")
	}

	// Relayed at the next pass, and kept through the source's next change.
	if err := e.Pass(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != uid+" DECLINED" {
		t.Fatalf("the phone's answer wasn't relayed: calls %v", calls)
	}
	writeFeed("Soutenance (salle C)")
	if err := e.Pass(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := onDavis(); got != "DECLINED" {
		t.Fatalf("the phone's answer was overwritten: %q on Davis", got)
	}
}
