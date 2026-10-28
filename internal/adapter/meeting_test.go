package adapter

import (
	"testing"

	"github.com/emersion/go-ical"
)

func TestMeeting(t *testing.T) {
	const link = "https://meet.google.com/abc-defg-hij"
	for _, c := range []struct {
		name, link, place, notes string
		wantPlace, wantNotes     string
	}{
		{"online only: in the place (the iPhone's video call field)", link, "", "Ordre du jour", link, "Ordre du jour"},
		{"a room: kept, the link in the notes", link, "Salle B", "Ordre du jour", "Salle B", "Ordre du jour\n\nGoogle Meet: " + link},
		{"a room, no notes", link, "Salle B", "", "Salle B", "Google Meet: " + link},
		{"already in the notes (Teams)", link, "Réunion Microsoft Teams", "Rejoindre : " + link, "Réunion Microsoft Teams", "Rejoindre : " + link},
		{"already the place", link, link, "", link, ""},
		{"no meeting", "", "Salle B", "x", "Salle B", "x"},
	} {
		ev := ical.NewComponent(ical.CompEvent)
		Meeting(ev, c.link, "Google Meet", c.place, c.notes)
		place, _ := ev.Props.Text(ical.PropLocation)
		notes, _ := ev.Props.Text(ical.PropDescription)
		conf := ev.Props.Get(ical.PropConference)
		if place != c.wantPlace || notes != c.wantNotes || (c.link != "") != (conf != nil) {
			t.Errorf("%s: place %q, notes %q, CONFERENCE %v", c.name, place, notes, conf)
		}
		if conf != nil && (conf.Value != link || conf.Params.Get("FEATURE") != "VIDEO" || conf.Params.Get("LABEL") != "Google Meet") {
			t.Errorf("%s: CONFERENCE %+v", c.name, conf)
		}
	}
}
