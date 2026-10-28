// Package google mirrors a Google account's main calendar through the Calendar API (spec §4): incremental listing
// with sync tokens, each event converted to iCalendar (spec §5.2), and my answers sent as a patch of my attendee
// (Google then replies to the organizer from the invited address).
package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/htmltext"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

const DefaultBase = "https://www.googleapis.com/calendar/v3"

// Calendar is one Google account's main calendar.
type Calendar struct {
	HTTP    *http.Client // signed in (oauth.Provider.Client)
	Base    string       // "" = DefaultBase
	ID      string       // "" = "primary", the account's main calendar
	Address string       // the account's address
}

type eventList struct {
	Items         []event `json:"items"`
	NextPageToken string  `json:"nextPageToken"`
	NextSyncToken string  `json:"nextSyncToken"`
	TimeZone      string  `json:"timeZone"` // the calendar's
}

type event struct {
	ID                string      `json:"id"`
	ICalUID           string      `json:"iCalUID"`
	Status            string      `json:"status"` // confirmed, tentative, cancelled
	EventType         string      `json:"eventType"`
	Summary           string      `json:"summary"`
	Description       string      `json:"description"`
	Location          string      `json:"location"`
	Created           string      `json:"created"`
	Updated           string      `json:"updated"`
	Sequence          int         `json:"sequence"`
	Transparency      string      `json:"transparency"`
	Visibility        string      `json:"visibility"`
	Start             *when       `json:"start"`
	End               *when       `json:"end"`
	Recurrence        []string    `json:"recurrence"`
	RecurringEventID  string      `json:"recurringEventId"`
	OriginalStartTime *when       `json:"originalStartTime"`
	Organizer         *person     `json:"organizer"`
	Attendees         []attendee  `json:"attendees"`
	HangoutLink       string      `json:"hangoutLink"`
	ConferenceData    *conference `json:"conferenceData"`
}

type when struct {
	Date     string `json:"date"`     // all day: 2026-10-24
	DateTime string `json:"dateTime"` // RFC 3339
	TimeZone string `json:"timeZone"` // IANA
}

type person struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Self        bool   `json:"self"`
}

type attendee struct {
	Email          string `json:"email"`
	DisplayName    string `json:"displayName"`
	ResponseStatus string `json:"responseStatus"` // needsAction, declined, tentative, accepted
	Self           bool   `json:"self"`
	Organizer      bool   `json:"organizer"`
	Optional       bool   `json:"optional"`
	Resource       bool   `json:"resource"`
}

type conference struct {
	EntryPoints []struct {
		EntryPointType string `json:"entryPointType"` // video, phone, sip, more
		URI            string `json:"uri"`
	} `json:"entryPoints"`
	ConferenceSolution *struct {
		Name string `json:"name"`
	} `json:"conferenceSolution"`
}

// List reads what changed since token, or everything when token is "" or Google refuses it (410: a full sync).
// A recurring event comes whole: when any of its occurrences changed, the series is read again.
func (c *Calendar) List(ctx context.Context, token string) (adapter.Page, error) {
	var items []event
	var zone, next string
	var err error
	if token != "" {
		items, zone, next, err = c.list(ctx, url.Values{"syncToken": {token}})
	}
	var gone *apiError
	if token == "" || errors.As(err, &gone) && gone.Status == http.StatusGone {
		items, zone, next, err = c.list(ctx, url.Values{})
		if err != nil {
			return adapter.Page{}, err
		}
		return adapter.Page{Changes: c.assembleAll(items, zone), Token: next, Full: true}, nil
	}
	if err != nil {
		return adapter.Page{}, err
	}

	var changes []adapter.Change
	var series []string
	seen := map[string]bool{}
	addSeries := func(uid string) {
		if !seen[uid] {
			seen[uid] = true
			series = append(series, uid)
		}
	}
	for _, e := range items {
		switch {
		case e.ICalUID != "" && (e.RecurringEventID != "" || len(e.Recurrence) > 0):
			addSeries(e.ICalUID)
		case e.RecurringEventID != "": // an occurrence cancelled, given by id only
			uid, err := c.uidOf(ctx, e.RecurringEventID)
			if err != nil {
				return adapter.Page{}, err
			}
			if uid != "" {
				addSeries(uid)
			} else {
				changes = append(changes, adapter.Change{SourceID: e.ID, Deleted: true})
			}
		case e.Status == "cancelled":
			changes = append(changes, adapter.Change{UID: e.ICalUID, SourceID: e.ID, Deleted: true})
		default:
			changes = append(changes, c.assemble(e.ICalUID, []event{e}, zone))
		}
	}
	for _, uid := range series {
		evs, _, _, err := c.list(ctx, url.Values{"iCalUID": {uid}})
		if err != nil {
			return adapter.Page{}, err
		}
		changes = append(changes, c.assemble(uid, evs, zone))
	}
	return adapter.Page{Changes: changes, Token: next}, nil
}

// list reads every page of an events listing: recurring events not expanded, cancelled ones included (a cancelled
// occurrence is how Google says it was removed from its series).
func (c *Calendar) list(ctx context.Context, q url.Values) (items []event, zone, syncToken string, err error) {
	if q.Get("syncToken") == "" {
		q.Del("syncToken")
	}
	q.Set("showDeleted", "true")
	q.Set("maxResults", "2500")
	for {
		var page eventList
		if err := c.do(ctx, http.MethodGet, "/events", q, nil, &page); err != nil {
			return nil, "", "", err
		}
		items = append(items, page.Items...)
		zone = page.TimeZone
		if page.NextPageToken == "" {
			return items, zone, page.NextSyncToken, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

// uidOf is the iCalUID of a series by its master's id ("" if it isn't on this calendar).
func (c *Calendar) uidOf(ctx context.Context, id string) (string, error) {
	var e event
	err := c.do(ctx, http.MethodGet, "/events/"+url.PathEscape(id), nil, nil, &e)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusGone) {
		return "", nil
	}
	return e.ICalUID, err
}

// assembleAll groups a full listing by iCalUID. What is cancelled or not mirrored is left out: the mirror deletes
// what a full listing doesn't name.
func (c *Calendar) assembleAll(items []event, zone string) []adapter.Change {
	byUID := map[string][]event{}
	var order []string
	for _, e := range items {
		if e.ICalUID == "" {
			continue
		}
		if _, ok := byUID[e.ICalUID]; !ok {
			order = append(order, e.ICalUID)
		}
		byUID[e.ICalUID] = append(byUID[e.ICalUID], e)
	}
	var out []adapter.Change
	for _, uid := range order {
		if ch := c.assemble(uid, byUID[uid], zone); !ch.Deleted {
			out = append(out, ch)
		}
	}
	return out
}

// assemble builds one change from every Google event of a UID: a single event, or a series' master with its
// changed occurrences as overrides and its cancelled ones as EXDATEs. I may be invited to occurrences only (no
// master on my calendar): they are written alone.
func (c *Calendar) assemble(uid string, evs []event, zone string) adapter.Change {
	var master *event
	var live, cancelled []event
	for i, e := range evs {
		switch {
		case e.RecurringEventID == "":
			master = &evs[i]
		case e.Status == "cancelled":
			cancelled = append(cancelled, e)
		default:
			live = append(live, e)
		}
	}
	gone := adapter.Change{UID: uid, Deleted: true}
	if master != nil {
		gone.SourceID = master.ID
		if master.Status == "cancelled" || !mirrored(*master) {
			return gone
		}
	} else if len(live) == 0 {
		return gone
	}

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, ics.ProdID)
	ch := adapter.Change{UID: uid, Calendar: cal}
	if master == nil {
		// Occurrences only: one is written as a plain event, several as overrides of a series I can't see.
		for _, e := range live {
			ev := c.vevent(e, zone)
			if len(live) > 1 {
				ev.Props.Set(recurrenceID(ical.PropRecurrenceID, *e.OriginalStartTime, e.Start, zone))
			}
			cal.Children = append(cal.Children, ev)
		}
		ch.SourceID = live[0].ID
		ch.Me = c.me(live[0])
		return ch
	}

	ev := c.vevent(*master, zone)
	for _, line := range master.Recurrence {
		if p, err := ics.Line(line); err == nil {
			ev.Props.Add(p)
		}
	}
	for _, e := range cancelled {
		if e.OriginalStartTime != nil {
			ev.Props.Add(recurrenceID(ical.PropExceptionDates, *e.OriginalStartTime, master.Start, zone))
		}
	}
	cal.Children = append(cal.Children, ev)
	for _, e := range live {
		if e.OriginalStartTime == nil {
			continue
		}
		ov := c.vevent(e, zone)
		ov.Props.Set(recurrenceID(ical.PropRecurrenceID, *e.OriginalStartTime, master.Start, zone))
		cal.Children = append(cal.Children, ov)
	}
	ch.SourceID = master.ID
	ch.Me = c.me(*master)
	return ch
}

// mirrored: working-location entries ("Home", "Office") are a setting shown in Google's grid, not events.
func mirrored(e event) bool { return e.EventType != "workingLocation" }

// me is the address invited in e when it isn't the account's (an alias): "" otherwise.
func (c *Calendar) me(e event) string {
	for _, a := range e.Attendees {
		if a.Self && ics.Address(a.Email) != ics.Address(c.Address) {
			return a.Email
		}
	}
	return ""
}

var partstats = map[string]string{"needsAction": "NEEDS-ACTION", "accepted": "ACCEPTED", "declined": "DECLINED",
	"tentative": "TENTATIVE"}

// vevent converts one Google event (spec §5.2's checklist). Recurrence lines and RECURRENCE-ID are the caller's.
func (c *Calendar) vevent(e event, calZone string) *ical.Component {
	ev := ical.NewComponent(ical.CompEvent)
	ev.Props.Set(ics.Prop(ical.PropUID, e.ICalUID))
	for name, v := range map[string]string{ical.PropDateTimeStamp: e.Updated, ical.PropLastModified: e.Updated,
		ical.PropCreated: e.Created} {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			ev.Props.Set(ics.DateTime(name, t, nil))
		}
	}
	ev.Props.Set(ics.Prop(ical.PropSequence, fmt.Sprint(e.Sequence)))
	if e.Summary != "" {
		ics.SetText(ev, ical.PropSummary, e.Summary)
	}
	if e.Start != nil {
		if p := timeProp(ical.PropDateTimeStart, *e.Start, calZone); p != nil {
			ev.Props.Set(p)
		}
	}
	if e.End != nil {
		if p := timeProp(ical.PropDateTimeEnd, *e.End, calZone); p != nil {
			ev.Props.Set(p)
		}
	}
	switch e.Status {
	case "tentative":
		ev.Props.Set(ics.Prop(ical.PropStatus, "TENTATIVE"))
	case "confirmed":
		ev.Props.Set(ics.Prop(ical.PropStatus, "CONFIRMED"))
	}
	if e.Transparency == "transparent" {
		ev.Props.Set(ics.Prop(ical.PropTransparency, "TRANSPARENT"))
	} else {
		ev.Props.Set(ics.Prop(ical.PropTransparency, "OPAQUE"))
	}
	switch e.Visibility {
	case "private":
		ev.Props.Set(ics.Prop(ical.PropClass, "PRIVATE"))
	case "confidential":
		ev.Props.Set(ics.Prop(ical.PropClass, "CONFIDENTIAL"))
	}

	// ORGANIZER and ATTENDEEs only on a meeting: an event of mine alone has no attendees.
	if len(e.Attendees) > 0 {
		if e.Organizer != nil && e.Organizer.Email != "" {
			ev.Props.Set(ics.Prop(ical.PropOrganizer, ics.Mailto(e.Organizer.Email), ical.ParamCommonName, e.Organizer.DisplayName))
		}
		for _, a := range e.Attendees {
			if a.Email == "" {
				continue
			}
			partstat := partstats[a.ResponseStatus]
			if partstat == "" {
				partstat = "NEEDS-ACTION"
			}
			role := "REQ-PARTICIPANT"
			switch {
			case a.Organizer:
				role = "CHAIR"
			case a.Optional:
				role = "OPT-PARTICIPANT"
			}
			p := ics.Prop(ical.PropAttendee, ics.Mailto(a.Email), ical.ParamCommonName, a.DisplayName,
				ical.ParamParticipationStatus, partstat, ical.ParamRole, role)
			if a.Resource {
				p.Params.Set(ical.ParamCalendarUserType, "RESOURCE")
			}
			if partstat == "NEEDS-ACTION" {
				p.Params.Set(ical.ParamRSVP, "TRUE")
			}
			ev.Props.Add(p)
		}
	}

	// The meeting link: kept apart from the description by the API, so copying the description alone would miss it.
	solution := "Google Meet"
	if e.ConferenceData != nil && e.ConferenceData.ConferenceSolution != nil && e.ConferenceData.ConferenceSolution.Name != "" {
		solution = e.ConferenceData.ConferenceSolution.Name
	}
	video := e.HangoutLink
	if e.ConferenceData != nil {
		for _, ep := range e.ConferenceData.EntryPoints {
			switch ep.EntryPointType {
			case "video":
				if video == "" || video == e.HangoutLink {
					video = ep.URI
				}
			case "phone":
				ev.Props.Add(ics.Prop(ical.PropConference, ep.URI, ical.ParamValue, string(ical.ValueURI), "FEATURE", "PHONE"))
			}
		}
	}
	adapter.Meeting(ev, video, solution, e.Location, htmltext.Plain(e.Description))
	return ev
}

// timeProp converts a Google start or end: a date, or a time written in its own zone (else the calendar's; UTC
// when neither is known, left for Localize).
func timeProp(name string, w when, calZone string) *ical.Prop {
	if w.Date != "" {
		p, err := ics.Date(name, w.Date)
		if err != nil {
			return nil
		}
		return p
	}
	t, err := time.Parse(time.RFC3339, w.DateTime)
	if err != nil {
		return nil
	}
	zone := w.TimeZone
	if zone == "" {
		zone = calZone
	}
	return ics.DateTime(name, t, ics.Zone(zone))
}

// recurrenceID writes an occurrence's original start (RECURRENCE-ID, or EXDATE) the way the master's DTSTART is
// written, so that clients pair them: a date for an all-day series, else the master's zone.
func recurrenceID(name string, original when, masterStart *when, calZone string) *ical.Prop {
	if masterStart != nil && masterStart.Date != "" {
		day := original.Date
		if day == "" {
			day = original.DateTime
		}
		if p, err := ics.Date(name, day); err == nil {
			return p
		}
	}
	zone := original.TimeZone
	if masterStart != nil && masterStart.TimeZone != "" {
		zone = masterStart.TimeZone
	}
	if zone == "" {
		zone = calZone
	}
	t, err := time.Parse(time.RFC3339, original.DateTime)
	if err != nil {
		return ics.Prop(name, "")
	}
	return ics.DateTime(name, t, ics.Zone(zone))
}

var answers = map[string]string{"ACCEPTED": "accepted", "TENTATIVE": "tentative", "DECLINED": "declined",
	"NEEDS-ACTION": "needsAction"}

// RSVP sets my answer on the event (a series' master answers the whole series) and has Google tell the organizer
// (sendUpdates=all). Only my attendee is sent (attendeesOmitted), the others stay as they are.
func (c *Calendar) RSVP(ctx context.Context, sourceID, partstat string) error {
	status, ok := answers[partstat]
	if !ok {
		return &adapter.Refused{Reason: "réponse inconnue (" + partstat + ")", Err: fmt.Errorf("google: unknown answer %q", partstat)}
	}
	path := "/events/" + url.PathEscape(sourceID)
	var e event
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &e); err != nil {
		return refused(err)
	}
	var self *attendee
	for i, a := range e.Attendees {
		if a.Self {
			self = &e.Attendees[i]
		}
	}
	if self == nil {
		return &adapter.Refused{Reason: "tu n'es pas invité à cet événement sur ce compte Google",
			Err: fmt.Errorf("google: no self attendee in %s", sourceID)}
	}
	body := map[string]any{"attendeesOmitted": true,
		"attendees": []map[string]string{{"email": self.Email, "responseStatus": status}}}
	return refused(c.do(ctx, http.MethodPatch, path, url.Values{"sendUpdates": {"all"}}, body, nil))
}

// refused tells the answers Google won't take from the ones worth trying again (rate limits, Google down).
func refused(err error) error {
	var ae *apiError
	if !errors.As(err, &ae) {
		return err
	}
	switch {
	case ae.Status == http.StatusNotFound || ae.Status == http.StatusGone:
		return &adapter.Refused{Reason: "l'événement n'existe plus chez Google", Err: err}
	case ae.Status == http.StatusTooManyRequests || ae.Status >= 500 || ae.rateLimited():
		return err
	case ae.Status == http.StatusBadRequest || ae.Status == http.StatusForbidden:
		return &adapter.Refused{Reason: "Google refuse la réponse : " + ae.Message, Err: err}
	}
	return err
}

type apiError struct {
	Status  int
	Message string
	Reasons []string
}

func (e *apiError) Error() string { return fmt.Sprintf("google: %d %s", e.Status, e.Message) }

func (e *apiError) rateLimited() bool {
	for _, r := range e.Reasons {
		if r == "rateLimitExceeded" || r == "userRateLimitExceeded" || r == "quotaExceeded" {
			return true
		}
	}
	return false
}

func (c *Calendar) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	base := c.Base
	if base == "" {
		base = DefaultBase
	}
	id := c.ID
	if id == "" {
		id = "primary"
	}
	u := base + "/calendars/" + url.PathEscape(id) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("google: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		return fmt.Errorf("google: %w", err)
	}
	if resp.StatusCode >= 300 {
		ae := &apiError{Status: resp.StatusCode, Message: resp.Status}
		var e struct {
			Error struct {
				Message string `json:"message"`
				Errors  []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			ae.Message = e.Error.Message
			for _, r := range e.Error.Errors {
				ae.Reasons = append(ae.Reasons, r.Reason)
			}
		}
		return ae
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("google: unreadable answer: %w", err)
	}
	return nil
}
