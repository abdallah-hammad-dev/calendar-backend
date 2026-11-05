package connector

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

// An event's video meeting, as the extension's event form writes it (2026-11-05, the user: "pouvoir ajouter un lien de
// visio"): CONFERENCE;VALUE=URI;FEATURE=VIDEO and its copies for the iPhone (adapter.Meeting); a link changed or taken
// off goes from wherever it was first.

var (
	linkInText = regexp.MustCompile(`https?://[^\s<>"'\\]+`)
	linkEnd    = regexp.MustCompile(`[).,;:!?\]]+$`)
	// Named as the extension names them on its Join button (its src/calendar/meeting.ts).
	providers = []struct {
		host *regexp.Regexp
		name string
	}{
		{regexp.MustCompile(`(^|\.)teams\.(microsoft|live)\.com$|(^|\.)teams\.cloud\.microsoft$`), "Teams"},
		{regexp.MustCompile(`(^|\.)zoom(gov)?\.(us|com)$`), "Zoom"},
		{regexp.MustCompile(`^meet\.google\.com$`), "Meet"},
		{regexp.MustCompile(`(^|\.)webex\.com$`), "Webex"},
		{regexp.MustCompile(`(^|\.)whereby\.com$`), "Whereby"},
		{regexp.MustCompile(`^meet\.jit\.si$`), "Jitsi"},
		{regexp.MustCompile(`(^|\.)gotomeeting\.com$|^meet\.goto\.com$`), "GoTo"},
		{regexp.MustCompile(`^join\.skype\.com$`), "Skype"},
		{regexp.MustCompile(`^facetime\.apple\.com$`), "FaceTime"},
	}
)

// meetingLink reads a meeting link as given: "" for none, the first web link in the text, or a host and path
// without "https://".
func meetingLink(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if m := linkInText.FindString(s); m != "" {
		return linkEnd.ReplaceAllString(m, ""), nil
	}
	if !strings.ContainsAny(s, " \t\n") && !strings.Contains(s, "://") {
		if u, err := url.Parse("https://" + s); err == nil && strings.Contains(u.Hostname(), ".") {
			return u.String(), nil
		}
	}
	return "", refuse("meeting: %q isn't a link (https://…)", s)
}

func meetingLabel(link string) string {
	if u, err := url.Parse(link); err == nil {
		host := strings.ToLower(u.Hostname())
		for _, p := range providers {
			if p.host.MatchString(host) {
				return p.name
			}
		}
	}
	return "Visio"
}

// phoneOnly is a CONFERENCE that is a dial-in number, not the video.
func phoneOnly(p ical.Prop) bool {
	f := strings.ToUpper(strings.Join(p.Params.Values("FEATURE"), ","))
	return strings.Contains(f, "PHONE") && !strings.Contains(f, "VIDEO")
}

// currentMeeting is the event's video link: its CONFERENCE, else a place that is only a link (the iPhone's
// "Location or Video Call").
func currentMeeting(ev *ical.Component) string {
	for _, p := range ev.Props[ical.PropConference] {
		if !phoneOnly(p) && strings.HasPrefix(p.Value, "http") {
			return p.Value
		}
	}
	if place := text(ev, ical.PropLocation); !strings.ContainsAny(place, " \t\n") && linkInText.MatchString(place) {
		return linkEnd.ReplaceAllString(place, "")
	}
	return ""
}

// withoutLink is a text without a link: a line left with only its name ("Zoom :") goes too.
func withoutLink(s, link string) string {
	var kept []string
	for line := range strings.SplitSeq(s, "\n") {
		if !strings.Contains(line, link) {
			kept = append(kept, line)
			continue
		}
		rest := strings.TrimSpace(strings.NewReplacer(link, "", "<>", "", "()", "", "[]", "").Replace(line))
		if rest != "" && !strings.HasSuffix(rest, ":") {
			kept = append(kept, rest)
		}
	}
	out := strings.Join(kept, "\n")
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out)
}

// setMeeting gives ev the video link ("" none), old being the one it had: gone from its CONFERENCE, the place and the
// notes, then the new one written as adapter.Meeting writes an account's (in the place when there's none, else a
// line in the notes, unless one of them holds it). The same link again puts its copy back where a new place or
// notes left none.
func setMeeting(ev *ical.Component, link, old string) {
	if old != "" && old != link {
		var keep []ical.Prop
		for _, p := range ev.Props[ical.PropConference] {
			if phoneOnly(p) {
				keep = append(keep, p)
			}
		}
		if len(keep) > 0 {
			ev.Props[ical.PropConference] = keep
		} else {
			ev.Props.Del(ical.PropConference)
		}
		for _, prop := range []string{ical.PropLocation, ical.PropDescription} {
			v, _ := ev.Props.Text(prop)
			if v == "" || !strings.Contains(v, old) {
				continue
			}
			if rest := withoutLink(v, old); rest != "" {
				ics.SetText(ev, prop, rest)
			} else {
				ev.Props.Del(prop)
			}
		}
	}
	if link == "" {
		return
	}
	// adapter.Meeting adds its CONFERENCE: the one said already goes first.
	var others []ical.Prop
	for _, p := range ev.Props[ical.PropConference] {
		if p.Value != link {
			others = append(others, p)
		}
	}
	ev.Props.Del(ical.PropConference)
	if len(others) > 0 {
		ev.Props[ical.PropConference] = others
	}
	adapter.Meeting(ev, link, meetingLabel(link), text(ev, ical.PropLocation), text(ev, ical.PropDescription))
}
