package ics

import (
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// Prop is a property with its value as is (not escaped: for dates, addresses, URIs, rules) and parameters given as
// name, value pairs.
func Prop(name, value string, params ...string) *ical.Prop {
	p := &ical.Prop{Name: name, Params: ical.Params{}, Value: value}
	for i := 0; i+1 < len(params); i += 2 {
		if params[i+1] != "" {
			p.Params.Set(params[i], params[i+1])
		}
	}
	return p
}

// DateTime is a date-time property at t: TZID + the wall-clock time in loc, or UTC when loc is nil or UTC (left
// for Localize, or kept so on a recurring master: spec §5.2).
func DateTime(name string, t time.Time, loc *time.Location) *ical.Prop {
	if loc == nil || loc == time.UTC {
		return Prop(name, t.UTC().Format(utcFormat))
	}
	return Prop(name, t.In(loc).Format(localFormat), ical.PropTimezoneID, loc.String())
}

// Date is an all-day property on the day of day ("2026-10-24", or anything that starts with one).
func Date(name, day string) (*ical.Prop, error) {
	d, err := time.Parse("2006-01-02", firstN(day, 10))
	if err != nil {
		return nil, fmt.Errorf("ics: bad date %q", day)
	}
	return Prop(name, d.Format("20060102"), ical.ParamValue, string(ical.ValueDate)), nil
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Line parses one content line as a provider writes it ("RRULE:FREQ=WEEKLY", "EXDATE;TZID=Europe/Paris:…").
func Line(line string) (*ical.Prop, error) {
	cal, err := Parse("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n" + strings.TrimSpace(line) + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	if err != nil {
		return nil, err
	}
	for _, props := range cal.Children[0].Props {
		return &props[0], nil
	}
	return nil, fmt.Errorf("ics: not a content line: %q", line)
}

// Zone loads an IANA zone, nil when name is empty, "UTC" or unknown.
func Zone(name string) *time.Location {
	if name == "" || name == "Local" || strings.EqualFold(name, "UTC") {
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return loc
}
