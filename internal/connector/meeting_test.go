package connector

import (
	"testing"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

func TestMeetingLink(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "",
		" https://meet.google.com/abc ": "https://meet.google.com/abc",
		"Rejoindre Zoom : https://us02web.zoom.us/j/1?pwd=x.": "https://us02web.zoom.us/j/1?pwd=x",
		"meet.google.com/abc-defg-hij":                        "https://meet.google.com/abc-defg-hij",
	} {
		if got, err := meetingLink(in); err != nil || got != want {
			t.Errorf("meetingLink(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"salle 3", "lundi", "zoommtg://zoom.us/join"} {
		if _, err := meetingLink(in); err == nil {
			t.Errorf("meetingLink(%q) should refuse it", in)
		}
	}
	if meetingLabel("https://us02web.zoom.us/j/1") != "Zoom" || meetingLabel("https://discord.gg/x") != "Visio" {
		t.Error("labels: a service's name, else Visio")
	}
}

func TestSetMeeting(t *testing.T) {
	const meet, zoom = "https://meet.google.com/abc-defg-hij", "https://us02web.zoom.us/j/1"
	videos := func(ev *ical.Component) (out []string) {
		for _, p := range ev.Props[ical.PropConference] {
			out = append(out, p.Params.Get("FEATURE")+" "+p.Value)
		}
		return out
	}
	// None: the place is its link (the iPhone's "Location or Video Call").
	ev := ical.NewComponent(ical.CompEvent)
	setMeeting(ev, meet, "")
	if got := videos(ev); len(got) != 1 || got[0] != "VIDEO "+meet || text(ev, ical.PropLocation) != meet || currentMeeting(ev) != meet {
		t.Fatalf("a new meeting: %v, place %q", got, text(ev, ical.PropLocation))
	}
	if p := ev.Props.Get(ical.PropConference); p.Params.Get(ical.ParamValue) != "URI" || p.Params.Get("LABEL") != "Meet" {
		t.Errorf("CONFERENCE's parameters: %v", p.Params)
	}
	// A place typed: the link goes to the notes, said once.
	had := currentMeeting(ev)
	ics.SetText(ev, ical.PropLocation, "Salle 3")
	setMeeting(ev, had, had)
	if text(ev, ical.PropDescription) != "Meet: "+meet || len(videos(ev)) != 1 {
		t.Errorf("a new place: notes %q, %v", text(ev, ical.PropDescription), videos(ev))
	}
	// Another link: the old one goes from everywhere, a dial-in and the other notes stay.
	ics.SetText(ev, ical.PropDescription, "Code 1234\n\nMeet: "+meet)
	ev.Props.Add(ics.Prop(ical.PropConference, "tel:+33100000000", ical.ParamValue, "URI", "FEATURE", "PHONE"))
	setMeeting(ev, zoom, currentMeeting(ev))
	if text(ev, ical.PropDescription) != "Code 1234\n\nZoom: "+zoom || text(ev, ical.PropLocation) != "Salle 3" {
		t.Errorf("another link: notes %q, place %q", text(ev, ical.PropDescription), text(ev, ical.PropLocation))
	}
	if got := videos(ev); len(got) != 2 || got[0] != "PHONE tel:+33100000000" || got[1] != "VIDEO "+zoom {
		t.Errorf("another link: %v", got)
	}
	// Taken off.
	setMeeting(ev, "", currentMeeting(ev))
	if text(ev, ical.PropDescription) != "Code 1234" || currentMeeting(ev) != "" || len(videos(ev)) != 1 {
		t.Errorf("taken off: notes %q, %v", text(ev, ical.PropDescription), videos(ev))
	}
	// A place that was only its link goes with it.
	ev = ical.NewComponent(ical.CompEvent)
	ics.SetText(ev, ical.PropLocation, zoom)
	setMeeting(ev, "", currentMeeting(ev))
	if ev.Props.Get(ical.PropLocation) != nil {
		t.Errorf("the place that was only the link should go")
	}
}
