package adapter

import (
	"strings"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

// Meeting writes an event's place, notes and video meeting (spec §5.2). The link always goes in CONFERENCE (what
// the extension reads first). For the iPhone, whose Calendar may not read CONFERENCE (untested), it also goes in the
// place when the source has none (the iPhone's "Location or Video Call" field), else in the notes when they don't
// hold it already: a room in the place is never replaced. place and notes are the source's ("" = none), link "" = no
// meeting.
func Meeting(ev *ical.Component, link, label, place, notes string) {
	if link != "" {
		ev.Props.Add(ics.Prop(ical.PropConference, link, ical.ParamValue, string(ical.ValueURI), "FEATURE", "VIDEO",
			"LABEL", label))
		switch {
		case place == "":
			place = link
		case strings.Contains(place, link) || strings.Contains(notes, link):
		case notes == "":
			notes = label + ": " + link
		default:
			notes += "\n\n" + label + ": " + link
		}
	}
	if place != "" {
		ics.SetText(ev, ical.PropLocation, place)
	}
	if notes != "" {
		ics.SetText(ev, ical.PropDescription, notes)
	}
}
