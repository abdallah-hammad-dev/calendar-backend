package davis

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

const (
	nsDAV    = "DAV:"
	nsCalDAV = "urn:ietf:params:xml:ns:caldav"
	nsApple  = "http://apple.com/ns/ical/"
	nsCS     = "http://calendarserver.org/ns/"
)

// elem is any XML element, kept whole: WebDAV answers are read by namespace + name, whatever the prefixes.
type elem struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []elem     `xml:",any"`
}

func (e *elem) child(ns, local string) *elem {
	for i := range e.Children {
		if e.Children[i].XMLName.Space == ns && e.Children[i].XMLName.Local == local {
			return &e.Children[i]
		}
	}
	return nil
}

func (e *elem) children(ns, local string) []*elem {
	var out []*elem
	for i := range e.Children {
		if e.Children[i].XMLName.Space == ns && e.Children[i].XMLName.Local == local {
			out = append(out, &e.Children[i])
		}
	}
	return out
}

func (e *elem) attr(local string) string {
	for _, a := range e.Attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

func (e *elem) text() string { return strings.TrimSpace(e.Text) }

// response is one <d:response> of a multistatus: its href, its status if it has one (removed members in a
// sync-collection report), and the properties found (200 propstats only).
type response struct {
	Href   string
	Status int
	Props  map[xml.Name]*elem
}

func (r *response) prop(ns, local string) *elem { return r.Props[xml.Name{Space: ns, Local: local}] }

// propText is a property's text, "" if it is missing.
func (r *response) propText(ns, local string) string {
	if p := r.prop(ns, local); p != nil {
		return p.text()
	}
	return ""
}

// propHref is the first <d:href> inside a property (current-user-principal, calendar-home-set), "" if missing.
func (r *response) propHref(ns, local string) string {
	if p := r.prop(ns, local); p != nil {
		if h := p.child(nsDAV, "href"); h != nil {
			return h.text()
		}
	}
	return ""
}

type multistatus struct {
	Responses []response
	SyncToken string
}

func parseMultistatus(body io.Reader) (*multistatus, error) {
	var root elem
	if err := xml.NewDecoder(body).Decode(&root); err != nil {
		return nil, fmt.Errorf("davis: bad multistatus: %w", err)
	}
	if root.XMLName.Space != nsDAV || root.XMLName.Local != "multistatus" {
		return nil, fmt.Errorf("davis: expected a multistatus, got <%s>", root.XMLName.Local)
	}
	ms := &multistatus{}
	if t := root.child(nsDAV, "sync-token"); t != nil {
		ms.SyncToken = t.text()
	}
	for _, r := range root.children(nsDAV, "response") {
		h := r.child(nsDAV, "href")
		if h == nil {
			continue
		}
		resp := response{Href: h.text(), Props: map[xml.Name]*elem{}}
		if s := r.child(nsDAV, "status"); s != nil {
			resp.Status = statusCode(s.text())
		}
		for _, ps := range r.children(nsDAV, "propstat") {
			s := ps.child(nsDAV, "status")
			if s == nil || statusCode(s.text()) != 200 {
				continue
			}
			if p := ps.child(nsDAV, "prop"); p != nil {
				for i := range p.Children {
					resp.Props[p.Children[i].XMLName] = &p.Children[i]
				}
			}
		}
		ms.Responses = append(ms.Responses, resp)
	}
	return ms, nil
}

// statusCode reads "HTTP/1.1 404 Not Found" → 404 (0 if unreadable).
func statusCode(s string) int {
	f := strings.Fields(s)
	if len(f) < 2 {
		return 0
	}
	var n int
	fmt.Sscanf(f[1], "%d", &n)
	return n
}

func escapeXML(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
