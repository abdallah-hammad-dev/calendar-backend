package connector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/vault"
)

var paris, _ = time.LoadLocation("Europe/Paris")

func TestRules(t *testing.T) {
	c := &Connector{Loc: paris}
	timed := when{t: time.Date(2026, 11, 10, 10, 0, 0, 0, paris)}
	day := when{t: time.Date(2026, 11, 10, 0, 0, 0, 0, paris), day: true}
	for _, tc := range []struct {
		in, want string
		at       when
	}{
		{"FREQ=WEEKLY;BYDAY=TU", "FREQ=WEEKLY;BYDAY=TU", timed},
		{"rrule:freq=weekly;until=20261231", "FREQ=WEEKLY;UNTIL=20261231T225959Z", timed},
		{"FREQ=WEEKLY;UNTIL=20261231", "FREQ=WEEKLY;UNTIL=20261231", day},
		{"FREQ=DAILY;UNTIL=20261231T225959Z", "FREQ=DAILY;UNTIL=20261231", day},
		{"", "", timed},
	} {
		got, err := c.ruleIn(tc.in, tc.at)
		if err != nil || got != tc.want {
			t.Errorf("ruleIn(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if got := c.ruleOut("FREQ=WEEKLY;UNTIL=20261231T225959Z"); got != "FREQ=WEEKLY;UNTIL=20261231" {
		t.Errorf("ruleOut: %q", got)
	}
	for _, bad := range []string{"every monday", "FREQ=WEEKLY;UNTIL=soon"} {
		if _, err := c.ruleIn(bad, timed); err == nil {
			t.Errorf("ruleIn(%q) accepted", bad)
		}
	}
	if pattern("FREQ=WEEKLY;BYDAY=TU;UNTIL=1") != pattern("BYDAY=TU;FREQ=WEEKLY") {
		t.Error("pattern")
	}
}

func TestNoteNameAndGuard(t *testing.T) {
	for in, want := range map[string]string{
		"Rendre le TP: réseau": "Rendre le TP réseau", "a/b\\c [x] #1": "a b c x 1", "...caché": "caché", "  ": "Untitled task",
	} {
		if got := noteName(in); got != want {
			t.Errorf("noteName(%q) = %q", in, got)
		}
	}
	task := vault.ReadMeta([]byte("---\ntype: task\ncaldav_uid: u\ndue: 2026-11-20\n---\nbody"))
	if err := guard(&task, vault.ReadMeta([]byte("---\ntype: task\ncaldav_uid: u\ndue: 2026-11-20\n---\nnew body"))); err != nil {
		t.Errorf("body edit refused: %v", err)
	}
	if err := guard(&task, vault.ReadMeta([]byte("---\ntype: task\ncaldav_uid: u\ndue: 2026-11-21\n---\nbody"))); err == nil {
		t.Error("due changed in a task note")
	}
	if err := guard(nil, vault.ReadMeta([]byte("---\ntype: task\n---\n"))); err == nil {
		t.Error("a new task note")
	}
	if err := guard(nil, vault.ReadMeta([]byte("---\ntags: [a]\n---\n"))); err != nil {
		t.Errorf("a plain note: %v", err)
	}
	re, _ := globRegexp("Projets/**/*.md")
	for p, want := range map[string]bool{"Projets/a.md": true, "Projets/x/y/a.md": true, "Autre/a.md": false, "Projets/a.png": false} {
		if re.MatchString(p) != want {
			t.Errorf("glob on %s", p)
		}
	}
}

// harness runs the connector's MCP server against the bench Davis (as orga, in throwaway collections) and a vault
// in a temporary folder, through the SDK's client: what Claude sends, schemas included.
type harness struct {
	t     *testing.T
	ctx   context.Context
	cs    *mcp.ClientSession
	c     *Connector
	dir   string
	cols  map[string]davis.Calendar // by short name
	names map[string]string         // short name → its collection's name
}

func newHarness(t *testing.T) *harness {
	url := os.Getenv("DAVIS_TEST_URL")
	if url == "" {
		t.Skip("DAVIS_TEST_URL not set")
	}
	ctx := context.Background()
	dc, err := davis.New(url, "orga", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := dc.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var b [3]byte
	rand.Read(b[:])
	suffix := hex.EncodeToString(b[:])
	h := &harness{t: t, ctx: ctx, cols: map[string]davis.Calendar{}, names: map[string]string{}}
	for _, short := range []string{"general", "ecole"} {
		display := map[string]string{"general": "IT Général ", "ecole": "IT École "}[short] + suffix
		href := p.Home + "it-connector-" + short + "-" + suffix + "/"
		if err := dc.MakeCalendar(ctx, href, display, "", "VEVENT", "VTODO"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { dc.Delete(context.Background(), href, "") })
		h.cols[short] = davis.Calendar{Href: href, DisplayName: display}
		h.names[short] = display
	}
	h.dir = t.TempDir()
	v, err := vault.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	v.Retouch = 0
	t.Cleanup(func() { v.Close() })
	h.c = &Connector{Davis: dc, Home: p.Home, Me: p.Email(), Loc: paris, Vault: v, TasksFolder: "Tasks",
		EventsFolder: "Calendar", EventDateFormat: "DD-MM-YYYY", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st, ct := mcp.NewInMemoryTransports()
	if _, err := h.c.Server().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	h.cs = cs
	return h
}

// call runs a tool and decodes its answer; refused: the error's text.
func (h *harness) call(name string, args map[string]any) (out map[string]any, refused string) {
	h.t.Helper()
	res, err := h.cs.CallTool(h.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		return nil, text
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		h.t.Fatalf("%s: %v: %s", name, err, text)
	}
	return out, ""
}

func (h *harness) ok(name string, args map[string]any) map[string]any {
	h.t.Helper()
	out, refused := h.call(name, args)
	if refused != "" {
		h.t.Fatalf("%s refused: %s", name, refused)
	}
	return out
}

func (h *harness) refused(name string, args map[string]any, want string) {
	h.t.Helper()
	_, refused := h.call(name, args)
	if refused == "" || !strings.Contains(refused, want) {
		h.t.Fatalf("%s: want a refusal with %q, got %q", name, want, refused)
	}
}

// raw is an item as stored, by UID.
func (h *harness) raw(uid, comp string) item {
	h.t.Helper()
	it, err := h.c.find(h.ctx, uid, comp)
	if err != nil {
		h.t.Fatal(err)
	}
	return it
}

func items(out map[string]any, key string) []map[string]any {
	var list []map[string]any
	for _, x := range out[key].([]any) {
		list = append(list, x.(map[string]any))
	}
	return list
}

func (h *harness) put(rel, text string) {
	full := filepath.Join(h.dir, "notes", rel)
	os.MkdirAll(filepath.Dir(full), 0o777)
	if err := os.WriteFile(full, []byte(text), 0o666); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) file(rel string) string {
	data, err := os.ReadFile(filepath.Join(h.dir, "notes", rel))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

func TestTasksAgainstDavis(t *testing.T) {
	h := newHarness(t)
	tools, err := h.cs.ListTools(h.ctx, nil)
	if err != nil || len(tools.Tools) != 21 {
		t.Fatalf("tools: %d %v", len(tools.Tools), err)
	}

	cols := h.ok("collections_list", nil)
	seen := 0
	for _, col := range items(cols, "collections") {
		if col["name"] == h.names["general"] || col["name"] == h.names["ecole"] {
			seen++
		}
	}
	if seen != 2 || cols["timeZone"] != "Europe/Paris" {
		t.Fatalf("collections: %v", cols)
	}

	h.refused("task_create", map[string]any{"title": "x", "collection": "Nope"}, "no collection")
	h.refused("task_create", map[string]any{"title": "x", "collection": h.names["ecole"], "priority": "urgent"}, "priority")
	h.put("Tasks/Rendre le TP réseau.md", "a note already there")
	tp := h.ok("task_create", map[string]any{"title": "Rendre le TP: réseau", "collection": h.names["ecole"], "due": "2026-11-20",
		"priority": "high", "tags": []string{"école", "#réseau"}, "notes": "Lire le sujet"})
	if tp["note"] != "Tasks/Rendre le TP réseau (2).md" || tp["link"] != "[[Rendre le TP réseau (2)]]" || tp["warning"] == nil {
		t.Fatalf("task_create: %v", tp)
	}
	uid := tp["uid"].(string)
	td := master(h.raw(uid, ical.CompToDo).cal, ical.CompToDo)
	if ics.Value(td, ical.PropDue) != "20261120" || ics.Value(td, ical.PropDateTimeStart) != "20261120" ||
		ics.Value(td, ical.PropPriority) != "1" || text(td, ical.PropDescription) != "Lire le sujet" ||
		td.Props.Get(ical.PropURL) != nil || !strings.EqualFold(ics.Value(td, ical.PropStatus), "NEEDS-ACTION") {
		t.Fatalf("stored: %s", ics.Encode(h.raw(uid, ical.CompToDo).cal))
	}
	if tags, _ := td.Props.Get(ical.PropCategories).TextList(); len(tags) != 2 || tags[1] != "réseau" {
		t.Fatalf("tags: %v", tags)
	}

	sub := h.ok("task_create", map[string]any{"title": "Lire le sujet", "parent": "Rendre le TP: réseau"})
	if sub["collection"] != h.names["ecole"] {
		t.Fatalf("sub-task: %v", sub)
	}
	list := items(h.ok("tasks_list", map[string]any{"collection": h.names["ecole"]}), "tasks")
	if len(list) != 2 || list[1]["parent"] != "Rendre le TP: réseau" || list[0]["due"] != "2026-11-20" {
		t.Fatalf("tasks_list: %v", list)
	}

	// Renamed, timed, repeating, then ticked; then moved to the other collection.
	h.ok("task_update", map[string]any{"task": uid, "title": "Rendre le TP", "due": "2026-11-20T18:00",
		"repeat": "FREQ=WEEKLY;UNTIL=20261231", "priority": ""})
	td = master(h.raw(uid, ical.CompToDo).cal, ical.CompToDo)
	if p := td.Props.Get(ical.PropDue); p.Value != "20261120T180000" || p.Params.Get(ical.PropTimezoneID) != "Europe/Paris" ||
		ics.Value(td, ical.PropRecurrenceRule) != "FREQ=WEEKLY;UNTIL=20261231T225959Z" || td.Props.Get(ical.PropPriority) != nil {
		t.Fatalf("updated: %s", ics.Encode(h.raw(uid, ical.CompToDo).cal))
	}
	if len(components(h.raw(uid, ical.CompToDo).cal, ical.CompTimezone)) != 1 {
		t.Fatal("no VTIMEZONE for the TZID")
	}
	if got := items(h.ok("tasks_list", map[string]any{"query": "Rendre le TP", "collection": h.names["ecole"]}), "tasks"); len(got) != 1 ||
		got[0]["repeat"] != "FREQ=WEEKLY;UNTIL=20261231" {
		t.Fatalf("repeat read back: %v", got)
	}
	h.ok("task_update", map[string]any{"task": uid, "collection": h.names["general"]})
	if it := h.raw(uid, ical.CompToDo); !davis.SamePath(it.coll.Href, h.cols["general"].Href) {
		t.Fatalf("not moved: %s", it.coll.Href)
	}

	// A work session in the task's collection, shown in the calendar.
	s := h.ok("task_schedule", map[string]any{"task": uid, "start": "2026-11-18T14:00", "minutes": 90})
	if s["collection"] != h.names["general"] || s["end"] != "2026-11-18T15:30" {
		t.Fatalf("session: %v", s)
	}
	cal := items(h.ok("calendar_list", map[string]any{"from": "2026-11-18", "to": "2026-11-21", "collection": h.names["general"]}), "items")
	if len(cal) != 2 || cal[0]["kind"] != "session" || cal[0]["taskUid"] != uid || cal[1]["kind"] != "deadline" {
		t.Fatalf("calendar: %v", cal)
	}

	h.refused("task_update", map[string]any{"task": "Nothing like it"}, "no task")
	h.ok("task_delete", map[string]any{"task": sub["uid"]})
	if _, err := h.c.find(h.ctx, sub["uid"].(string), ical.CompToDo); err == nil {
		t.Fatal("task not deleted")
	}
}

func TestEventsAgainstDavis(t *testing.T) {
	h := newHarness(t)
	g := h.names["general"]
	h.refused("event_create", map[string]any{"title": "x", "start": "2026-11-10T10:00", "end": "2026-11-10", "collection": g}, "both")
	ev := h.ok("event_create", map[string]any{"title": "Réunion d'équipe", "start": "2026-11-10T10:00", "collection": g,
		"repeat": "FREQ=WEEKLY;COUNT=3", "location": "Salle 2"})
	uid := ev["uid"].(string)
	if ev["end"] != "2026-11-10T11:00" {
		t.Fatalf("default end: %v", ev)
	}
	list := items(h.ok("calendar_list", map[string]any{"from": "2026-11-09", "to": "2026-12-01", "collection": g}), "items")
	if len(list) != 3 || list[1]["recurrenceId"] != "20261117T090000Z" || list[1]["start"] != "2026-11-17T10:00" || list[0]["location"] != "Salle 2" {
		t.Fatalf("series: %v", list)
	}

	// One occurrence moved and renamed: an exception at its own times; another deleted: an EXDATE.
	h.ok("event_update", map[string]any{"uid": uid, "recurrenceId": "20261117T090000Z", "title": "Réunion (déplacée)",
		"start": "2026-11-17T15:00"})
	h.ok("event_delete", map[string]any{"uid": uid, "recurrenceId": "20261124T090000Z"})
	list = items(h.ok("calendar_list", map[string]any{"from": "2026-11-09", "to": "2026-12-01", "collection": g}), "items")
	if len(list) != 2 || list[1]["title"] != "Réunion (déplacée)" || list[1]["start"] != "2026-11-17T15:00" || list[1]["end"] != "2026-11-17T16:00" {
		t.Fatalf("after changes: %v", list)
	}
	it := h.raw(uid, ical.CompEvent)
	series := master(it.cal, ical.CompEvent)
	if ex := series.Props.Get(ical.PropExceptionDates); ex == nil || ex.Value != "20261124T100000" || ex.Params.Get(ical.PropTimezoneID) != "Europe/Paris" {
		t.Fatalf("exdate: %s", ics.Encode(it.cal))
	}
	for _, e := range components(it.cal, ical.CompEvent) {
		if e != series {
			if r := e.Props.Get(ical.PropRecurrenceID); r.Value != "20261117T100000" || ics.Value(e, ical.PropLocation) != "Salle 2" {
				t.Fatalf("exception: %s", ics.Encode(it.cal))
			}
		}
	}

	// The occurrence's note, made as the extension makes it, found the second time.
	note := h.ok("event_note", map[string]any{"uid": uid, "recurrenceId": "20261117T090000Z"})
	if note["path"] != "Calendar/17-11-2026 Réunion (déplacée).md" || note["created"] != true {
		t.Fatalf("event note: %v", note)
	}
	if got := h.file("Calendar/17-11-2026 Réunion (déplacée).md"); got != "---\ntype: event\nstart: 2026-11-17T15:00\nend: 2026-11-17T16:00\n"+
		"caldav_uid: "+uid+"\nrecurrence_id: 20261117T090000Z\n---\n" {
		t.Fatalf("note:\n%s", got)
	}
	if again := h.ok("event_note", map[string]any{"uid": uid, "recurrenceId": "20261117T090000Z"}); again["created"] != false {
		t.Fatalf("second time: %v", again)
	}
	h.refused("event_note", map[string]any{"uid": uid}, "per occurrence")
	if got := items(h.ok("calendar_list", map[string]any{"from": "2026-11-17", "to": "2026-11-18"}), "items"); len(got) == 0 ||
		got[0]["note"] != "Calendar/17-11-2026 Réunion (déplacée).md" {
		t.Fatalf("the note in the list: %v", got)
	}

	// A video meeting: its link as the extension writes it, kept by a new place, changed, taken off.
	h.refused("event_create", map[string]any{"title": "x", "start": "2026-11-12T10:00", "collection": g, "meeting": "salle 3"}, "isn't a link")
	visio := h.ok("event_create", map[string]any{"title": "Point visio", "start": "2026-11-12T10:00", "collection": g,
		"meeting": "meet.google.com/abc-defg-hij"})["uid"].(string)
	shown := items(h.ok("calendar_list", map[string]any{"from": "2026-11-12", "to": "2026-11-13", "collection": g}), "items")
	if len(shown) != 1 || shown[0]["meeting"] != "https://meet.google.com/abc-defg-hij" || shown[0]["location"] != "https://meet.google.com/abc-defg-hij" {
		t.Fatalf("a meeting: %v", shown)
	}
	h.ok("event_update", map[string]any{"uid": visio, "location": "Salle 3"})
	ev1 := master(h.raw(visio, ical.CompEvent).cal, ical.CompEvent)
	if text(ev1, ical.PropDescription) != "Meet: https://meet.google.com/abc-defg-hij" || len(ev1.Props[ical.PropConference]) != 1 {
		t.Fatalf("a new place should keep the meeting: %s", ics.Encode(h.raw(visio, ical.CompEvent).cal))
	}
	h.ok("event_update", map[string]any{"uid": visio, "meeting": "https://us02web.zoom.us/j/1"})
	ev1 = master(h.raw(visio, ical.CompEvent).cal, ical.CompEvent)
	if currentMeeting(ev1) != "https://us02web.zoom.us/j/1" || text(ev1, ical.PropDescription) != "Zoom: https://us02web.zoom.us/j/1" {
		t.Fatalf("another link: %s", ics.Encode(h.raw(visio, ical.CompEvent).cal))
	}
	h.ok("event_update", map[string]any{"uid": visio, "meeting": ""})
	ev1 = master(h.raw(visio, ical.CompEvent).cal, ical.CompEvent)
	if currentMeeting(ev1) != "" || ev1.Props.Get(ical.PropDescription) != nil || text(ev1, ical.PropLocation) != "Salle 3" {
		t.Fatalf("taken off: %s", ics.Encode(h.raw(visio, ical.CompEvent).cal))
	}
	h.ok("event_delete", map[string]any{"uid": visio})

	// The whole series moved to the other collection, then made a single event.
	h.ok("event_update", map[string]any{"uid": uid, "collection": h.names["ecole"]})
	h.ok("event_update", map[string]any{"uid": uid, "repeat": ""})
	it = h.raw(uid, ical.CompEvent)
	if !davis.SamePath(it.coll.Href, h.cols["ecole"].Href) || len(components(it.cal, ical.CompEvent)) != 1 ||
		master(it.cal, ical.CompEvent).Props.Get(ical.PropExceptionDates) != nil {
		t.Fatalf("moved, single: %s %s", it.coll.Href, ics.Encode(it.cal))
	}

	// A mirrored invitation: only answered (and deleted).
	inv := newUID()
	data := strings.ReplaceAll(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:`+inv+`
DTSTAMP:20261101T080000Z
DTSTART;TZID=Europe/Paris:20261112T090000
DTEND;TZID=Europe/Paris:20261112T100000
SUMMARY:Soutenance
ORGANIZER;CN=Prof:mailto:prof@school.test
ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:`+ics.Address(h.c.Me)+`
X-CALTASKS-SOURCE-ACCOUNT:microsoft:me@school.test
X-CALTASKS-RSVP-STATE:confirmed
END:VEVENT
END:VCALENDAR
`, "\n", "\r\n")
	if _, err := h.c.Davis.Put(h.ctx, hrefIn(h.cols["ecole"], inv), data, ""); err != nil {
		t.Fatal(err)
	}
	h.refused("event_update", map[string]any{"uid": inv, "title": "x"}, "copied from the account")
	got := items(h.ok("calendar_list", map[string]any{"from": "2026-11-12", "to": "2026-11-13"}), "items")
	if len(got) != 1 || got[0]["invitation"] != "needs-action" || got[0]["account"] != "microsoft:me@school.test" || got[0]["organizer"] != "Prof" {
		t.Fatalf("invitation: %v", got)
	}
	free := items(h.ok("calendar_free_slots", map[string]any{"from": "2026-11-12", "to": "2026-11-13", "minutes": 30}), "slots")
	if len(free) != 1 || free[0]["start"] != "2026-11-12T09:00" {
		t.Fatalf("unanswered invitations don't count: %v", free)
	}
	h.ok("rsvp", map[string]any{"uid": inv, "answer": "accepted"})
	att := ics.Attendee(master(h.raw(inv, ical.CompEvent).cal, ical.CompEvent), h.c.Me)
	if ics.Partstat(att) != "ACCEPTED" || att.Params.Get("RSVP") != "" ||
		master(h.raw(inv, ical.CompEvent).cal, ical.CompEvent).Props.Get(rsvpStateOf) != nil {
		t.Fatalf("answered: %s", ics.Encode(h.raw(inv, ical.CompEvent).cal))
	}
	free = items(h.ok("calendar_free_slots", map[string]any{"from": "2026-11-12", "to": "2026-11-13", "minutes": 30}), "slots")
	if len(free) != 1 || free[0]["start"] != "2026-11-12T10:00" {
		t.Fatalf("free slots: %v", free)
	}
	h.refused("rsvp", map[string]any{"uid": inv, "answer": "maybe"}, "answer")
	h.ok("event_delete", map[string]any{"uid": inv})
	h.ok("event_delete", map[string]any{"uid": uid})
	h.refused("event_delete", map[string]any{"uid": uid}, "no event")
}

func TestVaultTools(t *testing.T) {
	h := newHarness(t)
	pain := h.ok("task_create", map[string]any{"title": "Acheter du pain", "collection": h.names["general"]})["uid"].(string)
	h.put("Tasks/Acheter du pain.md", "---\ntype: task\ncaldav_uid: "+pain+"\ndone: false\ndue: 2026-11-20\n---\nla boulangerie\n")
	h.put("Journal/2026-11-04.md", "- [[Acheter du pain]]\n- voir [[Idée]]\n")

	w := h.ok("vault_write", map[string]any{"path": "Projets/Idée.md", "content": "# Idée\n\nUn plan.\n"})
	if w["created"] != true {
		t.Fatalf("write: %v", w)
	}
	h.refused("vault_write", map[string]any{"path": "Projets/Idée.md", "content": "x"}, "exists")
	h.refused("vault_write", map[string]any{"path": "Tâche.md", "content": "---\ntype: task\n---\n"}, "task_create")
	h.refused("vault_write", map[string]any{"path": "../x.md", "content": "x"}, "inside the vault")
	h.refused("vault_write", map[string]any{"path": ".obsidian/x.json", "content": "x"}, "hidden")

	r := h.ok("vault_read", map[string]any{"path": "Projets/Idée.md"})
	h.ok("vault_edit", map[string]any{"path": "Projets/Idée.md", "oldText": "Un plan.", "newText": "Un plan, avec [[Acheter du pain]]."})
	h.refused("vault_write", map[string]any{"path": "Projets/Idée.md", "content": "x", "version": r["version"]}, "changed")
	h.refused("vault_edit", map[string]any{"path": "Projets/Idée.md", "oldText": "absent", "newText": "x"}, "isn't in")

	// A task note: the body is free, the rest is the task's.
	tr := h.ok("vault_read", map[string]any{"path": "Tasks/Acheter du pain.md"})
	if tr["kind"] != "task" || tr["uid"] != pain {
		t.Fatalf("task note: %v", tr)
	}
	h.ok("vault_edit", map[string]any{"path": "Tasks/Acheter du pain.md", "oldText": "la boulangerie", "newText": "la boulangerie du coin"})
	h.refused("vault_edit", map[string]any{"path": "Tasks/Acheter du pain.md", "oldText": "due: 2026-11-20", "newText": "due: 2026-11-21"}, "task_update")
	h.refused("vault_move", map[string]any{"from": "Tasks/Acheter du pain.md", "to": "Tasks/Pain.md"}, "task_update")
	h.refused("vault_delete", map[string]any{"path": "Tasks"}, "task_delete")
	h.ok("vault_move", map[string]any{"from": "Tasks/Acheter du pain.md", "to": "Courses/Acheter du pain.md"})

	// A rename rewrites the links.
	mv := h.ok("vault_move", map[string]any{"from": "Projets/Idée.md", "to": "Archive/Grande idée.md"})
	if re := mv["linksRewrittenIn"].([]any); len(re) != 1 || re[0] != "Journal/2026-11-04.md" {
		t.Fatalf("move: %v", mv)
	}
	if got := h.file("Journal/2026-11-04.md"); got != "- [[Acheter du pain]]\n- voir [[Grande idée]]\n" {
		t.Fatalf("links: %q", got)
	}

	found := items(h.ok("vault_search", map[string]any{"query": "boulangerie"}), "matches")
	if len(found) != 1 || found[0]["path"] != "Courses/Acheter du pain.md" || found[0]["line"] != float64(7) {
		t.Fatalf("search: %v", found)
	}
	ls := h.ok("vault_list", map[string]any{"pattern": "*.md"})
	if ls["total"] != float64(3) {
		t.Fatalf("list: %v", ls)
	}

	// Deleted, then restored; an edit undone.
	del := h.ok("vault_delete", map[string]any{"path": "Archive"})
	saved := items(h.ok("vault_restore", map[string]any{}), "saved")
	if len(saved) < 3 || saved[0]["id"] != del["restoreId"] || saved[0]["kind"] != "deleted" {
		t.Fatalf("saved: %v", saved)
	}
	h.ok("vault_restore", map[string]any{"id": del["restoreId"]})
	if !strings.Contains(h.file("Archive/Grande idée.md"), "Un plan, avec") {
		t.Fatal("not restored")
	}
	var undo string
	for _, s := range items(h.ok("vault_restore", map[string]any{"path": "Tasks/Acheter"}), "saved") {
		if s["why"] == "vault_edit" {
			undo = s["id"].(string)
		}
	}
	if undo == "" {
		t.Fatal("the task note's edit isn't in the history")
	}
	shown := h.ok("vault_restore", map[string]any{"id": undo, "show": true})
	if !strings.Contains(shown["content"].(map[string]any)["Tasks/Acheter du pain.md"].(string), "la boulangerie\n") {
		t.Fatalf("show: %v", shown)
	}
}

// What used to wait for an Obsidian to open (2026-11-05, the user: "pk je dois ouvrir obsidian pr la syncro?"): a
// task's text before its note exists, a task deleted with its note, a repeating task ticked. Dates are around today.
func TestTasksWithoutObsidianAgainstDavis(t *testing.T) {
	h := newHarness(t)
	now := time.Now().In(paris)
	day := func(d int) string { return now.AddDate(0, 0, d).Format(dayLayout) }
	gen := h.names["general"]

	// The text, while there is no note: read and changed in the reminder's notes.
	uid := h.ok("task_create", map[string]any{"title": "Traduire le casier", "collection": gen,
		"notes": "- [ ] Demander à Iris\n- [ ] Traduire"})["uid"].(string)
	h.ok("task_update", map[string]any{"task": "Traduire le casier", "notes": "- [ ] Traduire\n- [ ] Envoyer"})
	got := items(h.ok("tasks_list", map[string]any{"query": "casier"}), "tasks")
	if len(got) != 1 || got[0]["notes"] != "- [ ] Traduire\n- [ ] Envoyer" {
		t.Fatalf("notes: %v", got)
	}
	// An Obsidian made the note (its link is on the reminder), not here yet: refused, the edit would be lost.
	it := h.raw(uid, ical.CompToDo)
	td := master(it.cal, ical.CompToDo)
	td.Props.Set(ics.Prop(ical.PropURL, linkPrefix+"vault=v&uid="+uid))
	if _, err := h.c.Davis.Put(h.ctx, it.Href, ics.Encode(it.cal), it.ETag); err != nil {
		t.Fatal(err)
	}
	h.refused("task_update", map[string]any{"task": uid, "notes": "x"}, "on its way")
	h.put("Tasks/Traduire le casier.md", "---\ntype: task\ncaldav_uid: "+uid+"\ndone: false\n---\n- [ ] Traduire\n")
	h.refused("task_update", map[string]any{"task": uid, "notes": "x"}, "vault_edit")
	if got := items(h.ok("tasks_list", map[string]any{"query": "casier"}), "tasks"); got[0]["notes"] != nil ||
		got[0]["note"] != "Tasks/Traduire le casier.md" {
		t.Fatalf("with its note: %v", got)
	}

	// A whole-day repeating task ticked: back at once for its next date after today, the tick in its note.
	bin := h.ok("task_create", map[string]any{"title": "Sortir les poubelles", "collection": gen, "due": day(-1),
		"repeat": "FREQ=WEEKLY"})["uid"].(string)
	h.put("Tasks/Sortir les poubelles.md", "---\ntype: task\ndone: false\ndue: "+day(-1)+"\nrepeat: every week\n"+
		"completions:\n  - 2026-10-01T20:00\ncaldav_uid: "+bin+"\n---\nLe mardi soir.\n")
	tick := h.ok("task_update", map[string]any{"task": bin, "done": true})
	if tick["due"] != day(6) || !strings.HasPrefix(tick["done"].(string), "ticked") {
		t.Fatalf("ticked: %v", tick)
	}
	td = master(h.raw(bin, ical.CompToDo).cal, ical.CompToDo)
	if ics.Value(td, ical.PropDue) != strings.ReplaceAll(day(6), "-", "") || td.Props.Get(ical.PropCompleted) != nil ||
		!strings.EqualFold(ics.Value(td, ical.PropStatus), "NEEDS-ACTION") {
		t.Fatalf("moved on: %s", ics.Encode(h.raw(bin, ical.CompToDo).cal))
	}
	want := "---\ntype: task\ndone: false\ndue: " + day(6) + "\nrepeat: every week\ncompletions:\n  - 2026-10-01T20:00\n  - " +
		now.Format(dayLayout) + "T"
	if note := h.file("Tasks/Sortir les poubelles.md"); !strings.HasPrefix(note, want) || !strings.HasSuffix(note, "\n---\nLe mardi soir.\n") {
		t.Fatalf("its note:\n%s", note)
	}

	// Timed, its last day included; past it, ticked for good.
	next := day(0) + "T08:00"
	if now.Hour() >= 8 {
		next = day(1) + "T08:00"
	}
	tick = h.ok("task_update", map[string]any{"task": h.ok("task_create", map[string]any{"title": "Arroser", "collection": gen,
		"due": day(-1) + "T08:00", "repeat": "FREQ=DAILY;UNTIL=" + strings.ReplaceAll(day(1), "-", "")})["uid"], "done": true})
	if tick["due"] != next {
		t.Fatalf("timed: %v, want %s", tick, next)
	}
	tick = h.ok("task_update", map[string]any{"task": h.ok("task_create", map[string]any{"title": "Fini", "collection": gen,
		"due": day(-2), "repeat": "FREQ=DAILY;UNTIL=" + strings.ReplaceAll(day(-1), "-", "")})["uid"], "done": true})
	if tick["due"] != nil || !strings.Contains(tick["later"].(string), "last time") {
		t.Fatalf("ended: %v", tick)
	}
	if got := items(h.ok("tasks_list", map[string]any{"status": "done", "collection": gen}), "tasks"); len(got) != 1 || got[0]["title"] != "Fini" {
		t.Fatalf("done: %v", got)
	}

	// Deleted with its note; the note comes back unlinked.
	h.refused("vault_delete", map[string]any{"path": "Tasks/Traduire le casier.md"}, "deleteNote")
	del := h.ok("task_delete", map[string]any{"task": uid, "deleteNote": true})
	if del["note"] != "Tasks/Traduire le casier.md" || del["restoreId"] == nil || h.c.Vault.Exists("Tasks/Traduire le casier.md") {
		t.Fatalf("deleted: %v", del)
	}
	if _, err := h.c.find(h.ctx, uid, ical.CompToDo); err == nil {
		t.Fatal("its reminder is still there")
	}
	h.ok("vault_restore", map[string]any{"id": del["restoreId"]})
	if note := h.file("Tasks/Traduire le casier.md"); note != "---\ntype: task\ndone: true\n---\n- [ ] Traduire\n" {
		t.Fatalf("restored: %q", note)
	}

	// Its reminder deleted on the phone: the note still linked can go.
	it = h.raw(bin, ical.CompToDo)
	if err := h.c.Davis.Delete(h.ctx, it.Href, it.ETag); err != nil {
		t.Fatal(err)
	}
	h.ok("vault_delete", map[string]any{"path": "Tasks/Sortir les poubelles.md"})
	if d := h.ok("task_delete", map[string]any{"task": "Arroser", "deleteNote": true}); d["done"] != "deleted (it had no note yet: none will be made)" {
		t.Fatalf("no note: %v", d)
	}
}
