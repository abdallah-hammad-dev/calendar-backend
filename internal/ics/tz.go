package ics

import (
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

const (
	localFormat = "20060102T150405"
	utcFormat   = "20060102T150405Z"
)

// Localize applies the time rule shared with the extension (spec §3, extension spec §6.2): times are written with
// TZID + VTIMEZONE, never in UTC, or the iPhone shows a second "(GMT)" time.
//
//   - DTSTART / DTEND in UTC become wall-clock times in loc, except on a recurring master: there UTC means "the
//     same instant every time", which a TZID would shift across DST changes, so it is left as is. RECURRENCE-ID,
//     EXDATE, RDATE and RRULE's UNTIL are never touched (they must keep matching the master).
//   - Every TZID used gets a VTIMEZONE: the source's own if it gave one, else one generated from Go's zone database.
//
// It returns the TZIDs that have no definition and aren't in the zone database (left as they are).
func Localize(cal *ical.Calendar, loc *time.Location) []string {
	for _, ev := range Events(cal) {
		recurring := RecurrenceID(ev) == "" && (len(ev.Props[ical.PropRecurrenceRule]) > 0 || len(ev.Props[ical.PropRecurrenceDates]) > 0)
		if recurring {
			continue
		}
		for _, name := range []string{ical.PropDateTimeStart, ical.PropDateTimeEnd} {
			for i := range ev.Props[name] {
				p := &ev.Props[name][i]
				if p.Params.Get(ical.PropTimezoneID) != "" || !strings.HasSuffix(p.Value, "Z") {
					continue
				}
				t, err := time.ParseInLocation(utcFormat, p.Value, time.UTC)
				if err != nil {
					continue
				}
				p.Value = t.In(loc).Format(localFormat)
				p.Params.Set(ical.PropTimezoneID, loc.String())
			}
		}
	}

	defined := map[string]bool{}
	for _, c := range cal.Children {
		if c.Name == ical.CompTimezone {
			defined[Value(c, ical.PropTimezoneID)] = true
		}
	}
	used := map[string]time.Time{} // TZID → a time it is used at (the generated rules are the ones in force then)
	var order []string
	for _, ev := range Events(cal) {
		for _, props := range ev.Props {
			for _, p := range props {
				tzid := p.Params.Get(ical.PropTimezoneID)
				if tzid == "" || defined[tzid] {
					continue
				}
				if _, ok := used[tzid]; !ok {
					order = append(order, tzid)
					used[tzid] = time.Time{}
				}
				if t, err := time.Parse(localFormat, strings.SplitN(p.Value, ",", 2)[0]); err == nil && used[tzid].IsZero() {
					used[tzid] = t
				}
			}
		}
	}
	var unknown []string
	var zones []*ical.Component
	for _, tzid := range order {
		zl, err := time.LoadLocation(tzid)
		if err != nil || tzid == "Local" {
			unknown = append(unknown, tzid)
			continue
		}
		at := used[tzid]
		if at.IsZero() {
			at = time.Now()
		}
		zones = append(zones, VTimezone(zl, at))
	}
	if len(zones) > 0 {
		cal.Children = append(zones, cal.Children...)
	}
	return unknown
}

// VTimezone describes loc as an iCalendar VTIMEZONE: the offsets and DST rules in force in the year of at, written
// as yearly rules from 1970 (as Apple and the extension do), e.g. "last Sunday of March". If the rules can't be
// said that way, the transitions of a few years around at are listed one by one instead.
func VTimezone(loc *time.Location, at time.Time) *ical.Component {
	tz := ical.NewComponent(ical.CompTimezone)
	tz.Props.SetText(ical.PropTimezoneID, loc.String())
	year := at.Year()

	trans := transitions(loc, year)
	if len(trans) == 0 {
		t := time.Date(year, 1, 1, 0, 0, 0, 0, loc)
		name, off := t.Zone()
		tz.Children = append(tz.Children, observance(ical.CompTimezoneStandard, "19700101T000000", off, off, name, ""))
		return tz
	}

	// Yearly rules, if each transition is the same nth (or last) weekday of its month the year after.
	var rules []*ical.Component
	next := transitions(loc, year+1)
	if len(next) == len(trans) {
		for i, tr := range trans {
			rule, ok := weekdayRule(tr.wall)
			if !ok || next[i].wall.Format("0102 1504") != ruleDate(year+1, tr.wall, rule).Format("0102 1504") {
				rules = nil
				break
			}
			start := ruleDate(1970, tr.wall, rule).Format(localFormat)
			rules = append(rules, observance(tr.kind(), start, tr.from, tr.to, tr.name,
				fmt.Sprintf("FREQ=YEARLY;BYMONTH=%d;BYDAY=%s", int(tr.wall.Month()), rule)))
		}
	}
	if rules != nil {
		tz.Children = rules
		return tz
	}
	first := time.Date(year-1, 1, 1, 0, 0, 0, 0, loc)
	name, off := first.Zone()
	tz.Children = append(tz.Children, observance(ical.CompTimezoneStandard, "19700101T000000", off, off, name, ""))
	for y := year - 1; y <= year+5; y++ {
		for _, tr := range transitions(loc, y) {
			tz.Children = append(tz.Children, observance(tr.kind(), tr.wall.Format(localFormat), tr.from, tr.to, tr.name, ""))
		}
	}
	return tz
}

type transition struct {
	wall     time.Time // the wall-clock time it happens at, read in the offset before it
	from, to int       // offsets in seconds
	name     string    // the abbreviation after it (CEST)
	dst      bool      // the offset after it is the larger one of the year
}

func (t transition) kind() string {
	if t.dst {
		return ical.CompTimezoneDaylight
	}
	return ical.CompTimezoneStandard
}

// transitions lists the offset changes of loc during year, in order.
func transitions(loc *time.Location, year int) []transition {
	var out []transition
	t := time.Date(year, 1, 1, 0, 0, 0, 0, loc)
	end := time.Date(year+1, 1, 1, 0, 0, 0, 0, loc)
	for {
		_, zoneEnd := t.ZoneBounds()
		if zoneEnd.IsZero() || !zoneEnd.Before(end) {
			break
		}
		_, from := t.Zone()
		name, to := zoneEnd.Zone()
		if from != to {
			out = append(out, transition{
				wall: zoneEnd.UTC().Add(time.Duration(from) * time.Second).Truncate(time.Minute),
				from: from, to: to, name: name,
			})
		}
		t = zoneEnd
	}
	for i := range out {
		out[i].dst = out[i].to > out[i].from
	}
	return out
}

var weekdays = []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

// weekdayRule says on which weekday of its month t falls: "2SU" (second Sunday) or "-1SU" (last Sunday).
func weekdayRule(t time.Time) (string, bool) {
	day := t.Day()
	daysInMonth := time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	wd := weekdays[t.Weekday()]
	if day+7 > daysInMonth {
		return "-1" + wd, true
	}
	n := (day-1)/7 + 1
	if n > 4 {
		return "", false
	}
	return fmt.Sprintf("%d%s", n, wd), true
}

// ruleDate is the day a weekday rule ("2SU", "-1SU") gives in year, at the month and time of like.
func ruleDate(year int, like time.Time, rule string) time.Time {
	var n int
	var wd string
	fmt.Sscanf(rule[:len(rule)-2], "%d", &n)
	wd = rule[len(rule)-2:]
	want := 0
	for i, w := range weekdays {
		if w == wd {
			want = i
		}
	}
	if n > 0 {
		d := time.Date(year, like.Month(), 1, like.Hour(), like.Minute(), 0, 0, time.UTC)
		d = d.AddDate(0, 0, (want-int(d.Weekday())+7)%7)
		return d.AddDate(0, 0, 7*(n-1))
	}
	d := time.Date(year, like.Month()+1, 0, like.Hour(), like.Minute(), 0, 0, time.UTC) // last day of the month
	return d.AddDate(0, 0, -((int(d.Weekday()) - want + 7) % 7))
}

func observance(kind, start string, from, to int, name, rrule string) *ical.Component {
	c := ical.NewComponent(kind)
	c.Props.Set(&ical.Prop{Name: ical.PropDateTimeStart, Params: ical.Params{}, Value: start})
	c.Props.Set(&ical.Prop{Name: ical.PropTimezoneOffsetFrom, Params: ical.Params{}, Value: offset(from)})
	c.Props.Set(&ical.Prop{Name: ical.PropTimezoneOffsetTo, Params: ical.Params{}, Value: offset(to)})
	if name != "" {
		c.Props.SetText(ical.PropTimezoneName, name)
	}
	if rrule != "" {
		c.Props.Set(&ical.Prop{Name: ical.PropRecurrenceRule, Params: ical.Params{}, Value: rrule})
	}
	return c
}

func offset(sec int) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	s := fmt.Sprintf("%s%02d%02d", sign, sec/3600, sec/60%60)
	if sec%60 != 0 {
		s += fmt.Sprintf("%02d", sec%60)
	}
	return s
}
