package connector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
)

const (
	maxRange   = 400 * 24 * time.Hour
	maxItems   = 500
	maxNotes   = 1500 // characters of an event's description in calendar_list
	localTimes = `local time: "YYYY-MM-DD" (a whole day) or "YYYY-MM-DDTHH:mm"`
)

// schema is T's input schema with these properties' values limited to a list.
func schema[T any](enums map[string][]any) *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	for k, vals := range enums {
		p := s.Properties[k]
		if p.Items != nil && (p.Type == "array" || len(p.Types) > 0 && p.Types[len(p.Types)-1] == "array") {
			p = p.Items
		}
		p.Enum = vals
		if len(p.Types) > 0 && p.Types[0] == "null" {
			p.Enum = append(p.Enum, nil)
		}
	}
	return s
}

func (c *Connector) calendarTools(s *mcp.Server) {
	add(c, s, &mcp.Tool{Name: "collections_list", Annotations: readOnly("List collections"),
		Description: "The user's collections (\"Général\", \"École\"…: contexts, each holding events and tasks), what each " +
			"holds, and which of the user's other accounts are copied into it; also the user's time zone and the time now. " +
			"Every tool that makes an event or a task takes one of these names: pick the one that fits what the user says."},
		c.listCollections)
	add(c, s, &mcp.Tool{Name: "calendar_list", Annotations: readOnly("List events and deadlines"),
		Description: "Events (each occurrence of a series on its own, with its recurrenceId), work sessions on tasks and open " +
			"task deadlines between two local times. Invitations show the user's answer; events copied from the user's " +
			"other accounts show the account (read-only but for rsvp and event_delete). note: the event's note, if it has one."},
		c.listEvents)
	add(c, s, &mcp.Tool{Name: "calendar_free_slots", Annotations: readOnly("Find free time"),
		Description: "Free slots of at least `minutes` between two local times, within working hours each day, around timed " +
			"events (declined invitations and unanswered ones don't count)."},
		c.freeSlots)
	add(c, s, &mcp.Tool{Name: "event_create", Annotations: changes("Create an event", false),
		Description: "Creates an event in a collection (synced to the user's phone and Obsidian). For time spent on a task, " +
			"use task_schedule. Returns its uid."},
		c.createEvent)
	add(c, s, &mcp.Tool{Name: "event_update", Annotations: changes("Change an event", false),
		Description: "Changes an event: title, times, place, notes, repetition, collection. For one occurrence of a series pass " +
			"its recurrenceId (from calendar_list); without it, the whole series changes. An empty string takes a field off. " +
			"Invitations and events copied from other accounts can't be changed (rsvp answers them)."},
		c.updateEvent)
	add(c, s, &mcp.Tool{Name: "event_delete", Annotations: changes("Delete an event", true),
		Description: "Deletes an event, or one occurrence of a series (pass its recurrenceId). Events copied from the user's " +
			"other accounts can be deleted too: they don't come back. Ask the user first."},
		c.deleteEvent)
	add(c, s, &mcp.Tool{Name: "rsvp", Annotations: changes("Answer an invitation", false),
		InputSchema: schema[rsvpIn](map[string][]any{"answer": {"accepted", "tentative", "declined"}}),
		Description: "Accepts, tentatively accepts or declines an invitation (the whole series). The server relays the answer " +
			"to the organizer."},
		c.rsvp)
}

// --- reading

type rangeIn struct {
	From       string `json:"from" jsonschema:"start, local time: YYYY-MM-DD or YYYY-MM-DDTHH:mm"`
	To         string `json:"to" jsonschema:"end (exclusive), local time"`
	Collection string `json:"collection,omitempty" jsonschema:"only this collection"`
	Declined   bool   `json:"includeDeclined,omitempty" jsonschema:"also the invitations the user declined"`
}

type occurrenceOut struct {
	Kind         string `json:"kind" jsonschema:"event, session (time to work on a task) or deadline (a task's due)"`
	Title        string `json:"title"`
	Start        string `json:"start"`
	End          string `json:"end,omitempty" jsonschema:"exclusive; a whole day's is the next day"`
	AllDay       bool   `json:"allDay,omitempty"`
	Collection   string `json:"collection"`
	UID          string `json:"uid"`
	RecurrenceID string `json:"recurrenceId,omitempty" jsonschema:"this occurrence of a series"`
	Location     string `json:"location,omitempty"`
	Notes        string `json:"notes,omitempty"`
	URL          string `json:"url,omitempty"`
	Account      string `json:"account,omitempty" jsonschema:"copied from this account of the user's: read-only but for rsvp and event_delete"`
	Organizer    string `json:"organizer,omitempty"`
	Invitation   string `json:"invitation,omitempty" jsonschema:"the user's answer: needs-action, accepted, tentative, declined"`
	TaskUID      string `json:"taskUid,omitempty" jsonschema:"a session's or a deadline's task"`
	Note         string `json:"note,omitempty" jsonschema:"its note in the vault"`
}

type listOut struct {
	TimeZone  string          `json:"timeZone"`
	Items     []occurrenceOut `json:"items"`
	Truncated bool            `json:"truncated,omitempty" jsonschema:"more than shown: ask for a shorter range"`
}

func (c *Connector) span(from, to string) (time.Time, time.Time, error) {
	f, err := c.parseWhen(from, "from")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	t, err := c.parseWhen(to, "to")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !t.t.After(f.t) {
		return time.Time{}, time.Time{}, refuse("to must come after from")
	}
	if t.t.Sub(f.t) > maxRange {
		return time.Time{}, time.Time{}, refuse("ask for at most a year at a time")
	}
	return f.t, t.t, nil
}

// mine is my address in an event: the server's, or the one the backend says was invited.
func (c *Connector) mine(ev *ical.Component) []string {
	out := []string{c.Me}
	if v := ics.Value(ev, attendeeOf); v != "" {
		out = append(out, v)
	}
	return out
}

// invitation is my ATTENDEE in an event someone else organizes (nil: I organize it, or I'm not invited).
func (c *Connector) invitation(ev *ical.Component) *ical.Prop {
	att := ics.Attendee(ev, c.mine(ev)...)
	if att == nil {
		return nil
	}
	org := ev.Props.Get(ical.PropOrganizer)
	if org == nil {
		return nil
	}
	for _, m := range c.mine(ev) {
		if ics.Address(org.Value) == ics.Address(m) {
			return nil
		}
	}
	return att
}

func (c *Connector) occurrence(ev *ical.Component, coll string) (occurrenceOut, bool) {
	if strings.EqualFold(text(ev, ical.PropStatus), "CANCELLED") {
		return occurrenceOut{}, false
	}
	start, ok := c.localOf(ev.Props.Get(ical.PropDateTimeStart))
	if !ok {
		return occurrenceOut{}, false
	}
	o := occurrenceOut{Kind: "event", Title: text(ev, ical.PropSummary), Start: start.String(), AllDay: start.day,
		Collection: coll, UID: text(ev, ical.PropUID), RecurrenceID: rid(ev.Props.Get(ical.PropRecurrenceID)),
		Location: text(ev, ical.PropLocation), URL: ics.Value(ev, ical.PropURL), Account: text(ev, sourceProp)}
	if end, ok := c.localOf(ev.Props.Get(ical.PropDateTimeEnd)); ok {
		o.End = end.String()
	} else if p := ev.Props.Get(ical.PropDuration); p != nil {
		if d, err := p.Duration(); err == nil {
			o.End = when{t: start.t.Add(d), day: start.day}.String()
		}
	} else if start.day {
		o.End = when{t: start.t.AddDate(0, 0, 1), day: true}.String()
	}
	if n := text(ev, ical.PropDescription); n != "" {
		if r := []rune(n); len(r) > maxNotes {
			n = string(r[:maxNotes]) + "…"
		}
		o.Notes = n
	}
	if org := ev.Props.Get(ical.PropOrganizer); org != nil {
		o.Organizer = org.Params.Get(ical.ParamCommonName)
		if o.Organizer == "" {
			o.Organizer = ics.Address(org.Value)
		}
	}
	if att := c.invitation(ev); att != nil {
		o.Invitation = strings.ToLower(ics.Partstat(att))
	}
	if rel := ev.Props.Get(ical.PropRelatedTo); rel != nil {
		o.Kind, o.TaskUID = "session", rel.Value
	}
	return o, true
}

func (c *Connector) listEvents(ctx context.Context, in rangeIn) (listOut, error) {
	from, to, err := c.span(in.From, in.To)
	if err != nil {
		return listOut{}, err
	}
	cols, err := c.collections(ctx)
	if err != nil {
		return listOut{}, err
	}
	if in.Collection != "" {
		one, err := c.collection(ctx, in.Collection, "")
		if err != nil {
			return listOut{}, err
		}
		cols = []davis.Calendar{one}
	}
	notes := c.index()
	out := listOut{TimeZone: c.Loc.String(), Items: []occurrenceOut{}}
	for _, col := range cols {
		if holds(col, "VEVENT") {
			items, err := c.Davis.Events(ctx, col.Href, from, to)
			if err != nil {
				return out, dav(err)
			}
			for _, it := range items {
				cal, err := parse(it.Data)
				if err != nil {
					continue
				}
				for _, ev := range components(cal, ical.CompEvent) {
					o, ok := c.occurrence(ev, name(col))
					if !ok || o.Invitation == "declined" && !in.Declined {
						continue
					}
					o.Note = notes.events[o.UID+"\n"+o.RecurrenceID]
					out.Items = append(out.Items, o)
				}
			}
		}
		if holds(col, "VTODO") {
			tasks, err := c.tasksIn(ctx, col)
			if err != nil {
				return out, err
			}
			for _, t := range tasks {
				if t.Done || t.Due == "" {
					continue
				}
				due, _ := c.parseWhen(t.Due, "due")
				end := due.t.Add(time.Minute)
				if due.day {
					end = due.t.AddDate(0, 0, 1)
				}
				if due.t.Before(to) && end.After(from) {
					out.Items = append(out.Items, occurrenceOut{Kind: "deadline", Title: t.Title, Start: t.Due, AllDay: due.day,
						Collection: t.Collection, UID: t.UID, TaskUID: t.UID, Note: notes.tasks[t.UID]})
				}
			}
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return sortKey(out.Items[i]) < sortKey(out.Items[j]) })
	if len(out.Items) > maxItems {
		out.Items, out.Truncated = out.Items[:maxItems], true
	}
	return out, nil
}

func sortKey(o occurrenceOut) string {
	if o.AllDay {
		return o.Start + "T00:00"
	}
	return o.Start
}

// --- free slots

type slotsIn struct {
	From     string `json:"from" jsonschema:"start, local time"`
	To       string `json:"to" jsonschema:"end (exclusive), local time"`
	Minutes  int    `json:"minutes" jsonschema:"the shortest slot wanted, in minutes"`
	DayStart string `json:"dayStart,omitempty" jsonschema:"working day start, HH:MM (default 09:00)"`
	DayEnd   string `json:"dayEnd,omitempty" jsonschema:"working day end, HH:MM (default 18:00)"`
}

type slot struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type slotsOut struct {
	Slots []slot `json:"slots"`
}

func clock(s, def string) (int, error) {
	if s == "" {
		s = def
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, refuse("%q isn't a time of day (HH:MM)", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (c *Connector) freeSlots(ctx context.Context, in slotsIn) (slotsOut, error) {
	if in.Minutes < 5 || in.Minutes > 24*60 {
		return slotsOut{}, refuse("minutes: between 5 and 1440")
	}
	from, to, err := c.span(in.From, in.To)
	if err != nil {
		return slotsOut{}, err
	}
	ds, err := clock(in.DayStart, "09:00")
	if err != nil {
		return slotsOut{}, err
	}
	de, err := clock(in.DayEnd, "18:00")
	if err != nil {
		return slotsOut{}, err
	}
	list, err := c.listEvents(ctx, rangeIn{From: in.From, To: in.To})
	if err != nil {
		return slotsOut{}, err
	}
	type span struct{ s, e time.Time }
	var busy []span
	for _, o := range list.Items {
		if o.AllDay || o.Kind == "deadline" || o.End == "" || o.Invitation == "needs-action" || o.Invitation == "declined" {
			continue
		}
		s, err1 := c.parseWhen(o.Start, "start")
		e, err2 := c.parseWhen(o.End, "end")
		if err1 == nil && err2 == nil {
			busy = append(busy, span{s.t, e.t})
		}
	}
	sort.Slice(busy, func(i, j int) bool { return busy[i].s.Before(busy[j].s) })
	out := slotsOut{Slots: []slot{}}
	need := time.Duration(in.Minutes) * time.Minute
	for day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, c.Loc); day.Before(to); day = day.AddDate(0, 0, 1) {
		start := day.Add(time.Duration(ds) * time.Minute)
		end := day.Add(time.Duration(de) * time.Minute)
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		cursor := start
		for _, b := range busy {
			if !b.e.After(cursor) || !b.s.Before(end) {
				continue
			}
			if b.s.Sub(cursor) >= need {
				out.Slots = append(out.Slots, slot{cursor.Format(timeLayout), b.s.Format(timeLayout)})
			}
			if b.e.After(cursor) {
				cursor = b.e
			}
		}
		if end.Sub(cursor) >= need {
			out.Slots = append(out.Slots, slot{cursor.Format(timeLayout), end.Format(timeLayout)})
		}
	}
	return out, nil
}

// --- finding an item

// item is a stored item, parsed.
type item struct {
	coll davis.Calendar
	davis.Item
	cal *ical.Calendar
}

// find is the item with this UID, in whichever collection holds it.
func (c *Connector) find(ctx context.Context, uid, comp string) (item, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return item{}, refuse("uid is required")
	}
	cols, err := c.collections(ctx)
	if err != nil {
		return item{}, err
	}
	for _, col := range cols {
		if !holds(col, comp) {
			continue
		}
		list, err := c.Davis.ByUID(ctx, col.Href, comp, uid)
		if err != nil {
			return item{}, dav(err)
		}
		for _, it := range list {
			cal, err := parse(it.Data)
			if err != nil {
				continue
			}
			if m := master(cal, comp); m != nil && text(m, ical.PropUID) == uid {
				return item{coll: col, Item: it, cal: cal}, nil
			}
		}
	}
	what := "event"
	if comp == ical.CompToDo {
		what = "task"
	}
	return item{}, refuse("no %s with uid %q (calendar_list and tasks_list give them)", what, uid)
}

func (c *Connector) save(ctx context.Context, it item) error {
	_, err := c.Davis.Put(ctx, it.Href, ics.Encode(it.cal), it.ETag)
	return dav(err)
}

// moveTo writes the item into another collection under the same UID, then deletes it from its own (as the
// extension moves one).
func (c *Connector) moveTo(ctx context.Context, it item, to davis.Calendar) error {
	href := hrefIn(to, text(master(it.cal, firstComp(it.cal)), ical.PropUID))
	if _, err := c.Davis.Put(ctx, href, ics.Encode(it.cal), ""); err != nil {
		return dav(err)
	}
	if err := c.Davis.Delete(ctx, it.Href, it.ETag); err != nil {
		if errors.Is(err, davis.ErrPrecondition) {
			c.Davis.Delete(ctx, href, "")
		}
		return dav(err)
	}
	return nil
}

func firstComp(cal *ical.Calendar) string {
	for _, ch := range cal.Children {
		if ch.Name == ical.CompEvent || ch.Name == ical.CompToDo {
			return ch.Name
		}
	}
	return ical.CompEvent
}

func hrefIn(col davis.Calendar, uid string) string {
	h := col.Href
	if !strings.HasSuffix(h, "/") {
		h += "/"
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, uid)
	return h + safe + ".ics"
}

// --- writing events

type eventIn struct {
	Title      string `json:"title"`
	Start      string `json:"start" jsonschema:"local time: YYYY-MM-DD (a whole day) or YYYY-MM-DDTHH:mm"`
	End        string `json:"end,omitempty" jsonschema:"exclusive (a whole-day event's is the day after its last day); default: an hour, or one day"`
	Collection string `json:"collection" jsonschema:"one of collections_list's names"`
	Location   string `json:"location,omitempty"`
	Notes      string `json:"notes,omitempty" jsonschema:"the event's description"`
	Repeat     string `json:"repeat,omitempty" jsonschema:"an RRULE, as FREQ=WEEKLY;BYDAY=TU or FREQ=DAILY;COUNT=5; UNTIL=YYYYMMDD is the last day"`
}

type createdOut struct {
	UID        string `json:"uid"`
	Collection string `json:"collection"`
	Start      string `json:"start"`
	End        string `json:"end"`
}

// times reads a start and an end; a missing end is an hour (or a day) later.
func (c *Connector) times(start, end string) (when, when, error) {
	s, err := c.parseWhen(start, "start")
	if err != nil {
		return when{}, when{}, err
	}
	if end == "" {
		if s.day {
			return s, when{t: s.t.AddDate(0, 0, 1), day: true}, nil
		}
		return s, when{t: s.t.Add(time.Hour)}, nil
	}
	e, err := c.parseWhen(end, "end")
	if err != nil {
		return when{}, when{}, err
	}
	if e.day != s.day {
		return when{}, when{}, refuse("start and end are both days (a whole-day event) or both times")
	}
	if !e.t.After(s.t) {
		if s.day {
			return when{}, when{}, refuse("end is exclusive: a whole-day event on %s ends %s", s, when{t: s.t.AddDate(0, 0, 1), day: true})
		}
		return when{}, when{}, refuse("end must come after start")
	}
	return s, e, nil
}

func (c *Connector) createEvent(ctx context.Context, in eventIn) (createdOut, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return createdOut{}, refuse("title is required")
	}
	col, err := c.collection(ctx, in.Collection, ical.CompEvent)
	if err != nil {
		return createdOut{}, err
	}
	s, e, err := c.times(in.Start, in.End)
	if err != nil {
		return createdOut{}, err
	}
	uid, err := c.putEvent(ctx, col, newEvent{title: title, start: s, end: e, location: in.Location, notes: in.Notes, rule: in.Repeat})
	if err != nil {
		return createdOut{}, err
	}
	return createdOut{UID: uid, Collection: name(col), Start: s.String(), End: e.String()}, nil
}

type newEvent struct {
	title, location, notes, rule, related string
	start, end                            when
}

func (c *Connector) putEvent(ctx context.Context, col davis.Calendar, e newEvent) (string, error) {
	cal := newCalendar()
	ev := ical.NewComponent(ical.CompEvent)
	uid := newUID()
	ev.Props.Set(ics.Prop(ical.PropUID, uid))
	ev.Props.Set(ics.Prop(ical.PropCreated, time.Now().UTC().Format(icalUTC)))
	ev.Props.SetText(ical.PropSummary, e.title)
	cal.Children = append(cal.Children, ev)
	c.setWhen(cal, ev, ical.PropDateTimeStart, e.start)
	c.setWhen(cal, ev, ical.PropDateTimeEnd, e.end)
	if l := strings.TrimSpace(e.location); l != "" {
		ev.Props.SetText(ical.PropLocation, l)
	}
	if n := strings.TrimSpace(e.notes); n != "" {
		ev.Props.SetText(ical.PropDescription, n)
	}
	if e.related != "" {
		ev.Props.Set(ics.Prop(ical.PropRelatedTo, e.related))
	}
	if e.rule != "" {
		r, err := c.ruleIn(e.rule, e.start)
		if err != nil {
			return "", err
		}
		ev.Props.Set(ics.Prop(ical.PropRecurrenceRule, r))
	}
	touch(ev)
	if _, err := c.Davis.Put(ctx, hrefIn(col, uid), ics.Encode(cal), ""); err != nil {
		return "", dav(err)
	}
	c.Log.Info("connector: event created", "uid", uid)
	return uid, nil
}

type eventUpdateIn struct {
	UID          string  `json:"uid"`
	RecurrenceID string  `json:"recurrenceId,omitempty" jsonschema:"one occurrence of a series (from calendar_list); leave out for the whole event"`
	Title        *string `json:"title,omitempty"`
	Start        *string `json:"start,omitempty" jsonschema:"local time; alone, the event keeps its length"`
	End          *string `json:"end,omitempty" jsonschema:"local time, exclusive"`
	Location     *string `json:"location,omitempty" jsonschema:"\"\" takes it off"`
	Notes        *string `json:"notes,omitempty" jsonschema:"the description; \"\" takes it off"`
	Repeat       *string `json:"repeat,omitempty" jsonschema:"the series' RRULE (UNTIL=YYYYMMDD: the last day); \"\" makes it a single event. Another pattern drops the series' changed occurrences"`
	Collection   string  `json:"collection,omitempty" jsonschema:"move it to this collection"`
}

type doneOut struct {
	Done string `json:"done"`
	UID  string `json:"uid,omitempty"`
}

// editable refuses what only the organizer or the backend writes.
func (c *Connector) editable(it item) error {
	m := master(it.cal, ical.CompEvent)
	if acc := text(m, sourceProp); acc != "" {
		return refuse("this event is copied from the account %s: only rsvp and event_delete apply to it", acc)
	}
	if c.invitation(m) != nil {
		return refuse("this is an invitation: answer it with rsvp (only its organizer changes it)")
	}
	return nil
}

func (c *Connector) updateEvent(ctx context.Context, in eventUpdateIn) (doneOut, error) {
	it, err := c.find(ctx, in.UID, ical.CompEvent)
	if err != nil {
		return doneOut{}, err
	}
	if err := c.editable(it); err != nil {
		return doneOut{}, err
	}
	series := master(it.cal, ical.CompEvent)
	recurring := series.Props.Get(ical.PropRecurrenceRule) != nil || series.Props.Get(ical.PropRecurrenceDates) != nil
	ev := series
	if in.RecurrenceID != "" {
		if !recurring {
			return doneOut{}, refuse("this event doesn't repeat: leave out recurrenceId")
		}
		if in.Repeat != nil || in.Collection != "" {
			return doneOut{}, refuse("repeat and collection are the whole series': leave out recurrenceId")
		}
		id, err := c.normRID(in.RecurrenceID)
		if err != nil {
			return doneOut{}, err
		}
		ev = c.exception(it.cal, series, id)
	}
	if in.Title != nil {
		if strings.TrimSpace(*in.Title) == "" {
			return doneOut{}, refuse("an event needs a title")
		}
		ev.Props.SetText(ical.PropSummary, strings.TrimSpace(*in.Title))
	}
	for prop, v := range map[string]*string{ical.PropLocation: in.Location, ical.PropDescription: in.Notes} {
		if v == nil {
			continue
		}
		if strings.TrimSpace(*v) == "" {
			ev.Props.Del(prop)
		} else {
			ev.Props.SetText(prop, strings.TrimSpace(*v))
		}
	}
	if in.Start != nil || in.End != nil {
		oldS, okS := c.localOf(ev.Props.Get(ical.PropDateTimeStart))
		oldE, okE := c.localOf(ev.Props.Get(ical.PropDateTimeEnd))
		start, end := "", ""
		if in.Start != nil {
			start = *in.Start
		} else if okS {
			start = oldS.String()
		}
		if in.End != nil {
			end = *in.End
		} else if okS && okE && in.Start != nil {
			ns, err := c.parseWhen(start, "start")
			if err != nil {
				return doneOut{}, err
			}
			if ns.day == oldS.day {
				end = when{t: ns.t.Add(oldE.t.Sub(oldS.t)), day: ns.day}.String()
				if ns.day {
					end = when{t: ns.t.AddDate(0, 0, int(oldE.t.Sub(oldS.t).Hours()/24+0.5)), day: true}.String()
				}
			}
		}
		s, e, err := c.times(start, end)
		if err != nil {
			return doneOut{}, err
		}
		c.setWhen(it.cal, ev, ical.PropDateTimeStart, s)
		c.setWhen(it.cal, ev, ical.PropDateTimeEnd, e)
		ev.Props.Del(ical.PropDuration)
		if ev == series {
			if r := series.Props.Get(ical.PropRecurrenceRule); r != nil {
				fitted, err := c.ruleIn(c.ruleOut(r.Value), s)
				if err == nil {
					r.Value = fitted
				}
			}
		}
	}
	if in.Repeat != nil {
		start, _ := c.localOf(series.Props.Get(ical.PropDateTimeStart))
		old := ""
		if r := series.Props.Get(ical.PropRecurrenceRule); r != nil {
			old = r.Value
		}
		rule, err := c.ruleIn(*in.Repeat, start)
		if err != nil {
			return doneOut{}, err
		}
		if rule == "" {
			series.Props.Del(ical.PropRecurrenceRule)
			series.Props.Del(ical.PropRecurrenceDates)
			series.Props.Del(ical.PropExceptionDates)
			dropExceptions(it.cal, series)
		} else {
			series.Props.Set(ics.Prop(ical.PropRecurrenceRule, rule))
			if pattern(old) != pattern(rule) {
				series.Props.Del(ical.PropExceptionDates)
				dropExceptions(it.cal, series)
			}
		}
	}
	bump(ev)
	if in.Collection != "" {
		to, err := c.collection(ctx, in.Collection, ical.CompEvent)
		if err != nil {
			return doneOut{}, err
		}
		if !davis.SamePath(to.Href, it.coll.Href) {
			if err := c.moveTo(ctx, it, to); err != nil {
				return doneOut{}, err
			}
			c.Log.Info("connector: event moved", "uid", in.UID)
			return doneOut{Done: "changed and moved to " + name(to), UID: in.UID}, nil
		}
	}
	if err := c.save(ctx, it); err != nil {
		return doneOut{}, err
	}
	c.Log.Info("connector: event changed", "uid", in.UID)
	return doneOut{Done: "changed", UID: in.UID}, nil
}

// pattern is a rule without its end: another pattern means other occurrences.
func pattern(rule string) string {
	var keep []string
	for _, part := range strings.Split(strings.ToUpper(rule), ";") {
		if !strings.HasPrefix(part, "UNTIL=") && !strings.HasPrefix(part, "COUNT=") && part != "" {
			keep = append(keep, part)
		}
	}
	sort.Strings(keep)
	return strings.Join(keep, ";")
}

func dropExceptions(cal *ical.Calendar, series *ical.Component) {
	kept := cal.Children[:0]
	for _, ch := range cal.Children {
		if ch.Name == series.Name && ch != series && ch.Props.Get(ical.PropRecurrenceID) != nil {
			continue
		}
		kept = append(kept, ch)
	}
	cal.Children = kept
}

type eventRef struct {
	UID          string `json:"uid"`
	RecurrenceID string `json:"recurrenceId,omitempty" jsonschema:"one occurrence of a series (from calendar_list)"`
}

func (c *Connector) deleteEvent(ctx context.Context, in eventRef) (doneOut, error) {
	it, err := c.find(ctx, in.UID, ical.CompEvent)
	if err != nil {
		return doneOut{}, err
	}
	if in.RecurrenceID == "" {
		if err := c.Davis.Delete(ctx, it.Href, it.ETag); err != nil {
			return doneOut{}, dav(err)
		}
		c.Log.Info("connector: event deleted", "uid", in.UID)
		return doneOut{Done: "deleted", UID: in.UID}, nil
	}
	series := master(it.cal, ical.CompEvent)
	if series.Props.Get(ical.PropRecurrenceRule) == nil && series.Props.Get(ical.PropRecurrenceDates) == nil {
		return doneOut{}, refuse("this event doesn't repeat: leave out recurrenceId")
	}
	id, err := c.normRID(in.RecurrenceID)
	if err != nil {
		return doneOut{}, err
	}
	kept := it.cal.Children[:0]
	for _, ch := range it.cal.Children {
		if ch.Name == ical.CompEvent && ch != series && rid(ch.Props.Get(ical.PropRecurrenceID)) == id {
			continue
		}
		kept = append(kept, ch)
	}
	it.cal.Children = kept
	ex := atLike(series.Props.Get(ical.PropDateTimeStart), id, c.Loc, ical.PropExceptionDates)
	series.Props.Add(ex)
	bump(series)
	if err := c.save(ctx, it); err != nil {
		return doneOut{}, err
	}
	c.Log.Info("connector: occurrence deleted", "uid", in.UID)
	return doneOut{Done: "occurrence deleted", UID: in.UID}, nil
}

type rsvpIn struct {
	UID    string `json:"uid"`
	Answer string `json:"answer"`
}

func (c *Connector) rsvp(ctx context.Context, in rsvpIn) (doneOut, error) {
	partstat := strings.ToUpper(strings.TrimSpace(in.Answer))
	if partstat != "ACCEPTED" && partstat != "TENTATIVE" && partstat != "DECLINED" {
		return doneOut{}, refuse("answer: accepted, tentative or declined")
	}
	it, err := c.find(ctx, in.UID, ical.CompEvent)
	if err != nil {
		return doneOut{}, err
	}
	found := false
	for _, ev := range components(it.cal, ical.CompEvent) {
		changed := false
		mine := false
		for i := range ev.Props[ical.PropAttendee] {
			att := &ev.Props[ical.PropAttendee][i]
			for _, m := range c.mine(ev) {
				if ics.Address(att.Value) != ics.Address(m) {
					continue
				}
				if ics.Partstat(att) != partstat {
					changed = true
				}
				att.Params.Set(ical.ParamParticipationStatus, partstat)
				delete(att.Params, "RSVP")
				mine = true
				break
			}
		}
		if !mine {
			continue
		}
		found = true
		if changed {
			ev.Props.Del(rsvpStateOf)
		}
		touch(ev)
	}
	if !found {
		return doneOut{}, refuse("the user isn't invited to this event: nothing to answer")
	}
	if err := c.save(ctx, it); err != nil {
		return doneOut{}, err
	}
	if c.Wake != nil {
		c.Wake()
	}
	c.Log.Info("connector: answered", "uid", in.UID)
	return doneOut{Done: fmt.Sprintf("answered %s (the organizer is told)", strings.ToLower(partstat)), UID: in.UID}, nil
}
