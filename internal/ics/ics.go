// Package ics reads and writes iCalendar data (RFC 5545) on top of go-ical's model.
//
// Decoding is go-ical's. Encoding is ours: go-ical's encoder doesn't fold long lines and refuses some items that
// clients do write, and the backend must be able to write back whatever Davis holds.
package ics

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-ical"
)

// ProdID is the PRODID of the objects the backend writes.
const ProdID = "-//organisation//calendar-backend//EN"

// Parse decodes one iCalendar object. go-ical panics on some malformed parameters; that comes back as an error.
func Parse(data string) (cal *ical.Calendar, err error) {
	defer func() {
		if r := recover(); r != nil {
			cal, err = nil, fmt.Errorf("ics: malformed data: %v", r)
		}
	}()
	cal, err = ical.NewDecoder(strings.NewReader(data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("ics: %w", err)
	}
	return cal, nil
}

// Encode writes cal with CRLF line ends, lines folded at 75 octets, properties sorted by name (deterministic, so
// two encodings of the same content compare equal).
func Encode(cal *ical.Calendar) string {
	var b bytes.Buffer
	encodeComponent(&b, cal.Component)
	return b.String()
}

func encodeComponent(b *bytes.Buffer, c *ical.Component) {
	writeLine(b, "BEGIN:"+c.Name)
	names := make([]string, 0, len(c.Props))
	for name := range c.Props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, p := range c.Props[name] {
			writeLine(b, contentLine(&p))
		}
	}
	for _, child := range c.Children {
		encodeComponent(b, child)
	}
	writeLine(b, "END:"+c.Name)
}

func contentLine(p *ical.Prop) string {
	var s strings.Builder
	s.WriteString(p.Name)
	params := make([]string, 0, len(p.Params))
	for name := range p.Params {
		params = append(params, name)
	}
	sort.Strings(params)
	for _, name := range params {
		s.WriteString(";" + name + "=")
		for i, v := range p.Params[name] {
			if i > 0 {
				s.WriteString(",")
			}
			v = strings.ReplaceAll(v, `"`, "'") // a parameter value can't hold a double quote (RFC 5545 §3.1)
			if strings.ContainsAny(v, ";:,") {
				v = `"` + v + `"`
			}
			s.WriteString(v)
		}
	}
	s.WriteString(":")
	// A raw value never holds a line break (TEXT escapes it as \n); drop any, rather than write a broken line.
	s.WriteString(strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(p.Value))
	return s.String()
}

// writeLine folds at 75 octets without splitting a UTF-8 sequence (RFC 5545 §3.1).
func writeLine(b *bytes.Buffer, line string) {
	limit := 75
	for len(line) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		b.WriteString(line[:cut])
		b.WriteString("\r\n ")
		line = line[cut:]
		limit = 74 // the leading space counts
	}
	b.WriteString(line)
	b.WriteString("\r\n")
}

// SetText sets a TEXT property, escaped, without the VALUE=TEXT parameter go-ical adds to properties it doesn't
// know (all X- ones).
func SetText(c *ical.Component, name, text string) {
	p := ical.NewProp(name)
	p.SetText(text)
	p.Params.Del(ical.ParamValue)
	c.Props.Set(p)
}

// Address is a calendar user address compared the way the extension does: no "mailto:", trimmed, lower case.
func Address(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 7 && strings.EqualFold(v[:7], "mailto:") {
		v = v[7:]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// Mailto is the property value for an address.
func Mailto(addr string) string { return "mailto:" + Address(addr) }

// Events returns the VEVENT components (master and overrides) of cal.
func Events(cal *ical.Calendar) []*ical.Component {
	var out []*ical.Component
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent {
			out = append(out, c)
		}
	}
	return out
}

// Value is the value of the first name property of c, or "".
func Value(c *ical.Component, name string) string {
	if p := c.Props.Get(name); p != nil {
		return p.Value
	}
	return ""
}

// RecurrenceID is the component's RECURRENCE-ID value ("" for a master): what pairs an override in two copies.
func RecurrenceID(c *ical.Component) string { return Value(c, ical.PropRecurrenceID) }

// Attendee returns the ATTENDEE property of c whose address is one of addrs, or nil.
func Attendee(c *ical.Component, addrs ...string) *ical.Prop {
	for i, p := range c.Props[ical.PropAttendee] {
		for _, a := range addrs {
			if a != "" && Address(p.Value) == Address(a) {
				return &c.Props[ical.PropAttendee][i]
			}
		}
	}
	return nil
}

// Partstat is an attendee's answer, NEEDS-ACTION when unset (RFC 5545 §3.2.12).
func Partstat(p *ical.Prop) string {
	if v := strings.ToUpper(p.Params.Get(ical.ParamParticipationStatus)); v != "" {
		return v
	}
	return "NEEDS-ACTION"
}

// MyPartstat is my answer in an event: on the master if I'm invited there, else on the first override that has me.
// "" when I'm not an attendee.
func MyPartstat(cal *ical.Calendar, me string) string {
	answer := ""
	for _, ev := range Events(cal) {
		p := Attendee(ev, me)
		if p == nil {
			continue
		}
		if RecurrenceID(ev) == "" {
			return Partstat(p)
		}
		if answer == "" {
			answer = Partstat(p)
		}
	}
	return answer
}
