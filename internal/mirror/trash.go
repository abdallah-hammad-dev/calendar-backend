package mirror

import (
	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

// The trash (spec §5.4): a copy I delete on Davis, from any client, is never written again until I restore it from
// the accounts page; a date I delete from a series (the EXDATE a client adds, iOS and the extension alike) is
// written back on every copy until then. Nothing goes to the provider: deleting hides, it doesn't decline.

// isMaster tells a series' main component: no RECURRENCE-ID, and a rule or dates that repeat it.
func isMaster(ev *ical.Component) bool {
	return ics.RecurrenceID(ev) == "" && (len(ev.Props[ical.PropRecurrenceRule]) > 0 || len(ev.Props[ical.PropRecurrenceDates]) > 0)
}

// excluded is every date a calendar's series leave out, by instant.
func excluded(cal *ical.Calendar) map[string]bool {
	out := map[string]bool{}
	for _, ev := range ics.Events(cal) {
		if !isMaster(ev) {
			continue
		}
		for i := range ev.Props[ical.PropExceptionDates] {
			p := &ev.Props[ical.PropExceptionDates][i]
			for _, v := range ics.Values(p) {
				out[ics.Instant(p, v)] = true
			}
		}
	}
	return out
}

// AddedExdates are the dates a client deleted from the series in cur, the Davis copy, that base, the copy built
// from the source (and what is already in the trash), doesn't leave out: one EXDATE content line per value, in the
// form the client wrote it.
func AddedExdates(base, cur *ical.Calendar) []string {
	had := excluded(base)
	var out []string
	for _, ev := range ics.Events(cur) {
		if !isMaster(ev) {
			continue
		}
		for i := range ev.Props[ical.PropExceptionDates] {
			p := &ev.Props[ical.PropExceptionDates][i]
			for _, v := range ics.Values(p) {
				key := ics.Instant(p, v)
				if had[key] {
					continue
				}
				had[key] = true
				one := *p
				one.Value = v
				out = append(out, ics.ContentLine(&one))
			}
		}
	}
	return out
}

// HideOccurrences leaves the dates in the trash out of cal's series: an EXDATE for each one the master doesn't
// already leave out, and no override for it. Lines that don't parse are skipped.
func HideOccurrences(cal *ical.Calendar, lines []string) {
	if len(lines) == 0 {
		return
	}
	var props []*ical.Prop
	keys := map[string]bool{}
	for _, l := range lines {
		p, err := ics.Line(l)
		if err != nil || p.Name != ical.PropExceptionDates {
			continue
		}
		for _, v := range ics.Values(p) {
			keys[ics.Instant(p, v)] = true
		}
		props = append(props, p)
	}
	if len(props) == 0 {
		return
	}
	var kept []*ical.Component
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent {
			if rid := c.Props.Get(ical.PropRecurrenceID); rid != nil && keys[ics.Instant(rid, rid.Value)] {
				continue
			}
		}
		kept = append(kept, c)
	}
	cal.Children = kept
	had := excluded(cal)
	for _, ev := range ics.Events(cal) {
		if !isMaster(ev) {
			continue
		}
		for _, p := range props {
			if v := ics.Values(p); len(v) == 1 && !had[ics.Instant(p, v[0])] {
				had[ics.Instant(p, v[0])] = true
				ev.Props.Add(p)
			}
		}
	}
}

// StillRemoved keeps the dates in the trash that the source, src, still has: the series is still one, and doesn't
// leave that date out itself (cancelled by its organizer, it has nothing left to restore).
func StillRemoved(src *ical.Calendar, removed []store.Removed) []store.Removed {
	if len(removed) == 0 {
		return removed
	}
	series := false
	for _, ev := range ics.Events(src) {
		series = series || isMaster(ev)
	}
	if !series {
		return nil
	}
	had := excluded(src)
	var out []store.Removed
	for _, r := range removed {
		p, err := ics.Line(r.Line)
		if err != nil {
			continue
		}
		if v := ics.Values(p); len(v) == 1 && !had[ics.Instant(p, v[0])] {
			out = append(out, r)
		}
	}
	return out
}
