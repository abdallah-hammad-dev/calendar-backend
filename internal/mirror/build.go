// Package mirror copies external accounts into their Davis collections and watches those collections for my
// answers (spec §5, §6).
package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

// Property names shared with the extension (extension spec §4.6, its src/constants.ts).
const (
	PropSourceAccount  = "X-CALTASKS-SOURCE-ACCOUNT"  // which account; what makes an event a mirror for the extension
	PropSourceAttendee = "X-CALTASKS-SOURCE-ATTENDEE" // the address that was invited, my ATTENDEE being the Davis one
	PropRSVPState      = "X-CALTASKS-RSVP-STATE"      // pending / confirmed / failed:<reason>, written by the relay
)

// Target is what a mirror event is written for.
type Target struct {
	Account string         // X-CALTASKS-SOURCE-ACCOUNT value
	Me      string         // my address on Davis (its calendar-user-address-set), written as my ATTENDEE
	Vault   string         // Obsidian vault name in the URL ("" = no vault parameter: Obsidian's last vault)
	Loc     *time.Location // where times given in UTC are written
}

// Base builds the Davis copy of a source event from the source alone (spec §5.2, the part common to all accounts):
// my ATTENDEE rewritten to the Davis address (the invited one kept in X-CALTASKS-SOURCE-ATTENDEE), X-CALTASKS-SOURCE-ACCOUNT, the
// obsidian:// URL, times in a TZID with their VTIMEZONEs, no METHOD.
//
// src is modified: pass a fresh parse. invited is my address on the source account. answer, when not "", is my
// answer read on Davis that the source doesn't show yet: it replaces the source's PARTSTAT (spec §6.4).
//
// When the ORGANIZER is me (an event I organize on that account), my ATTENDEE is left alone: it isn't an
// invitation, and making me an attendee would show me Accept / Decline on my own event. The ORGANIZER is never
// rewritten to the Davis address, or Davis would start sending invitations itself (part 2, spec §9).
func Base(src *ical.Calendar, invited, answer string, t Target) *ical.Calendar {
	out := ical.NewCalendar()
	out.Props.SetText(ical.PropVersion, "2.0")
	out.Props.SetText(ical.PropProductID, ics.ProdID)
	for name, props := range src.Props {
		if strings.HasPrefix(name, "X-") {
			out.Props[name] = props // X-WR-TIMEZONE and the like; METHOD, PRODID, CALSCALE are dropped
		}
	}
	for _, c := range src.Children {
		if c.Name == ical.CompTimezone || c.Name == ical.CompEvent {
			out.Children = append(out.Children, c)
		}
	}

	for _, ev := range ics.Events(out) {
		ev.Props.Del(PropRSVPState)
		ev.Props.Del(PropSourceAttendee)
		ics.SetText(ev, PropSourceAccount, t.Account)

		organizer := ics.Address(ics.Value(ev, ical.PropOrganizer))
		if invited != "" && organizer != ics.Address(invited) {
			if att := ics.Attendee(ev, invited); att != nil {
				if ics.Address(invited) != ics.Address(t.Me) {
					att.Value = ics.Mailto(t.Me)
					ev.Props.Set(&ical.Prop{Name: PropSourceAttendee, Params: ical.Params{}, Value: ics.Mailto(invited)})
				}
				if answer != "" {
					att.Params.Set(ical.ParamParticipationStatus, answer)
				}
			}
		}

		// The URL is the link back to the note (extension spec §4.5). A source's own URL goes into the notes, so
		// that a meeting link there is still found.
		if p := ev.Props.Get(ical.PropURL); p != nil && p.Value != "" {
			desc, _ := ev.Props.Text(ical.PropDescription)
			if !strings.Contains(desc, p.Value) {
				if desc != "" {
					desc += "\n\n"
				}
				ev.Props.SetText(ical.PropDescription, desc+p.Value)
			}
		}
		ev.Props.Set(&ical.Prop{Name: ical.PropURL, Params: ical.Params{}, Value: noteLink(t.Vault, ics.Value(ev, ical.PropUID))})
	}
	ics.Localize(out, t.Loc)
	return out
}

func noteLink(vault, uid string) string {
	q := url.Values{}
	if vault != "" {
		q.Set("vault", vault)
	}
	q.Set("uid", uid)
	return "obsidian://calendar-tasks?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

// keptPrefixes are the client properties a mirror event keeps across rewrites: what the iPhone (or Thunderbird)
// adds for itself. Everything else is the source's.
var keptPrefixes = []string{"X-APPLE-", "X-MOZ-"}

// Merge carries into base what a client added to the Davis copy existing and the source knows nothing of: alarms
// (once a copy exists, its VALARMs are the client's), X-APPLE-* / X-MOZ-* properties, and the relay's
// X-CALTASKS-RSVP-STATE. Components are paired by RECURRENCE-ID. base is modified and returned.
func Merge(base, existing *ical.Calendar) *ical.Calendar {
	if existing == nil {
		return base
	}
	old := map[string]*ical.Component{}
	for _, ev := range ics.Events(existing) {
		old[ics.RecurrenceID(ev)] = ev
	}
	for _, ev := range ics.Events(base) {
		prev, ok := old[ics.RecurrenceID(ev)]
		if !ok {
			continue
		}
		for name, props := range prev.Props {
			if name == PropRSVPState || hasPrefix(name, keptPrefixes) {
				if _, own := ev.Props[name]; !own {
					ev.Props[name] = props
				}
			}
		}
		var children []*ical.Component
		for _, c := range ev.Children {
			if c.Name != ical.CompAlarm {
				children = append(children, c)
			}
		}
		for _, c := range prev.Children {
			if c.Name == ical.CompAlarm {
				children = append(children, c)
			}
		}
		ev.Children = children
	}
	return base
}

func hasPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// EnsureStamp gives every VEVENT without a DTSTAMP (required) the one of its Davis copy, else now. Base can't
// invent one: it would change at every pass, and every event would be rewritten.
func EnsureStamp(cal, existing *ical.Calendar, now time.Time) {
	old := map[string]string{}
	if existing != nil {
		for _, ev := range ics.Events(existing) {
			old[ics.RecurrenceID(ev)] = ics.Value(ev, ical.PropDateTimeStamp)
		}
	}
	for _, ev := range ics.Events(cal) {
		if ics.Value(ev, ical.PropDateTimeStamp) != "" {
			continue
		}
		v := old[ics.RecurrenceID(ev)]
		if v == "" {
			v = now.UTC().Format("20060102T150405Z")
		}
		ev.Props.Set(&ical.Prop{Name: ical.PropDateTimeStamp, Params: ical.Params{}, Value: v})
	}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// FileName is the item's name in its collection: the UID when it is a plain one, else a cleaned UID and a hash
// (UIDs like "abc@google.com" or with slashes make awkward paths).
func FileName(uid string) string {
	clean := unsafeName.ReplaceAllString(uid, "_")
	if clean == uid && len(uid) <= 100 {
		return uid + ".ics"
	}
	sum := sha256.Sum256([]byte(uid))
	if len(clean) > 60 {
		clean = clean[:60]
	}
	return clean + "-" + hex.EncodeToString(sum[:])[:12] + ".ics"
}

// Hash identifies a built event, to skip rewriting what didn't change.
func Hash(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:16])
}
