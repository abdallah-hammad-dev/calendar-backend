package davis

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// calendar-query (RFC 4791 §7.8), for the Claude connector (spec §10.2), which reads what it needs when asked.

const davTime = "20060102T150405Z"

// Events are the collection's events that take place in [from, to), each expanded by sabre/dav: every occurrence
// of a series is a VEVENT of its own with its RECURRENCE-ID, EXDATEs left out, exceptions in place, times in UTC
// (all-day ones stay dates), alarms and unknown properties kept (bench/TESTS.md, 2026-11-04). Not for writing back.
func (c *Client) Events(ctx context.Context, coll string, from, to time.Time) ([]Item, error) {
	r := fmt.Sprintf(`start="%s" end="%s"`, from.UTC().Format(davTime), to.UTC().Format(davTime))
	return c.query(ctx, coll, `<c:calendar-data><c:expand `+r+`/></c:calendar-data>`,
		`<c:comp-filter name="VEVENT"><c:time-range `+r+`/></c:comp-filter>`)
}

// Todos are the collection's tasks, as stored.
func (c *Client) Todos(ctx context.Context, coll string) ([]Item, error) {
	return c.query(ctx, coll, `<c:calendar-data/>`, `<c:comp-filter name="VTODO"/>`)
}

// ByUID are the collection's items whose UID holds uid (text-match is a substring match: the caller checks).
func (c *Client) ByUID(ctx context.Context, coll, comp, uid string) ([]Item, error) {
	return c.query(ctx, coll, `<c:calendar-data/>`, `<c:comp-filter name="`+escapeXML(comp)+`">`+
		`<c:prop-filter name="UID"><c:text-match collation="i;octet">`+escapeXML(uid)+`</c:text-match></c:prop-filter>`+
		`</c:comp-filter>`)
}

func (c *Client) query(ctx context.Context, coll, data, filter string) ([]Item, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><c:calendar-query ` + namespaces + `>` +
		`<d:prop><d:getetag/>` + data + `</d:prop>` +
		`<c:filter><c:comp-filter name="VCALENDAR">` + filter + `</c:comp-filter></c:filter></c:calendar-query>`
	resp, err := c.do(ctx, "REPORT", coll, body, "application/xml; charset=utf-8", http.Header{"Depth": {"1"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := expect(resp, http.StatusMultiStatus); err != nil {
		return nil, fmt.Errorf("REPORT calendar-query %s: %w", coll, err)
	}
	ms, err := parseMultistatus(resp.Body)
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, r := range ms.Responses {
		if r.Status == http.StatusNotFound {
			continue
		}
		if data := r.propText(nsCalDAV, "calendar-data"); data != "" {
			out = append(out, Item{Href: r.Href, ETag: r.propText(nsDAV, "getetag"), Data: data})
		}
	}
	return out, nil
}
