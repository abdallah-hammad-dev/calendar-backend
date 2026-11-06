package mirror

// The RSVP relay against a real Davis: DAVIS_TEST_URL=http://127.0.0.1:9000/dav/ go test ./internal/mirror/
// As orga, in a throwaway collection (see engine_integration_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// answering is a feed that takes answers, the way the next RSVP call is told to go.
type answering struct {
	*icsfeed.Feed
	calls *[]string
	next  *error
}

func (f answering) RSVP(_ context.Context, id, partstat string) error {
	*f.calls = append(*f.calls, id+" "+partstat)
	return *f.next
}

func TestRelayAgainstDavis(t *testing.T) {
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
	feed := filepath.Join(dir, "feed.ics")
	writeFeed := func(partstat string) {
		t.Helper()
		body := feedHead + invitationEvent("Soutenance", partstat) + "END:VCALENDAR\n"
		if err := os.WriteFile(feed, []byte(strings.ReplaceAll(body, "\n", "\r\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFeed("NEEDS-ACTION")

	var calls []string
	var next error
	clock := time.Now()
	e := &Engine{Store: st, Davis: dc, Me: p.Email(), Home: p.Home, Loc: paris, Now: func() time.Time { return clock },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Adapter: func(a store.Account) (adapter.Adapter, error) {
			return answering{Feed: &icsfeed.Feed{URL: "file://" + feed, Files: true}, calls: &calls, next: &next}, nil
		}}
	if testing.Verbose() {
		e.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	coll := p.Home + "it-relay-" + suffix + "/"
	if err := dc.MakeCalendar(ctx, coll, "Relay test "+suffix, "", "VEVENT"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.Delete(context.Background(), coll, "") })
	conf, _ := json.Marshal(map[string]string{"url": "file://" + feed})
	a, err := e.AddAccount(ctx, store.Account{Kind: "ics", Name: "School", Address: "me@school.test", Config: string(conf)}, coll)
	if err != nil {
		t.Fatal(err)
	}

	const uid = "it-invitation@school.test"
	pass := func() error {
		t.Helper()
		return e.Pass(ctx, true)
	}
	copyOnDavis := func() (davis.Item, *ical.Component) {
		t.Helper()
		it, err := st.Item(ctx, a.ID, uid)
		if err != nil {
			t.Fatal(err)
		}
		got, err := dc.Get(ctx, it.Href)
		if err != nil {
			t.Fatal(err)
		}
		cal, err := ics.Parse(got.Data)
		if err != nil {
			t.Fatal(err)
		}
		return got, ics.Events(cal)[0]
	}
	state := func() string {
		t.Helper()
		_, ev := copyOnDavis()
		v, _ := ev.Props.Text(PropRSVPState)
		return v
	}
	// answer is what the extension does: my PARTSTAT changed and the state removed, PUT with If-Match.
	answer := func(partstat string) {
		t.Helper()
		got, ev := copyOnDavis()
		ics.Attendee(ev, e.Me).Params.Set(ical.ParamParticipationStatus, partstat)
		ev.Props.Del(PropRSVPState)
		cal, _ := ics.Parse(got.Data)
		cal.Children = append([]*ical.Component{}, cal.Children...)
		for i, c := range cal.Children {
			if c.Name == ical.CompEvent {
				cal.Children[i] = ev
			}
		}
		if _, err := dc.Put(ctx, got.Href, ics.Encode(cal), got.ETag); err != nil {
			t.Fatal(err)
		}
	}

	if err := pass(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 || state() != "" {
		t.Fatalf("an answer sent before I answered: %v", calls)
	}

	// 1. I accept: sent once, the event says so.
	answer("ACCEPTED")
	if err := pass(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != uid+" ACCEPTED" || state() != StateConfirmed {
		t.Fatalf("calls %v, state %q", calls, state())
	}
	before, _ := copyOnDavis()
	if err := pass(); err != nil {
		t.Fatal(err)
	}
	if after, _ := copyOnDavis(); len(calls) != 1 || after.ETag != before.ETag {
		t.Fatalf("sent or written again: %v", calls)
	}

	// 2. The source shows it: no longer in flight, the state stays.
	writeFeed("ACCEPTED")
	if err := pass(); err != nil {
		t.Fatal(err)
	}
	if it, _ := st.Item(ctx, a.ID, uid); it.Answer != "" || state() != StateConfirmed {
		t.Fatalf("after the source showed it: %+v, state %q", it, state())
	}

	// 3. I decline while the provider is down: pending, tried again, then refused for good.
	next = errors.New("provider down")
	answer("DECLINED")
	if err := pass(); err == nil {
		t.Fatal("a failed answer not reported on the account")
	}
	if len(calls) != 2 || state() != StatePending {
		t.Fatalf("calls %v, state %q", calls, state())
	}
	next = &adapter.Refused{Reason: "l'événement n'existe plus", Err: errors.New("gone")}
	pass()
	if len(calls) != 3 || state() != StateFailed+"l'événement n'existe plus" {
		t.Fatalf("calls %v, state %q", calls, state())
	}
	pass()
	if len(calls) != 3 {
		t.Fatal("a refused answer tried again")
	}
	if _, ev := copyOnDavis(); ics.Partstat(ics.Attendee(ev, e.Me)) != "DECLINED" {
		t.Fatal("my answer lost while the source says otherwise")
	}

	// 4. I answer again: tried again.
	next = nil
	answer("TENTATIVE")
	pass()
	if len(calls) != 4 || calls[3] != uid+" TENTATIVE" || state() != StateConfirmed {
		t.Fatalf("calls %v, state %q", calls, state())
	}

	// 5. Changed on the provider after the answer was taken: once it's been a while, the source wins.
	writeFeed("DECLINED")
	pass()
	if _, ev := copyOnDavis(); ics.Partstat(ics.Attendee(ev, e.Me)) != "TENTATIVE" {
		t.Fatal("the source won right after my answer was sent")
	}
	clock = clock.Add(sourceWins + time.Minute)
	writeFeed("DECLINED") // the same content: a feed's unchanged listing is skipped, so make it new
	os.WriteFile(feed, append(mustRead(t, feed), []byte("\r\n")...), 0o600)
	pass()
	if _, ev := copyOnDavis(); ics.Partstat(ics.Attendee(ev, e.Me)) != "DECLINED" {
		t.Fatal("the source didn't win")
	}
	if len(calls) != 4 {
		t.Fatalf("the source's answer was sent back: %v", calls)
	}

	// 6. A source that says when it changed (Google, Microsoft): declined here, yes on the provider a minute later
	// (2026-11-05, the user's case). Older than my answer, it's the provider not caught up yet: mine stays. Newer,
	// it's a change made there: it wins at once, and isn't sent back.
	writeFeedAt := func(partstat string, at time.Time) {
		t.Helper()
		ev := strings.Replace(invitationEvent("Soutenance", partstat), "SEQUENCE:0\n",
			"SEQUENCE:0\nLAST-MODIFIED:"+at.UTC().Format("20060102T150405Z")+"\n", 1)
		if err := os.WriteFile(feed, []byte(strings.ReplaceAll(feedHead+ev+"END:VCALENDAR\n", "\n", "\r\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFeedAt("ACCEPTED", clock.Add(-2*time.Hour)) // nothing in flight: the source's answer, as is
	pass()
	if _, ev := copyOnDavis(); ics.Partstat(ics.Attendee(ev, e.Me)) != "ACCEPTED" {
		t.Fatal("the source's answer not written")
	}
	answer("DECLINED")
	pass()
	if len(calls) != 5 || calls[4] != uid+" DECLINED" || state() != StateConfirmed {
		t.Fatalf("calls %v, state %q", calls, state())
	}
	writeFeedAt("ACCEPTED", clock.Add(-time.Hour))
	pass()
	if _, ev := copyOnDavis(); ics.Partstat(ics.Attendee(ev, e.Me)) != "DECLINED" {
		t.Fatal("a listing older than my answer won")
	}
	clock = clock.Add(time.Minute)
	writeFeedAt("ACCEPTED", clock)
	pass()
	if _, ev := copyOnDavis(); ics.Partstat(ics.Attendee(ev, e.Me)) != "ACCEPTED" {
		t.Fatal("a change made on the provider after my answer didn't win")
	}
	if it, _ := st.Item(ctx, a.ID, uid); it.Answer != "" || len(calls) != 5 {
		t.Fatalf("still in flight or sent back: %+v, calls %v", it, calls)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
