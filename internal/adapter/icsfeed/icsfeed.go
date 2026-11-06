// Package icsfeed reads a published iCalendar feed (an https or webcal URL, or a local file for tests): read-only,
// no answers sent. It is spec §4's plan B for an account that can't be signed into (the school's published calendar),
// and B1's first source.
package icsfeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

type Feed struct {
	URL   string       // https://… or webcal://… (read as https), or file:///… with Files
	Files bool         // tests only: file:// URLs read from this machine
	HTTP  *http.Client // nil = defaultHTTP
}

// CheckURL is what a feed's link may be: https or webcal (https too), with a host. Anyone with the accounts page's
// password types it, and the backend fetches it from inside the VPS (security review, 2026-11-05).
func CheckURL(link string) error {
	u, err := url.Parse(link)
	switch {
	case err != nil:
		return fmt.Errorf("feed: bad URL: %w", scrub(err))
	case u.Scheme != "https" && u.Scheme != "webcal":
		return fmt.Errorf("feed: only https:// and webcal:// links, not %q", u.Scheme)
	case u.Hostname() == "":
		return errors.New("feed: the link has no host")
	}
	return nil
}

// defaultHTTP gives up after two minutes: a feed that never answers would stop every pass, silently. It reaches the
// public internet only, by https only, and through no proxy from the environment: a feed's link pointing at Davis,
// CouchDB or anything else on the VPS's networks would otherwise be fetched from inside them, its answer (or error)
// shown on the accounts page and copied into a calendar.
var defaultHTTP = &http.Client{
	Timeout: 2 * time.Minute,
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: publicOnly}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: time.Minute,
	},
	CheckRedirect: httpsOnly,
}

var errNotPublic = errors.New("not a public internet address")

// notPublic: ranges Go's netip doesn't flag, private in practice or carrying an IPv4 address inside.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, and Tailscale's addresses
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"), // 6to4
}

// publicOnly runs once the name is resolved, on the address about to be dialled: a name resolving to a private
// address, even only on the second lookup (DNS rebinding), never gets a connection.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if !isPublic(ip) {
		return errNotPublic
	}
	return nil
}

func isPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// httpsOnly follows redirects to https only: an https feed sending to http:// would go out unencrypted.
func httpsOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" {
		return errors.New("redirected to a link that isn't https")
	}
	return nil
}

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
	switch {
	case u.Scheme == "file" && f.Files:
		return os.ReadFile(u.Path)
	case u.Scheme == "https" || u.Scheme == "webcal":
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
	return nil, fmt.Errorf("feed: unsupported URL scheme %q (https:// or webcal:// only)", u.Scheme)
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
