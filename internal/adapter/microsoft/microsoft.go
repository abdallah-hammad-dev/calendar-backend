// Package microsoft mirrors a Microsoft account's main calendar (personal or school) through Microsoft Graph
// (spec §4), and answers its invitations (Outlook then replies to the organizer).
//
// Graph v1.0 tracks changes only over a date range (calendarView/delta), and gives each occurrence of a series
// apart, with an id of its own. The mirror wants a series whole (RRULE, overrides, EXDATEs: spec §5.2), so any
// change to an occurrence has the series read again from its master, with its exceptions and cancelled
// occurrences. The token carries what that needs: the delta link, the range, and which Graph id belongs to which
// event (a deletion comes as an id alone).
package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/htmltext"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

const DefaultBase = "https://graph.microsoft.com/v1.0"

// The range tracked: a year back from the first listing (kept), a year and a half ahead, renewed (a full listing)
// when less than a year is left ahead.
const (
	back       = 365 * 24 * time.Hour
	ahead      = 548 * 24 * time.Hour
	renewAhead = 365 * 24 * time.Hour
)

// Calendar is one Microsoft account's main calendar.
type Calendar struct {
	HTTP    *http.Client // signed in (oauth.Provider.Client)
	Base    string       // "" = DefaultBase
	Address string       // the account's address
	Now     func() time.Time
}

// state is the token between passes.
type state struct {
	Delta  string            `json:"delta"`  // the last round's delta link ("" = list everything)
	From   time.Time         `json:"from"`   // the range tracked
	To     time.Time         `json:"to"`     //
	IDs    map[string]string `json:"ids"`    // Graph id (single event, master, occurrence) → the UID it is part of
	Series map[string]string `json:"series"` // UID → its series master's id, for recurring events
}

type event struct {
	ID                    string        `json:"id"`
	Type                  string        `json:"type"` // singleInstance, occurrence, exception, seriesMaster
	SeriesMasterID        string        `json:"seriesMasterId"`
	ICalUID               string        `json:"iCalUId"`
	Subject               string        `json:"subject"`
	Body                  *body         `json:"body"`
	Start                 *dateTimeZone `json:"start"`
	End                   *dateTimeZone `json:"end"`
	IsAllDay              bool          `json:"isAllDay"`
	OriginalStart         string        `json:"originalStart"` // an exception's start before it was moved, UTC
	OriginalStartTimeZone string        `json:"originalStartTimeZone"`
	Location              *struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	Organizer             *recipient     `json:"organizer"`
	Attendees             []attendee     `json:"attendees"`
	IsOrganizer           bool           `json:"isOrganizer"`
	IsCancelled           bool           `json:"isCancelled"`
	ResponseStatus        *status        `json:"responseStatus"`
	ShowAs                string         `json:"showAs"`
	Sensitivity           string         `json:"sensitivity"`
	OnlineMeeting         *onlineMeeting `json:"onlineMeeting"`
	OnlineMeetingURL      string         `json:"onlineMeetingUrl"`
	OnlineMeetingProvider string         `json:"onlineMeetingProvider"`
	Recurrence            *recurrence    `json:"recurrence"`
	LastModified          string         `json:"lastModifiedDateTime"`
	Created               string         `json:"createdDateTime"`
	ExceptionOccurrences  []event        `json:"exceptionOccurrences"`
	CancelledOccurrences  []string       `json:"cancelledOccurrences"` // "OID.<master id>.2026-11-16"
	Removed               *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

type body struct {
	ContentType string `json:"contentType"` // text, html
	Content     string `json:"content"`
}

type dateTimeZone struct {
	DateTime string `json:"dateTime"` // 2026-11-23T09:00:00.0000000, in TimeZone
	TimeZone string `json:"timeZone"`
}

type recipient struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type attendee struct {
	recipient
	Type   string  `json:"type"` // required, optional, resource
	Status *status `json:"status"`
}

type status struct {
	Response string `json:"response"` // none, organizer, tentativelyAccepted, accepted, declined, notResponded
}

type onlineMeeting struct {
	JoinURL string `json:"joinUrl"`
}

type recurrence struct {
	Pattern struct {
		Type           string   `json:"type"`
		Interval       int      `json:"interval"`
		Month          int      `json:"month"`
		DayOfMonth     int      `json:"dayOfMonth"`
		DaysOfWeek     []string `json:"daysOfWeek"`
		FirstDayOfWeek string   `json:"firstDayOfWeek"`
		Index          string   `json:"index"`
	} `json:"pattern"`
	Range struct {
		Type                string `json:"type"` // endDate, noEnd, numbered
		EndDate             string `json:"endDate"`
		RecurrenceTimeZone  string `json:"recurrenceTimeZone"`
		NumberOfOccurrences int    `json:"numberOfOccurrences"`
	} `json:"range"`
}

// fields read on a series master and its exceptions ($select also names the expanded relation, as Graph's own
// example does).
const fields = "subject,body,start,end,isAllDay,location,attendees,organizer,recurrence,iCalUId,isCancelled," +
	"isOrganizer,responseStatus,onlineMeeting,onlineMeetingUrl,onlineMeetingProvider,showAs,sensitivity,type," +
	"originalStart,originalStartTimeZone,lastModifiedDateTime,createdDateTime,seriesMasterId," +
	"exceptionOccurrences,cancelledOccurrences"

func (c *Calendar) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Calendar) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return DefaultBase
}

// List reads what changed since token, or everything (a new round) when there is no token, Graph refused it, or the
// range must move ahead.
func (c *Calendar) List(ctx context.Context, token string) (adapter.Page, error) {
	var st state
	if token != "" && json.Unmarshal([]byte(token), &st) != nil {
		st = state{}
	}
	now := c.now()
	full := st.Delta == "" || st.To.Sub(now) < renewAhead
	items, err := c.round(ctx, &st, full, now)
	if !full && syncLost(err) {
		full = true
		items, err = c.round(ctx, &st, full, now)
	}
	if err != nil {
		return adapter.Page{}, err
	}

	var changes []adapter.Change
	var dirty []string               // series master ids to read again
	members := map[string][]string{} // master id → its occurrences listed in this round
	reread := func(masterID string) {
		if _, ok := members[masterID]; !ok {
			members[masterID] = nil
			dirty = append(dirty, masterID)
		}
	}
	for _, e := range items {
		switch {
		case e.Removed != nil:
			uid := st.IDs[e.ID]
			delete(st.IDs, e.ID)
			if uid == "" {
				continue // never mirrored (outside the range)
			}
			if master, ok := st.Series[uid]; ok && master != e.ID {
				reread(master) // one occurrence gone: cancelled, or moved out of the range
				continue
			}
			changes = append(changes, c.forget(&st, uid, e.ID))
		case e.Type == "seriesMaster":
			reread(e.ID)
		case e.SeriesMasterID != "":
			reread(e.SeriesMasterID)
			members[e.SeriesMasterID] = append(members[e.SeriesMasterID], e.ID)
		case e.ICalUID != "":
			st.IDs[e.ID] = e.ICalUID
			changes = append(changes, c.single(e))
		}
	}
	for _, masterID := range dirty {
		ch, err := c.series(ctx, &st, masterID)
		if err != nil {
			return adapter.Page{}, err
		}
		if !ch.Deleted {
			for _, id := range members[masterID] {
				st.IDs[id] = ch.UID // so that its deletion, given by id alone, finds the series
			}
		}
		changes = append(changes, ch)
	}
	if full {
		var kept []adapter.Change
		for _, ch := range changes {
			if !ch.Deleted {
				kept = append(kept, ch)
			}
		}
		changes = kept
	}
	next, _ := json.Marshal(st)
	return adapter.Page{Changes: changes, Token: string(next), Full: full}, nil
}

// round reads a delta round to its end: from the last delta link, or a new round over a range moved ahead.
func (c *Calendar) round(ctx context.Context, st *state, full bool, now time.Time) ([]event, error) {
	link := st.Delta
	if full {
		if st.From.IsZero() {
			st.From = now.Add(-back).UTC().Truncate(24 * time.Hour)
		}
		st.To = now.Add(ahead).UTC().Truncate(24 * time.Hour)
		st.IDs, st.Series = map[string]string{}, map[string]string{}
		q := url.Values{"startDateTime": {st.From.Format(time.RFC3339)}, "endDateTime": {st.To.Format(time.RFC3339)}}
		link = c.base() + "/me/calendarView/delta?" + q.Encode()
	}
	if st.IDs == nil {
		st.IDs = map[string]string{}
	}
	if st.Series == nil {
		st.Series = map[string]string{}
	}
	var items []event
	for {
		var page struct {
			Value     []event `json:"value"`
			NextLink  string  `json:"@odata.nextLink"`
			DeltaLink string  `json:"@odata.deltaLink"`
		}
		if err := c.do(ctx, http.MethodGet, link, nil, &page, "odata.maxpagesize=200"); err != nil {
			return nil, err
		}
		items = append(items, page.Value...)
		switch {
		case page.NextLink != "":
			link = page.NextLink
		case page.DeltaLink != "":
			st.Delta = page.DeltaLink
			return items, nil
		default:
			return nil, errors.New("microsoft: a delta page with neither a next nor a delta link")
		}
	}
}

// syncLost: Graph no longer knows the round (its state was dropped, or must be restarted).
func syncLost(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	code := strings.ToLower(ae.Code)
	return ae.Status == http.StatusGone || strings.Contains(code, "syncstate") || strings.Contains(code, "resync")
}

// forget is the deletion of an event, and of what the token knows of it.
func (c *Calendar) forget(st *state, uid, sourceID string) adapter.Change {
	if master, ok := st.Series[uid]; ok {
		sourceID = master
		delete(st.Series, uid)
	}
	for id, u := range st.IDs {
		if u == uid {
			delete(st.IDs, id)
		}
	}
	return adapter.Change{UID: uid, SourceID: sourceID, Deleted: true}
}

// series reads a recurring event whole, from its master: the master with its RRULE, each exception as an override,
// each cancelled occurrence as an EXDATE.
func (c *Calendar) series(ctx context.Context, st *state, masterID string) (adapter.Change, error) {
	var m event
	q := url.Values{"$select": {fields}, "$expand": {"exceptionOccurrences"}}
	err := c.do(ctx, http.MethodGet, c.base()+"/me/events/"+url.PathEscape(masterID)+"?"+q.Encode(), nil, &m)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		uid := st.IDs[masterID]
		for u, id := range st.Series {
			if id == masterID {
				uid = u
			}
		}
		return c.forget(st, uid, masterID), nil
	}
	if err != nil {
		return adapter.Change{}, err
	}
	uid := m.ICalUID
	st.Series[uid] = masterID
	st.IDs[masterID] = uid

	loc := seriesZone(m)
	cal := newCalendar()
	master := c.vevent(m, loc)
	if m.Recurrence != nil {
		if rule := rrule(*m.Recurrence, m, loc); rule != "" {
			master.Props.Set(ics.Prop(ical.PropRecurrenceRule, rule))
		}
	}
	for _, oid := range m.CancelledOccurrences {
		if p := exdate(oid, m, loc); p != nil {
			master.Props.Add(p)
		}
	}
	cal.Children = append(cal.Children, master)
	for _, ex := range m.ExceptionOccurrences {
		st.IDs[ex.ID] = uid
		rid := originalStart(ical.PropRecurrenceID, ex, m, loc)
		if rid == nil {
			continue // its place in the series is unknown
		}
		ov := c.vevent(ex, zoneOf(ex.OriginalStartTimeZone, loc))
		ov.Props.Set(rid)
		cal.Children = append(cal.Children, ov)
	}
	return adapter.Change{UID: uid, SourceID: masterID, Calendar: cal}, nil
}

func (c *Calendar) single(e event) adapter.Change {
	cal := newCalendar()
	cal.Children = append(cal.Children, c.vevent(e, zoneOf(e.OriginalStartTimeZone, nil)))
	return adapter.Change{UID: e.ICalUID, SourceID: e.ID, Calendar: cal}
}

func newCalendar() *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, ics.ProdID)
	return cal
}

var partstats = map[string]string{"none": "NEEDS-ACTION", "notResponded": "NEEDS-ACTION", "accepted": "ACCEPTED",
	"tentativelyAccepted": "TENTATIVE", "declined": "DECLINED"}

// vevent converts one Graph event (spec §5.2's checklist); loc is where its times are written (nil: UTC, left to
// Localize). Recurrence and RECURRENCE-ID are the caller's.
func (c *Calendar) vevent(e event, loc *time.Location) *ical.Component {
	ev := ical.NewComponent(ical.CompEvent)
	ev.Props.Set(ics.Prop(ical.PropUID, e.ICalUID))
	for name, v := range map[string]string{ical.PropDateTimeStamp: e.LastModified, ical.PropLastModified: e.LastModified,
		ical.PropCreated: e.Created} {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			ev.Props.Set(ics.DateTime(name, t.Truncate(time.Second), nil))
		}
	}
	if e.Subject != "" {
		ics.SetText(ev, ical.PropSummary, e.Subject)
	}
	for _, t := range []struct {
		name string
		at   *dateTimeZone
	}{{ical.PropDateTimeStart, e.Start}, {ical.PropDateTimeEnd, e.End}} {
		if p := timeProp(t.name, t.at, e.IsAllDay, loc); p != nil {
			ev.Props.Set(p)
		}
	}
	if e.IsCancelled {
		ev.Props.Set(ics.Prop(ical.PropStatus, "CANCELLED"))
	} else {
		ev.Props.Set(ics.Prop(ical.PropStatus, "CONFIRMED"))
	}
	if e.ShowAs == "free" {
		ev.Props.Set(ics.Prop(ical.PropTransparency, "TRANSPARENT"))
	} else {
		ev.Props.Set(ics.Prop(ical.PropTransparency, "OPAQUE"))
	}
	switch e.Sensitivity {
	case "private":
		ev.Props.Set(ics.Prop(ical.PropClass, "PRIVATE"))
	case "confidential":
		ev.Props.Set(ics.Prop(ical.PropClass, "CONFIDENTIAL"))
	}

	// A meeting: my own events have no attendees and me as organizer. My answer is the event's responseStatus
	// (mine for sure, the attendee list may not have me: invited through a group).
	if len(e.Attendees) > 0 || !e.IsOrganizer {
		if e.Organizer != nil && e.Organizer.EmailAddress.Address != "" {
			ev.Props.Set(ics.Prop(ical.PropOrganizer, ics.Mailto(e.Organizer.EmailAddress.Address),
				ical.ParamCommonName, e.Organizer.EmailAddress.Name))
		}
		mine := ""
		if e.ResponseStatus != nil {
			mine = partstats[e.ResponseStatus.Response]
		}
		foundMe := false
		for _, a := range e.Attendees {
			addr := a.EmailAddress.Address
			if addr == "" {
				continue
			}
			partstat := "NEEDS-ACTION"
			if a.Status != nil && partstats[a.Status.Response] != "" {
				partstat = partstats[a.Status.Response]
			}
			if ics.Address(addr) == ics.Address(c.Address) {
				foundMe = true
				if mine != "" {
					partstat = mine
				}
			}
			ev.Props.Add(attendeeProp(addr, a.EmailAddress.Name, partstat, a.Type))
		}
		if !foundMe && !e.IsOrganizer && mine != "" && c.Address != "" {
			ev.Props.Add(attendeeProp(c.Address, "", mine, "required"))
		}
	}

	// The meeting link (a Teams invitation's text and place usually hold it already).
	desc := ""
	if e.Body != nil {
		desc = strings.TrimSpace(e.Body.Content)
		if e.Body.ContentType == "html" {
			desc = htmltext.Text(desc)
		}
	}
	join := e.OnlineMeetingURL
	if e.OnlineMeeting != nil && e.OnlineMeeting.JoinURL != "" {
		join = e.OnlineMeeting.JoinURL
	}
	label := map[string]string{"skypeForBusiness": "Skype Entreprise", "skypeForConsumer": "Skype"}[e.OnlineMeetingProvider]
	if label == "" {
		label = "Microsoft Teams"
	}
	place := ""
	if e.Location != nil {
		place = e.Location.DisplayName
	}
	adapter.Meeting(ev, join, label, place, desc)
	return ev
}

func attendeeProp(addr, name, partstat, kind string) *ical.Prop {
	role := "REQ-PARTICIPANT"
	if kind == "optional" {
		role = "OPT-PARTICIPANT"
	}
	p := ics.Prop(ical.PropAttendee, ics.Mailto(addr), ical.ParamCommonName, name, ical.ParamParticipationStatus, partstat,
		ical.ParamRole, role)
	if kind == "resource" {
		p.Params.Set(ical.ParamCalendarUserType, "RESOURCE")
	}
	if partstat == "NEEDS-ACTION" {
		p.Params.Set(ical.ParamRSVP, "TRUE")
	}
	return p
}

// zoneOf maps a zone name as Outlook writes it (Windows, sometimes IANA) to a zone; def when it can't, or when it
// is UTC (written as such).
func zoneOf(name string, def *time.Location) *time.Location {
	if loc := ics.OutlookZone(name); loc != nil {
		return loc
	}
	return def
}

// seriesZone is the zone a series repeats in: across DST changes, a weekly meeting stays at its wall-clock time
// there. Without one (a legacy custom zone), the series stays in UTC.
func seriesZone(m event) *time.Location {
	if m.Recurrence != nil && m.Recurrence.Range.RecurrenceTimeZone != "" {
		if loc := zoneOf(m.Recurrence.Range.RecurrenceTimeZone, nil); loc != nil {
			return loc
		}
	}
	return zoneOf(m.OriginalStartTimeZone, nil)
}

const graphLayout = "2006-01-02T15:04:05.9999999"

// instant reads a Graph date-time in its zone.
func instant(d *dateTimeZone) (time.Time, bool) {
	if d == nil || d.DateTime == "" {
		return time.Time{}, false
	}
	loc := zoneOf(d.TimeZone, time.UTC)
	t, err := time.ParseInLocation(graphLayout, d.DateTime, loc)
	return t, err == nil
}

// timeProp converts a start or end. An all-day event's midnight may come shifted to the zone asked (UTC): its day
// is read in loc, else the nearest midnight's.
func timeProp(name string, d *dateTimeZone, allDay bool, loc *time.Location) *ical.Prop {
	t, ok := instant(d)
	if !ok {
		return nil
	}
	if allDay {
		day := t
		if loc != nil {
			day = t.In(loc)
		}
		if day.Hour() >= 12 {
			day = day.AddDate(0, 0, 1)
		}
		p, _ := ics.Date(name, day.Format("2006-01-02"))
		return p
	}
	return ics.DateTime(name, t, loc)
}

// originalStart is an exception's place in its series, written like the master's DTSTART.
func originalStart(name string, ex, m event, loc *time.Location) *ical.Prop {
	t, err := time.Parse(time.RFC3339Nano, ex.OriginalStart)
	if err != nil {
		return nil
	}
	if m.IsAllDay {
		return timeProp(name, &dateTimeZone{DateTime: t.UTC().Format(graphLayout), TimeZone: "UTC"}, true, loc)
	}
	return ics.DateTime(name, t, loc)
}

// exdate is a cancelled occurrence ("OID.<master id>.2026-11-16": its day) at the series' time of day.
func exdate(oid string, m event, loc *time.Location) *ical.Prop {
	i := strings.LastIndex(oid, ".")
	if i < 0 {
		return nil
	}
	day, err := time.Parse("2006-01-02", oid[i+1:])
	if err != nil {
		return nil
	}
	if m.IsAllDay {
		p, _ := ics.Date(ical.PropExceptionDates, day.Format("2006-01-02"))
		return p
	}
	start, ok := instant(m.Start)
	if !ok {
		return nil
	}
	in := time.UTC
	if loc != nil {
		in = loc
	}
	start = start.In(in)
	at := time.Date(day.Year(), day.Month(), day.Day(), start.Hour(), start.Minute(), start.Second(), 0, in)
	return ics.DateTime(ical.PropExceptionDates, at, loc)
}

var weekdays = map[string]string{"sunday": "SU", "monday": "MO", "tuesday": "TU", "wednesday": "WE",
	"thursday": "TH", "friday": "FR", "saturday": "SA"}

var positions = map[string]string{"first": "1", "second": "2", "third": "3", "fourth": "4", "last": "-1"}

// rrule converts Outlook's recurrence pattern ("" when it can't be said).
func rrule(r recurrence, m event, loc *time.Location) string {
	p := r.Pattern
	freq := map[string]string{"daily": "DAILY", "weekly": "WEEKLY", "absoluteMonthly": "MONTHLY",
		"relativeMonthly": "MONTHLY", "absoluteYearly": "YEARLY", "relativeYearly": "YEARLY"}[p.Type]
	if freq == "" {
		return ""
	}
	parts := []string{"FREQ=" + freq}
	if p.Interval > 1 {
		parts = append(parts, fmt.Sprintf("INTERVAL=%d", p.Interval))
	}
	var days []string
	for _, d := range p.DaysOfWeek {
		if wd := weekdays[strings.ToLower(d)]; wd != "" {
			days = append(days, wd)
		}
	}
	switch p.Type {
	case "weekly":
		if len(days) > 0 {
			parts = append(parts, "BYDAY="+strings.Join(days, ","))
		}
		if wd := weekdays[strings.ToLower(p.FirstDayOfWeek)]; wd != "" {
			parts = append(parts, "WKST="+wd)
		}
	case "absoluteMonthly":
		parts = append(parts, fmt.Sprintf("BYMONTHDAY=%d", p.DayOfMonth))
	case "relativeMonthly", "relativeYearly":
		if p.Type == "relativeYearly" {
			parts = append(parts, fmt.Sprintf("BYMONTH=%d", p.Month))
		}
		parts = append(parts, "BYDAY="+strings.Join(days, ","))
		if pos := positions[p.Index]; pos != "" {
			parts = append(parts, "BYSETPOS="+pos)
		}
	case "absoluteYearly":
		parts = append(parts, fmt.Sprintf("BYMONTH=%d", p.Month), fmt.Sprintf("BYMONTHDAY=%d", p.DayOfMonth))
	}
	switch r.Range.Type {
	case "numbered":
		if r.Range.NumberOfOccurrences > 0 {
			parts = append(parts, fmt.Sprintf("COUNT=%d", r.Range.NumberOfOccurrences))
		}
	case "endDate":
		end, err := time.Parse("2006-01-02", r.Range.EndDate)
		if err != nil {
			break
		}
		if m.IsAllDay {
			parts = append(parts, "UNTIL="+end.Format("20060102"))
			break
		}
		// The last day, to its end where the series repeats; UNTIL is in UTC next to a TZID'd DTSTART.
		in := time.UTC
		if loc != nil {
			in = loc
		}
		last := time.Date(end.Year(), end.Month(), end.Day(), 23, 59, 59, 0, in)
		parts = append(parts, "UNTIL="+last.UTC().Format("20060102T150405Z"))
	}
	return strings.Join(parts, ";")
}

// Owner is the address of the account's mailbox: the one invitations are sent to.
func (c *Calendar) Owner(ctx context.Context) (string, error) {
	var cal struct {
		Owner struct {
			Address string `json:"address"`
		} `json:"owner"`
	}
	if err := c.do(ctx, http.MethodGet, c.base()+"/me/calendar?$select=owner", nil, &cal); err != nil {
		return "", err
	}
	if cal.Owner.Address == "" {
		return "", errors.New("microsoft: the calendar has no owner address")
	}
	return cal.Owner.Address, nil
}

// RSVP answers the invitation (a series' master answers the whole series); Outlook replies to the organizer
// when they asked for answers.
func (c *Calendar) RSVP(ctx context.Context, sourceID, partstat string) error {
	action := map[string]string{"ACCEPTED": "accept", "TENTATIVE": "tentativelyAccept", "DECLINED": "decline"}[partstat]
	if action == "" {
		return &adapter.Refused{Reason: "Outlook ne permet pas de revenir à « sans réponse »",
			Err: fmt.Errorf("microsoft: can't answer %q", partstat)}
	}
	path := c.base() + "/me/events/" + url.PathEscape(sourceID)
	var e struct {
		IsOrganizer       bool  `json:"isOrganizer"`
		ResponseRequested *bool `json:"responseRequested"`
	}
	if err := c.do(ctx, http.MethodGet, path+"?$select=isOrganizer,responseRequested", nil, &e); err != nil {
		return refused(err)
	}
	if e.IsOrganizer {
		return &adapter.Refused{Reason: "tu es l'organisateur de cet événement",
			Err: fmt.Errorf("microsoft: %s is mine", sourceID)}
	}
	send := e.ResponseRequested == nil || *e.ResponseRequested
	return refused(c.do(ctx, http.MethodPost, path+"/"+action, map[string]bool{"sendResponse": send}, nil))
}

// refused tells the answers Outlook won't take from the ones worth trying again (throttling, Graph down).
func refused(err error) error {
	var ae *apiError
	if !errors.As(err, &ae) {
		return err
	}
	switch {
	case ae.Status == http.StatusNotFound || ae.Status == http.StatusGone:
		return &adapter.Refused{Reason: "l'événement n'existe plus chez Microsoft", Err: err}
	case ae.Status == http.StatusBadRequest || ae.Status == http.StatusForbidden:
		return &adapter.Refused{Reason: "Microsoft refuse la réponse : " + ae.Message, Err: err}
	}
	return err
}

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("microsoft: %d %s %s", e.Status, e.Code, e.Message)
}

// do calls Graph. link is a full URL: Graph's next and delta links are followed as given, but only on Graph itself
// (the client adds the account's token to whatever it calls).
func (c *Calendar) do(ctx context.Context, method, link string, in, out any, prefer ...string) error {
	if !strings.HasPrefix(link, c.base()+"/") {
		return fmt.Errorf("microsoft: refusing to follow a link outside Graph")
	}
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, link, rd)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Add("Prefer", `outlook.timezone="UTC"`)
	req.Header.Add("Prefer", `outlook.body-content-type="text"`)
	for _, p := range prefer {
		req.Header.Add("Prefer", p)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("microsoft: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		return fmt.Errorf("microsoft: %w", err)
	}
	if resp.StatusCode >= 300 {
		ae := &apiError{Status: resp.StatusCode, Message: resp.Status}
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Code != "" {
			ae.Code, ae.Message = e.Error.Code, e.Error.Message
		}
		return ae
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("microsoft: unreadable answer: %w", err)
	}
	return nil
}
