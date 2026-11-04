package connector

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

// iCalendar as the extension writes it (its spec §6.2): items edited in place (alarms, X-APPLE-* and unknown
// properties kept), times with TZID + VTIMEZONE, a rule's last day (UNTIL) a date where I am.

const (
	dayLayout   = "2006-01-02"
	timeLayout  = "2006-01-02T15:04"
	icalDay     = "20060102"
	icalLocal   = "20060102T150405"
	icalUTC     = "20060102T150405Z"
	sourceProp  = "X-CALTASKS-SOURCE-ACCOUNT"
	attendeeOf  = "X-CALTASKS-SOURCE-ATTENDEE"
	rsvpStateOf = "X-CALTASKS-RSVP-STATE"
)

func parse(data string) (*ical.Calendar, error) { return ics.Parse(data) }

func components(cal *ical.Calendar, name string) []*ical.Component {
	var out []*ical.Component
	for _, c := range cal.Children {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// master is the series (or the only one) of an item: the component without RECURRENCE-ID.
func master(cal *ical.Calendar, name string) *ical.Component {
	list := components(cal, name)
	for _, c := range list {
		if c.Props.Get(ical.PropRecurrenceID) == nil {
			return c
		}
	}
	if len(list) > 0 {
		return list[0]
	}
	return nil
}

func text(c *ical.Component, prop string) string {
	v, _ := c.Props.Text(prop)
	return strings.TrimSpace(v)
}

func newUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// --- local times

// when is a time as the tools say it: a whole day, or a date-time in my zone.
type when struct {
	t   time.Time // in loc; midnight for a day
	day bool
}

func (c *Connector) parseWhen(s, what string) (when, error) {
	s = strings.TrimSpace(s)
	if t, err := time.ParseInLocation(dayLayout, s, c.Loc); err == nil {
		return when{t: t, day: true}, nil
	}
	for _, l := range []string{timeLayout, "2006-01-02T15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(l, s, c.Loc); err == nil {
			return when{t: t.Truncate(time.Minute)}, nil
		}
	}
	return when{}, refuse(`%s: %q isn't "YYYY-MM-DD" (a whole day) or "YYYY-MM-DDTHH:mm" (local time)`, what, s)
}

func (w when) String() string {
	if w.day {
		return w.t.Format(dayLayout)
	}
	return w.t.Format(timeLayout)
}

// localOf reads a date or date-time property as a local value ("" if none).
func (c *Connector) localOf(p *ical.Prop) (when, bool) {
	if p == nil || p.Value == "" {
		return when{}, false
	}
	if isDate(p) {
		t, err := time.ParseInLocation(icalDay, p.Value[:8], c.Loc)
		return when{t: t, day: true}, err == nil
	}
	t, err := p.DateTime(c.Loc)
	if err != nil {
		// A TZID this server doesn't know (Outlook's names): the wall clock as written.
		if t, err = time.ParseInLocation(icalLocal, strings.TrimSuffix(p.Value, "Z"), c.Loc); err != nil {
			return when{}, false
		}
	}
	return when{t: t.In(c.Loc)}, true
}

func isDate(p *ical.Prop) bool {
	return strings.EqualFold(p.Params.Get(ical.ParamValue), "DATE") || len(p.Value) == 8
}

// setWhen writes a date property: VALUE=DATE for a day, else TZID with the zone's VTIMEZONE in the calendar.
func (c *Connector) setWhen(cal *ical.Calendar, comp *ical.Component, prop string, w when) {
	if w.day {
		comp.Props.Set(ics.Prop(prop, w.t.Format(icalDay), ical.ParamValue, "DATE"))
		return
	}
	comp.Props.Set(ics.DateTime(prop, w.t, c.Loc))
	c.ensureZone(cal, w.t)
}

func (c *Connector) ensureZone(cal *ical.Calendar, at time.Time) {
	for _, z := range components(cal, ical.CompTimezone) {
		if ics.Value(z, ical.PropTimezoneID) == c.Loc.String() {
			return
		}
	}
	cal.Children = append([]*ical.Component{ics.VTimezone(c.Loc, at)}, cal.Children...)
}

func touch(comp *ical.Component) {
	now := time.Now().UTC().Format(icalUTC)
	comp.Props.Set(ics.Prop(ical.PropDateTimeStamp, now))
	comp.Props.Set(ics.Prop(ical.PropLastModified, now))
}

// bump is touch for a change clients must take as new (an event's times, title…): SEQUENCE goes up.
func bump(comp *ical.Component) {
	seq := 0
	if p := comp.Props.Get(ical.PropSequence); p != nil {
		fmt.Sscan(p.Value, &seq)
	}
	comp.Props.Set(ics.Prop(ical.PropSequence, fmt.Sprint(seq+1)))
	touch(comp)
}

func newCalendar() *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.Set(ics.Prop(ical.PropVersion, "2.0"))
	cal.Props.Set(ics.Prop(ical.PropProductID, "-//calendar-backend//Claude connector//EN"))
	return cal
}

// --- rules

var untilPart = regexp.MustCompile(`(?i)(^|;)UNTIL=([0-9TZ]+)`)

// ruleIn checks a rule Claude gives (an RRULE value, "RRULE:" optional) and writes its UNTIL in the start's form
// (RFC 5545 §3.3.10): a date for a whole day, else UTC, where a date says the last day where I am (as the
// extension's rules do).
func (c *Connector) ruleIn(rule string, start when) (string, error) {
	r := strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(rule)), "RRULE:"))
	if r == "" {
		return "", nil
	}
	if m := untilPart.FindStringSubmatch(r); m != nil {
		var last time.Time
		var err error
		v := m[2]
		switch {
		case len(v) == 8:
			last, err = time.ParseInLocation(icalDay, v, c.Loc)
			last = last.Add(24*time.Hour - time.Second)
		case strings.HasSuffix(v, "Z"):
			last, err = time.Parse(icalUTC, v)
		default:
			last, err = time.ParseInLocation(icalLocal, v, c.Loc)
		}
		if err != nil {
			return "", refuse("repeat: UNTIL=%s isn't a date (YYYYMMDD, the last day)", v)
		}
		u := last.UTC().Format(icalUTC)
		if start.day {
			u = last.In(c.Loc).Format(icalDay)
		}
		r = untilPart.ReplaceAllString(r, "${1}UNTIL="+u)
	}
	if _, err := rrule.StrToROption(r); err != nil {
		return "", refuse("repeat: %q isn't an RRULE (as FREQ=WEEKLY;BYDAY=MO,TH or FREQ=MONTHLY;INTERVAL=2;UNTIL=20270630): %v", rule, err)
	}
	return r, nil
}

// ruleOut is a rule as the tools say it: its UNTIL the last day where I am.
func (c *Connector) ruleOut(rule string) string {
	m := untilPart.FindStringSubmatch(rule)
	if m == nil {
		return rule
	}
	v := m[2]
	day := v
	if strings.HasSuffix(v, "Z") {
		if t, err := time.Parse(icalUTC, v); err == nil {
			day = t.In(c.Loc).Format(icalDay)
		}
	} else if len(v) > 8 {
		day = v[:8]
	}
	return untilPart.ReplaceAllString(rule, "${1}UNTIL="+day)
}

func (c *Connector) ruleOf(comp *ical.Component) string {
	if p := comp.Props.Get(ical.PropRecurrenceRule); p != nil {
		return c.ruleOut(p.Value)
	}
	return ""
}

// --- recurrence ids

// rid is a RECURRENCE-ID as the extension keeps it (in its event notes, and here between calls): the UTC instant
// ("20261102T090000Z"), or the day of an all-day series ("20261102").
func rid(p *ical.Prop) string {
	if p == nil {
		return ""
	}
	v := ics.Instant(p, p.Value)
	if i := strings.LastIndexByte(v, ':'); i >= 0 {
		v = v[i+1:] // a zone unknown here: as written
	}
	return v
}

func (c *Connector) normRID(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if _, err := time.Parse(icalDay, s); err == nil && len(s) == 8 {
		return s, nil
	}
	if t, err := time.Parse(icalUTC, s); err == nil {
		return t.Format(icalUTC), nil
	}
	if w, err := c.parseWhen(s, "recurrenceId"); err == nil {
		if w.day {
			return w.t.Format(icalDay), nil
		}
		return w.t.UTC().Format(icalUTC), nil
	}
	return "", refuse("recurrenceId %q: use the one calendar_list gives (as 20261102T090000Z)", s)
}

// exception is the component for one occurrence of a series: its exception, or a new one cloned from the series
// (RECURRENCE-ID in DTSTART's form, at the occurrence's own times).
func (c *Connector) exception(cal *ical.Calendar, series *ical.Component, id string) *ical.Component {
	for _, e := range components(cal, series.Name) {
		if e != series && rid(e.Props.Get(ical.PropRecurrenceID)) == id {
			return e
		}
	}
	ex := clone(series)
	for _, p := range []string{ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates, ical.PropRecurrenceID} {
		ex.Props.Del(p)
	}
	start := series.Props.Get(ical.PropDateTimeStart)
	ridProp := atLike(start, id, c.Loc, ical.PropRecurrenceID)
	ex.Props.Set(ridProp)
	startProp := *ridProp
	startProp.Name = ical.PropDateTimeStart
	ex.Props.Set(&startProp)
	at, _ := c.localOf(ridProp)
	s, okS := c.localOf(start)
	endProp := series.Props.Get(ical.PropDateTimeEnd)
	if e, okE := c.localOf(endProp); okS && okE {
		if at.day {
			days := int(e.t.Sub(s.t).Hours()/24 + 0.5)
			c.setWhen(cal, ex, ical.PropDateTimeEnd, when{t: at.t.AddDate(0, 0, days), day: true})
		} else {
			ex.Props.Set(likeProp(endProp, at.t.Add(e.t.Sub(s.t)), c.Loc))
		}
	}
	cal.Children = append(cal.Children, ex)
	return ex
}

// atLike is a date property named name for the occurrence id, in the form of like (same zone, or a date).
func atLike(like *ical.Prop, id string, loc *time.Location, name string) *ical.Prop {
	if len(id) == 8 {
		return ics.Prop(name, id, ical.ParamValue, "DATE")
	}
	t, _ := time.Parse(icalUTC, id)
	p := likeProp(like, t, loc)
	p.Name = name
	return p
}

// likeProp is like (its name and zone) at t.
func likeProp(like *ical.Prop, t time.Time, loc *time.Location) *ical.Prop {
	if like == nil {
		return ics.DateTime(ical.PropDateTimeStart, t, loc)
	}
	if tzid := like.Params.Get(ical.PropTimezoneID); tzid != "" {
		if z := ics.Zone(tzid); z != nil {
			return ics.Prop(like.Name, t.In(z).Format(icalLocal), ical.PropTimezoneID, tzid)
		}
	}
	if strings.HasSuffix(like.Value, "Z") {
		return ics.Prop(like.Name, t.UTC().Format(icalUTC))
	}
	return ics.Prop(like.Name, t.In(loc).Format(icalLocal))
}

func clone(c *ical.Component) *ical.Component {
	out := ical.NewComponent(c.Name)
	for name, ps := range c.Props {
		for _, p := range ps {
			q := ical.Prop{Name: p.Name, Value: p.Value, Params: ical.Params{}}
			for k, v := range p.Params {
				q.Params[k] = append([]string(nil), v...)
			}
			out.Props[name] = append(out.Props[name], q)
		}
	}
	for _, ch := range c.Children {
		out.Children = append(out.Children, clone(ch))
	}
	return out
}
