package mirror

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

func exdates(cal *ical.Calendar) []string {
	var out []string
	for _, ev := range ics.Events(cal) {
		for i := range ev.Props[ical.PropExceptionDates] {
			out = append(out, ics.ContentLine(&ev.Props[ical.PropExceptionDates][i]))
		}
	}
	return out
}

func TestAddedExdates(t *testing.T) {
	base := Base(parse(t, source), "me@gmail.com", "", target)
	cur := Base(parse(t, source), "me@gmail.com", "", target)
	master := ics.Events(cur)[0]
	// As iOS deletes one date: an EXDATE in the master's form. The source's own (UTC) is the same date said
	// another way, and isn't a deletion.
	master.Props.Add(ics.Prop(ical.PropExceptionDates, "20261106T090000Z"))
	master.Props.Add(ics.Prop(ical.PropExceptionDates, "20261113T100000", ical.PropTimezoneID, "Europe/Paris"))
	ics.Events(base)[0].Props.Add(ics.Prop(ical.PropExceptionDates, "20261106T090000Z"))

	got := AddedExdates(base, cur)
	if len(got) != 1 || got[0] != "EXDATE;TZID=Europe/Paris:20261113T100000" {
		t.Fatalf("added %q", got)
	}
	if got := AddedExdates(base, base); len(got) != 0 {
		t.Fatalf("nothing deleted, found %q", got)
	}
}

func TestHideOccurrences(t *testing.T) {
	cal := Base(parse(t, source), "me@gmail.com", "", target)
	// The moved date (its override goes) and another one, said in UTC and in Paris time: one EXDATE each.
	lines := []string{"EXDATE:20261030T090000Z", "EXDATE;TZID=Europe/Paris:20261106T100000", "EXDATE:20261106T090000Z", "not a line"}
	HideOccurrences(cal, lines)
	if n := len(ics.Events(cal)); n != 1 {
		t.Fatalf("%d events: the override of a deleted date stays", n)
	}
	if got := exdates(cal); strings.Join(got, "|") != "EXDATE:20261030T090000Z|EXDATE;TZID=Europe/Paris:20261106T100000" {
		t.Fatalf("EXDATEs %q", got)
	}
	again := ics.Encode(cal)
	HideOccurrences(cal, lines)
	if ics.Encode(cal) != again {
		t.Fatal("hidden twice")
	}
}

func TestStillRemoved(t *testing.T) {
	removed := []store.Removed{{Line: "EXDATE:20261106T090000Z", At: time.Now()}, {Line: "EXDATE:20261113T090000Z"}}
	src := parse(t, source)
	if got := StillRemoved(src, removed); len(got) != 2 {
		t.Fatalf("kept %v", got)
	}
	// The organizer cancels one of them: nothing left to restore there.
	ics.Events(src)[0].Props.Add(ics.Prop(ical.PropExceptionDates, "20261106T100000", ical.PropTimezoneID, "Europe/Paris"))
	if got := StillRemoved(src, removed); len(got) != 1 || got[0].Line != "EXDATE:20261113T090000Z" {
		t.Fatalf("kept %v", got)
	}
	// No longer a series.
	ics.Events(src)[0].Props.Del(ical.PropRecurrenceRule)
	if got := StillRemoved(src, removed); len(got) != 0 {
		t.Fatalf("kept %v", got)
	}
}
