// Package icsfeed reads a published iCalendar feed (an http(s) URL, or a local file for tests): read-only, no
// answers sent. It is spec §4's plan B for an account that can't be signed into (the school's published calendar),
// and B1's first source.
package icsfeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

type Feed struct {
	URL  string       // http(s)://… or file:///…
	HTTP *http.Client // nil = defaultHTTP
}

// defaultHTTP gives up after two minutes: a feed that never answers would stop every pass, silently.
var defaultHTTP = &http.Client{Timeout: 2 * time.Minute}

// List fetches the whole feed every time: a feed has no sync token. The token returned is a hash of the feed, so
// an unchanged feed gives an empty page.
func (f *Feed) List(ctx context.Context, token string) (adapter.Page, error) {
	data, err := f.fetch(ctx)
	if err != nil {
		return adapter.Page{}, err
	}
	sum := sha256.Sum256(data)
	next := hex.EncodeToString(sum[:])
	if token == next {
		return adapter.Page{Token: next}, nil
	}
	cal, err := ics.Parse(string(data))
	if err != nil {
		return adapter.Page{}, fmt.Errorf("feed %s: %w", f.redacted(), err)
	}
	return adapter.Page{Changes: Split(cal), Token: next, Full: true}, nil
}

func (f *Feed) RSVP(context.Context, string, string) error { return adapter.ErrReadOnly }

// Split turns one calendar of many events into one change per UID (master and overrides together), each with the
// VTIMEZONEs it uses. VTODOs and the like are left out: mirrors hold events.
func Split(cal *ical.Calendar) []adapter.Change {
	zones := map[string]*ical.Component{}
	byUID := map[string][]*ical.Component{}
	var order []string
	for _, c := range cal.Children {
		switch c.Name {
		case ical.CompTimezone:
			zones[ics.Value(c, ical.PropTimezoneID)] = c
		case ical.CompEvent:
			uid := ics.Value(c, ical.PropUID)
			if uid == "" {
				continue
			}
			if _, ok := byUID[uid]; !ok {
				order = append(order, uid)
			}
			byUID[uid] = append(byUID[uid], c)
		}
	}
	var out []adapter.Change
	for _, uid := range order {
		ev := ical.NewCalendar()
		ev.Props.SetText(ical.PropVersion, "2.0")
		ev.Props.SetText(ical.PropProductID, ics.ProdID)
		var tzids []string
		seen := map[string]bool{}
		for _, c := range byUID[uid] {
			for _, props := range c.Props {
				for _, p := range props {
					if id := p.Params.Get(ical.PropTimezoneID); id != "" && !seen[id] {
						seen[id] = true
						tzids = append(tzids, id)
					}
				}
			}
		}
		sort.Strings(tzids)
		for _, id := range tzids {
			if z, ok := zones[id]; ok {
				ev.Children = append(ev.Children, z)
			}
		}
		ev.Children = append(ev.Children, byUID[uid]...)
		out = append(out, adapter.Change{UID: uid, SourceID: uid, Calendar: ev})
	}
	return out
}

func (f *Feed) fetch(ctx context.Context) ([]byte, error) {
	u, err := url.Parse(f.URL)
	if err != nil {
		return nil, fmt.Errorf("feed: bad URL: %w", scrub(err))
	}
	switch u.Scheme {
	case "file":
		return os.ReadFile(u.Path)
	case "http", "https", "webcal":
		if u.Scheme == "webcal" {
			u.Scheme = "https"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("feed %s: %w", f.redacted(), scrub(err))
		}
		hc := f.HTTP
		if hc == nil {
			hc = defaultHTTP
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("feed %s: %w", f.redacted(), scrub(err))
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("feed %s: %s", f.redacted(), resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	}
	return nil, fmt.Errorf("feed: unsupported URL scheme %q", u.Scheme)
}

// scrub keeps the feed's secret out of an error: Go's *url.Error repeats the whole URL ("Get \"https://…\": …").
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// redacted is the feed's URL for messages: published calendar links carry their secret in the path or query.
func (f *Feed) redacted() string {
	u, err := url.Parse(f.URL)
	switch {
	case err != nil:
		return "(unreadable URL)"
	case u.Scheme == "file":
		return f.URL
	}
	return u.Scheme + "://" + u.Host + "/…"
}
