// Package davis is the backend's CalDAV client for Davis (sabre/dav), written for what the backend does there:
// find my principal and address, create and remove mirror collections, write events with preconditions, and
// follow collections with sync-collection (RFC 6578).
package davis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrPrecondition: the item changed (If-Match) or already exists (If-None-Match) — 412.
	ErrPrecondition = errors.New("davis: precondition failed")
	// ErrNotFound: 404.
	ErrNotFound = errors.New("davis: not found")
	// ErrInvalidToken: the server can't serve this sync token; start again with a full listing.
	ErrInvalidToken = errors.New("davis: invalid sync token")
)

type Client struct {
	base     *url.URL
	user     string
	password string
	http     *http.Client
}

// New makes a client for the server at base (e.g. http://127.0.0.1:9000/dav/). hc nil = a client that gives up
// after two minutes: without a limit, a server that never answers stops every pass, silently.
func New(base, user, password string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("davis: bad server URL %q", base)
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Client{base: u, user: user, password: password, http: hc}, nil
}

// Principal is what discovery finds: where my calendars live and the address Davis knows me by.
type Principal struct {
	Href      string
	Home      string   // calendar-home-set
	Addresses []string // calendar-user-address-set, as given (mailto:…, /dav/principals/…)
}

// Email is my mailto: address on Davis, the one written as my ATTENDEE in mirror events ("" if none).
func (p Principal) Email() string {
	for _, a := range p.Addresses {
		if strings.HasPrefix(strings.ToLower(a), "mailto:") {
			return a[len("mailto:"):]
		}
	}
	return ""
}

// Calendar is a calendar collection under my home.
type Calendar struct {
	Href        string
	DisplayName string
	Color       string
	Components  []string // VEVENT, VTODO
	SyncToken   string
}

// Item is a calendar object resource.
type Item struct {
	Href string
	ETag string
	Data string // "" when not asked for
}

func (c *Client) Discover(ctx context.Context) (Principal, error) {
	ms, err := c.propfind(ctx, c.base.Path, "0", `<d:prop><d:current-user-principal/></d:prop>`)
	if err != nil {
		return Principal{}, err
	}
	var p Principal
	for _, r := range ms.Responses {
		if h := r.propHref(nsDAV, "current-user-principal"); h != "" {
			p.Href = h
		}
	}
	if p.Href == "" {
		return Principal{}, fmt.Errorf("davis: no current-user-principal at %s", c.base)
	}
	ms, err = c.propfind(ctx, p.Href, "0", `<d:prop><c:calendar-home-set/><c:calendar-user-address-set/></d:prop>`)
	if err != nil {
		return Principal{}, err
	}
	for _, r := range ms.Responses {
		if h := r.propHref(nsCalDAV, "calendar-home-set"); h != "" {
			p.Home = h
		}
		if set := r.prop(nsCalDAV, "calendar-user-address-set"); set != nil {
			for _, h := range set.children(nsDAV, "href") {
				p.Addresses = append(p.Addresses, h.text())
			}
		}
	}
	if p.Home == "" {
		return Principal{}, fmt.Errorf("davis: no calendar-home-set for %s", p.Href)
	}
	return p, nil
}

func (c *Client) Calendars(ctx context.Context, home string) ([]Calendar, error) {
	ms, err := c.propfind(ctx, home, "1", `<d:prop><d:resourcetype/><d:displayname/><a:calendar-color/>`+
		`<c:supported-calendar-component-set/><d:sync-token/></d:prop>`)
	if err != nil {
		return nil, err
	}
	var out []Calendar
	for _, r := range ms.Responses {
		rt := r.prop(nsDAV, "resourcetype")
		if rt == nil || rt.child(nsCalDAV, "calendar") == nil {
			continue
		}
		cal := Calendar{
			Href:        r.Href,
			DisplayName: r.propText(nsDAV, "displayname"),
			Color:       r.propText(nsApple, "calendar-color"),
			SyncToken:   r.propText(nsDAV, "sync-token"),
		}
		if set := r.prop(nsCalDAV, "supported-calendar-component-set"); set != nil {
			for _, comp := range set.children(nsCalDAV, "comp") {
				cal.Components = append(cal.Components, comp.attr("name"))
			}
		}
		out = append(out, cal)
	}
	return out, nil
}

// MakeCalendar creates a calendar collection at href (MKCALENDAR). color "" = the server's.
func (c *Client) MakeCalendar(ctx context.Context, href, displayName, color string, components ...string) error {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><c:mkcalendar ` + namespaces + `><d:set><d:prop>`)
	b.WriteString(`<d:displayname>` + escapeXML(displayName) + `</d:displayname>`)
	if color != "" {
		b.WriteString(`<a:calendar-color>` + escapeXML(color) + `</a:calendar-color>`)
	}
	b.WriteString(`<c:supported-calendar-component-set>`)
	for _, comp := range components {
		b.WriteString(`<c:comp name="` + escapeXML(comp) + `"/>`)
	}
	b.WriteString(`</c:supported-calendar-component-set></d:prop></d:set></c:mkcalendar>`)
	resp, err := c.do(ctx, "MKCALENDAR", href, b.String(), "application/xml; charset=utf-8", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("davis: MKCALENDAR %s: %s", href, resp.Status)
	}
	return nil
}

// Get reads one item.
func (c *Client) Get(ctx context.Context, href string) (Item, error) {
	resp, err := c.do(ctx, http.MethodGet, href, "", "", nil)
	if err != nil {
		return Item{}, err
	}
	defer resp.Body.Close()
	if err := expect(resp, http.StatusOK); err != nil {
		return Item{}, fmt.Errorf("GET %s: %w", href, err)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Item{}, err
	}
	return Item{Href: href, ETag: resp.Header.Get("ETag"), Data: string(data)}, nil
}

// Put writes an item. ifMatch "" creates it (If-None-Match: *, ErrPrecondition if it exists); otherwise it
// replaces the version with that ETag (ErrPrecondition if it changed). The new ETag is "" when the server didn't
// store the data exactly as sent (sabre then sends none): read the item back to know it.
func (c *Client) Put(ctx context.Context, href, data, ifMatch string) (string, error) {
	h := http.Header{}
	if ifMatch == "" {
		h.Set("If-None-Match", "*")
	} else {
		h.Set("If-Match", ifMatch)
	}
	resp, err := c.do(ctx, http.MethodPut, href, data, "text/calendar; charset=utf-8", h)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := expect(resp, http.StatusCreated, http.StatusNoContent, http.StatusOK); err != nil {
		return "", fmt.Errorf("PUT %s: %w", href, err)
	}
	return resp.Header.Get("ETag"), nil
}

// Delete removes an item or a collection. ifMatch "" = whatever its version.
func (c *Client) Delete(ctx context.Context, href, ifMatch string) error {
	h := http.Header{}
	if ifMatch != "" {
		h.Set("If-Match", ifMatch)
	}
	resp, err := c.do(ctx, http.MethodDelete, href, "", "", h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := expect(resp, http.StatusNoContent, http.StatusOK); err != nil {
		return fmt.Errorf("DELETE %s: %w", href, err)
	}
	return nil
}

// Changes is what changed in a collection since a sync token.
type Changes struct {
	Token   string
	Changed []Item   // with Data when the server sent it
	Removed []string // hrefs
	Full    bool     // a full listing (no token, or the token was invalid): members not in Changed are gone
}

// Sync lists what changed in the collection since token ("" = everything), with each changed item's data. An
// invalid token falls back to a full listing (Changes.Full).
//
// sabre/dav answers an unknown token that has its own prefix with "no changes" and the same token instead of an
// error; that is told apart from a real "no changes" by the collection's current token (as in the extension).
func (c *Client) Sync(ctx context.Context, coll, token string) (Changes, error) {
	ch, err := c.syncReport(ctx, coll, token)
	if errors.Is(err, ErrInvalidToken) && token != "" {
		ch, err = c.syncReport(ctx, coll, "")
	} else if err == nil && token != "" && ch.Token == token && len(ch.Changed) == 0 && len(ch.Removed) == 0 {
		current, cerr := c.syncToken(ctx, coll)
		if cerr != nil {
			return Changes{}, cerr
		}
		if current != token {
			ch, err = c.syncReport(ctx, coll, "")
		}
	}
	if err != nil {
		return Changes{}, err
	}
	return ch, nil
}

func (c *Client) syncReport(ctx context.Context, coll, token string) (Changes, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><d:sync-collection ` + namespaces + `>` +
		`<d:sync-token>` + escapeXML(token) + `</d:sync-token><d:sync-level>1</d:sync-level>` +
		`<d:prop><d:getetag/><c:calendar-data/></d:prop></d:sync-collection>`
	resp, err := c.do(ctx, "REPORT", coll, body, "application/xml; charset=utf-8", http.Header{"Depth": {"1"}})
	if err != nil {
		return Changes{}, err
	}
	defer resp.Body.Close()
	if token != "" && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusConflict) {
		// RFC 6578 §3.2: DAV:valid-sync-token precondition.
		return Changes{}, ErrInvalidToken
	}
	if err := expect(resp, http.StatusMultiStatus); err != nil {
		return Changes{}, fmt.Errorf("REPORT sync-collection %s: %w", coll, err)
	}
	ms, err := parseMultistatus(resp.Body)
	if err != nil {
		return Changes{}, err
	}
	ch := Changes{Token: ms.SyncToken, Full: token == ""}
	collPath := cleanPath(c.resolve(coll).Path)
	for _, r := range ms.Responses {
		if cleanPath(c.resolve(r.Href).Path) == collPath {
			continue
		}
		if r.Status == http.StatusNotFound {
			ch.Removed = append(ch.Removed, r.Href)
			continue
		}
		etag := r.propText(nsDAV, "getetag")
		if etag == "" {
			continue // a member that isn't an item (sabre lists nothing else here, but be safe)
		}
		ch.Changed = append(ch.Changed, Item{Href: r.Href, ETag: etag, Data: r.propText(nsCalDAV, "calendar-data")})
	}
	return ch, nil
}

func (c *Client) syncToken(ctx context.Context, coll string) (string, error) {
	ms, err := c.propfind(ctx, coll, "0", `<d:prop><d:sync-token/></d:prop>`)
	if err != nil {
		return "", err
	}
	for _, r := range ms.Responses {
		if t := r.propText(nsDAV, "sync-token"); t != "" {
			return t, nil
		}
	}
	return "", nil
}

const namespaces = `xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:a="http://apple.com/ns/ical/"`

func (c *Client) propfind(ctx context.Context, href, depth, props string) (*multistatus, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><d:propfind ` + namespaces + `>` + props + `</d:propfind>`
	resp, err := c.do(ctx, "PROPFIND", href, body, "application/xml; charset=utf-8", http.Header{"Depth": {depth}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := expect(resp, http.StatusMultiStatus); err != nil {
		return nil, fmt.Errorf("PROPFIND %s: %w", href, err)
	}
	return parseMultistatus(resp.Body)
}

func (c *Client) resolve(href string) *url.URL {
	ref, err := url.Parse(href)
	if err != nil {
		return c.base
	}
	return c.base.ResolveReference(ref)
}

func (c *Client) do(ctx context.Context, method, href, body, contentType string, h http.Header) (*http.Response, error) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.resolve(href).String(), r)
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Davis's Caddy compresses answers over 512 bytes and then appends "-gzip" to the ETag header, which If-Match
	// no longer accepts (412). Uncompressed answers keep the real ETag.
	req.Header.Set("Accept-Encoding", "identity")
	req.SetBasicAuth(c.user, c.password)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("davis: %s %s: %w", method, href, err)
	}
	return resp, nil
}

func expect(resp *http.Response, codes ...int) error {
	for _, code := range codes {
		if resp.StatusCode == code {
			return nil
		}
	}
	switch resp.StatusCode {
	case http.StatusPreconditionFailed:
		return ErrPrecondition
	case http.StatusNotFound:
		return ErrNotFound
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
}

// SamePath tells whether two hrefs name the same resource (one may be percent-encoded differently).
func SamePath(a, b string) bool { return cleanPath(a) == cleanPath(b) }

func cleanPath(p string) string {
	if u, err := url.Parse(p); err == nil {
		p = u.Path // decoded; drops any scheme and host
	}
	return strings.TrimSuffix(p, "/")
}
