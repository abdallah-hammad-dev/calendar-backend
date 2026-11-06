package connector

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/teambition/rrule-go"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/vault"
)

// Tasks are VTODOs, read and written as the extension does (its vtodo.ts): the deadline is DUE (with DTSTART =
// DUE, as iOS writes it), priority 1 / 5 / 9, tags are CATEGORIES, a sub-task has RELATED-TO;RELTYPE=PARENT.

func (c *Connector) taskTools(s *mcp.Server) {
	priorities := map[string][]any{"priority": {"high", "medium", "low"}}
	add(c, s, &mcp.Tool{Name: "tasks_list", Annotations: readOnly("List tasks"),
		InputSchema: schema[tasksIn](map[string][]any{"status": {"open", "done", "all"}}),
		Description: "Tasks (reminders) with their deadline, repetition, priority, tags, parent, collection, and their note " +
			"in the vault (made by the extension) with its link; a task with no note yet shows its text (notes). Filters are " +
			"optional."},
		c.listTasks)
	add(c, s, &mcp.Tool{Name: "task_create", Annotations: changes("Create a task", false),
		InputSchema: schema[taskIn](priorities),
		Description: "Creates a task: a reminder on the server, and a note the extension makes in Obsidian at its next " +
			"sync (named after the title; `notes` become its body). Returns the note's link: write it where the task belongs " +
			"(a line \"- [[Title]]\" shows its checkbox). `due` is the deadline; to block time for it, task_schedule. A step " +
			"inside a task is a \"- [ ]\" line in its note; a step with its own deadline is a sub-task (`parent`)."},
		c.createTask)
	add(c, s, &mcp.Tool{Name: "task_update", Annotations: changes("Change a task", true),
		InputSchema: schema[taskUpdateIn](map[string][]any{"priority": {"high", "medium", "low", ""}}),
		Description: "Changes a task: tick it (done), rename it (the extension renames its note), its deadline, repetition, " +
			"priority, tags, parent, collection, and its text (`notes`) while it has no note yet (tasks_list shows them; once " +
			"the note is made, change its body with vault_edit). An empty string takes a field off. A repeating task ticked " +
			"comes back at once at its next date. `task`: its uid, title or note."},
		c.updateTask)
	add(c, s, &mcp.Tool{Name: "task_delete", Annotations: changes("Delete a task", true),
		Description: "Deletes a task's reminder, and its note with deleteNote (to the server's trash: vault_restore brings it " +
			"back). Without it the note stays, marked done and unlinked. To close a task, tick it instead (task_update done). " +
			"Ask the user first."},
		c.deleteTask)
	add(c, s, &mcp.Tool{Name: "task_schedule", Annotations: changes("Block time for a task", false),
		Description: "Makes a work session for a task: an event linked to it (in the calendar, a click opens the task). Use " +
			"calendar_free_slots first to pick a time. It doesn't change the deadline."},
		c.scheduleTask)
}

type task struct {
	UID        string   `json:"uid"`
	Title      string   `json:"title"`
	Done       bool     `json:"done"`
	Due        string   `json:"due,omitempty" jsonschema:"the deadline, local"`
	Repeat     string   `json:"repeat,omitempty" jsonschema:"its RRULE (UNTIL: the last day)"`
	Priority   string   `json:"priority,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	ParentUID  string   `json:"parentUid,omitempty"`
	Parent     string   `json:"parent,omitempty" jsonschema:"the parent task's title"`
	Collection string   `json:"collection"`
	Created    string   `json:"created,omitempty"`
	Completed  string   `json:"completed,omitempty"`
	Note       string   `json:"note,omitempty" jsonschema:"its note in the vault"`
	Link       string   `json:"link,omitempty" jsonschema:"how a note links to it"`
	Notes      string   `json:"notes,omitempty" jsonschema:"while it has no note yet: the reminder's notes, which become the note's body"`

	item item
}

// linkPrefix starts the extension's link back to a task's note (its taskLink: the reminder's URL, and the last line
// of its notes).
const linkPrefix = "obsidian://calendar-tasks?"

// notesOf are a reminder's notes without the extension's link line (its vtodo.ts readTask).
func notesOf(td *ical.Component) string {
	var kept []string
	for _, l := range strings.Split(text(td, ical.PropDescription), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), linkPrefix) {
			kept = append(kept, l)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// madeInObsidian: an Obsidian has made the task's note, and gave the reminder its link (ensureLink), even if the
// note hasn't reached this server yet.
func madeInObsidian(td *ical.Component) bool {
	return strings.HasPrefix(ics.Value(td, ical.PropURL), linkPrefix) || strings.Contains(text(td, ical.PropDescription), linkPrefix)
}

var priorityOf = map[int]string{1: "high", 2: "high", 3: "high", 4: "high", 5: "medium", 6: "low", 7: "low", 8: "low", 9: "low"}

func (c *Connector) readTask(data string) (task, bool) {
	cal, err := parse(data)
	if err != nil {
		return task{}, false
	}
	return c.taskOf(cal)
}

func (c *Connector) taskOf(cal *ical.Calendar) (task, bool) {
	td := master(cal, ical.CompToDo)
	if td == nil || text(td, ical.PropUID) == "" {
		return task{}, false
	}
	t := task{UID: text(td, ical.PropUID), Title: text(td, ical.PropSummary), Repeat: c.ruleOf(td)}
	pct, _ := strconv.Atoi(ics.Value(td, ical.PropPercentComplete))
	t.Done = strings.EqualFold(ics.Value(td, ical.PropStatus), "COMPLETED") || pct >= 100 || td.Props.Get(ical.PropCompleted) != nil
	for _, f := range []struct {
		prop string
		to   *string
	}{{ical.PropDue, &t.Due}, {ical.PropCreated, &t.Created}, {ical.PropCompleted, &t.Completed}} {
		if w, ok := c.localOf(td.Props.Get(f.prop)); ok {
			*f.to = w.String()
			if f.prop != ical.PropDue && w.day {
				*f.to = w.t.Format(timeLayout)
			}
		}
	}
	if n, err := strconv.Atoi(ics.Value(td, ical.PropPriority)); err == nil {
		t.Priority = priorityOf[n]
	}
	for _, p := range td.Props[ical.PropCategories] {
		vals, _ := p.TextList()
		for _, v := range vals {
			if v = strings.TrimSpace(v); v != "" {
				t.Tags = append(t.Tags, v)
			}
		}
	}
	for _, p := range td.Props[ical.PropRelatedTo] {
		if rt := p.Params.Get(ical.ParamRelationshipType); rt == "" || strings.EqualFold(rt, "PARENT") {
			t.ParentUID = p.Value
		}
	}
	return t, true
}

// tasksIn are a collection's tasks.
func (c *Connector) tasksIn(ctx context.Context, col davis.Calendar) ([]task, error) {
	items, err := c.Davis.Todos(ctx, col.Href)
	if err != nil {
		return nil, dav(err)
	}
	var out []task
	for _, it := range items {
		cal, err := parse(it.Data)
		if err != nil {
			continue
		}
		if t, ok := c.taskOf(cal); ok {
			t.Collection = name(col)
			t.item = item{coll: col, Item: it, cal: cal}
			out = append(out, t)
		}
	}
	return out, nil
}

func (c *Connector) allTasks(ctx context.Context) ([]task, error) {
	cols, err := c.collections(ctx)
	if err != nil {
		return nil, err
	}
	var all []task
	for _, col := range cols {
		if holds(col, "VTODO") {
			list, err := c.tasksIn(ctx, col)
			if err != nil {
				return nil, err
			}
			all = append(all, list...)
		}
	}
	return all, nil
}

// --- the task's note

var forbidden = regexp.MustCompile(`[\\/:*?"<>|#^\[\]\r\n\t]+`)

// noteName is a task title as the extension names its note (notes.ts sanitizeTitle).
func noteName(title string) string {
	s := strings.Join(strings.Fields(forbidden.ReplaceAllString(title, " ")), " ")
	s = strings.TrimLeft(s, ".")
	if s == "" {
		s = "Untitled task"
	}
	if r := []rune(s); len(r) > 120 {
		s = strings.TrimSpace(string(r[:120]))
	}
	return s
}

func basename(p string) string {
	p = p[strings.LastIndex(p, "/")+1:]
	return strings.TrimSuffix(p, ".md")
}

// withNote fills in a task's note and link.
func (t *task) withNote(notes noteIndex, folder string) {
	if p := notes.tasks[t.UID]; p != "" {
		t.Note, t.Link = p, "[["+basename(p)+"]]"
		return
	}
	t.Link = "[[" + noteName(t.Title) + "]]"
}

// --- listing

type tasksIn struct {
	Status     string `json:"status,omitempty" jsonschema:"default: open"`
	Tag        string `json:"tag,omitempty" jsonschema:"only tasks with this tag"`
	DueBefore  string `json:"dueBefore,omitempty" jsonschema:"only tasks due before this local time"`
	Collection string `json:"collection,omitempty"`
	Query      string `json:"query,omitempty" jsonschema:"only tasks whose title holds this"`
}

type tasksOut struct {
	Tasks     []task `json:"tasks"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (c *Connector) listTasks(ctx context.Context, in tasksIn) (tasksOut, error) {
	all, err := c.allTasks(ctx)
	if err != nil {
		return tasksOut{}, err
	}
	status := strings.ToLower(in.Status)
	if status == "" {
		status = "open"
	}
	var before when
	if in.DueBefore != "" {
		if before, err = c.parseWhen(in.DueBefore, "dueBefore"); err != nil {
			return tasksOut{}, err
		}
	}
	notes := c.index()
	titles := map[string]string{}
	for _, t := range all {
		titles[t.UID] = t.Title
	}
	tag := strings.TrimLeft(strings.TrimSpace(in.Tag), "#")
	out := tasksOut{Tasks: []task{}}
	for _, t := range all {
		switch {
		case status == "open" && t.Done, status == "done" && !t.Done:
			continue
		case tag != "" && !hasTag(t.Tags, tag):
			continue
		case in.Collection != "" && !strings.EqualFold(t.Collection, strings.TrimSpace(in.Collection)):
			continue
		case in.Query != "" && !strings.Contains(strings.ToLower(t.Title), strings.ToLower(in.Query)):
			continue
		}
		if in.DueBefore != "" {
			due, err := c.parseWhen(t.Due, "due")
			if t.Due == "" || err != nil || !due.t.Before(before.t) {
				continue
			}
		}
		t.Parent = titles[t.ParentUID]
		t.withNote(notes, c.TasksFolder)
		if t.Note == "" {
			t.Notes = notesOf(master(t.item.cal, ical.CompToDo))
		}
		out.Tasks = append(out.Tasks, t)
	}
	sort.SliceStable(out.Tasks, func(i, j int) bool {
		a, b := out.Tasks[i].Due, out.Tasks[j].Due
		if a == "" {
			a = "9999"
		}
		if b == "" {
			b = "9999"
		}
		return a < b
	})
	if len(out.Tasks) > maxItems {
		out.Tasks, out.Truncated = out.Tasks[:maxItems], true
	}
	return out, nil
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.ReplaceAll(t, " ", "-"), strings.ReplaceAll(want, " ", "-")) {
			return true
		}
	}
	return false
}

// findTask is the task Claude names: by uid, title, or note (its path or name).
func (c *Connector) findTask(ctx context.Context, ref string) (task, error) {
	ref = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(ref), "[["), "]]"))
	if ref == "" {
		return task{}, refuse("say which task (its uid, title or note)")
	}
	all, err := c.allTasks(ctx)
	if err != nil {
		return task{}, err
	}
	notes := c.index()
	var found []task
	for _, t := range all {
		note := notes.tasks[t.UID]
		if t.UID == ref || note != "" && (note == ref || note == ref+".md" || strings.EqualFold(basename(note), basename(ref))) {
			return t, nil
		}
		if strings.EqualFold(t.Title, ref) || strings.EqualFold(noteName(t.Title), noteName(ref)) {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 0:
		return task{}, refuse("no task %q (tasks_list gives them)", ref)
	case 1:
		return found[0], nil
	}
	var open []task
	for _, t := range found {
		if !t.Done {
			open = append(open, t)
		}
	}
	if len(open) == 1 {
		return open[0], nil
	}
	var ids []string
	for _, t := range found {
		ids = append(ids, fmt.Sprintf("%s (%s, %s)", t.UID, t.Collection, map[bool]string{true: "done", false: "open"}[t.Done]))
	}
	return task{}, refuse("several tasks are called %q: pass the uid of one of %s", ref, strings.Join(ids, ", "))
}

// --- writing

var priorityValue = map[string]string{"high": "1", "medium": "5", "low": "9"}

func (c *Connector) setDue(cal *ical.Calendar, td *ical.Component, due *when) {
	if due == nil {
		td.Props.Del(ical.PropDateTimeStart)
		td.Props.Del(ical.PropDue)
		return
	}
	c.setWhen(cal, td, ical.PropDateTimeStart, *due)
	c.setWhen(cal, td, ical.PropDue, *due)
}

func setDone(td *ical.Component, done bool) {
	if done {
		td.Props.Set(ics.Prop(ical.PropStatus, "COMPLETED"))
		td.Props.Set(ics.Prop(ical.PropPercentComplete, "100"))
		if td.Props.Get(ical.PropCompleted) == nil {
			td.Props.Set(ics.Prop(ical.PropCompleted, time.Now().UTC().Format(icalUTC)))
		}
		return
	}
	td.Props.Set(ics.Prop(ical.PropStatus, "NEEDS-ACTION"))
	td.Props.Del(ical.PropPercentComplete)
	td.Props.Del(ical.PropCompleted)
}

func setTags(td *ical.Component, tags []string) {
	td.Props.Del(ical.PropCategories)
	var clean []string
	for _, t := range tags {
		if t = strings.TrimLeft(strings.TrimSpace(t), "#"); t != "" {
			clean = append(clean, t)
		}
	}
	if len(clean) > 0 {
		p := ical.NewProp(ical.PropCategories)
		p.SetTextList(clean)
		td.Props.Set(p)
	}
}

func setParent(td *ical.Component, uid string) {
	kept := td.Props[ical.PropRelatedTo][:0]
	for _, p := range td.Props[ical.PropRelatedTo] {
		if rt := p.Params.Get(ical.ParamRelationshipType); rt != "" && !strings.EqualFold(rt, "PARENT") {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		delete(td.Props, ical.PropRelatedTo)
	} else {
		td.Props[ical.PropRelatedTo] = kept
	}
	if uid != "" {
		td.Props.Add(ics.Prop(ical.PropRelatedTo, uid, ical.ParamRelationshipType, "PARENT"))
	}
}

type taskIn struct {
	Title      string   `json:"title"`
	Collection string   `json:"collection,omitempty" jsonschema:"one of collections_list's names; default: the parent's"`
	Due        string   `json:"due,omitempty" jsonschema:"the deadline, local time: YYYY-MM-DD or YYYY-MM-DDTHH:mm"`
	Repeat     string   `json:"repeat,omitempty" jsonschema:"an RRULE (needs a due): FREQ=WEEKLY;BYDAY=MO, FREQ=DAILY;INTERVAL=2… UNTIL=YYYYMMDD is the last day"`
	Priority   string   `json:"priority,omitempty" jsonschema:"leave out for none"`
	Tags       []string `json:"tags,omitempty" jsonschema:"its tags, without #"`
	Parent     string   `json:"parent,omitempty" jsonschema:"the parent task (uid, title or note) for a sub-task"`
	Notes      string   `json:"notes,omitempty" jsonschema:"markdown: the body of the note the extension makes for it"`
}

type taskCreatedOut struct {
	UID        string `json:"uid"`
	Collection string `json:"collection"`
	Note       string `json:"note" jsonschema:"the note the extension will make for it"`
	Link       string `json:"link" jsonschema:"write this where the task belongs"`
	Warning    string `json:"warning,omitempty"`
}

func (c *Connector) createTask(ctx context.Context, in taskIn) (taskCreatedOut, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return taskCreatedOut{}, refuse("title is required")
	}
	var parent task
	if in.Parent != "" {
		p, err := c.findTask(ctx, in.Parent)
		if err != nil {
			return taskCreatedOut{}, err
		}
		parent = p
	}
	want := in.Collection
	if want == "" && parent.UID != "" {
		want = parent.Collection
	}
	col, err := c.collection(ctx, want, ical.CompToDo)
	if err != nil {
		return taskCreatedOut{}, err
	}
	cal := newCalendar()
	td := ical.NewComponent(ical.CompToDo)
	uid := newUID()
	td.Props.Set(ics.Prop(ical.PropUID, uid))
	td.Props.Set(ics.Prop(ical.PropCreated, time.Now().UTC().Format(icalUTC)))
	td.Props.SetText(ical.PropSummary, title)
	cal.Children = append(cal.Children, td)
	setDone(td, false)
	var due *when
	if in.Due != "" {
		w, err := c.parseWhen(in.Due, "due")
		if err != nil {
			return taskCreatedOut{}, err
		}
		due = &w
		c.setDue(cal, td, due)
	}
	if in.Repeat != "" {
		if due == nil {
			return taskCreatedOut{}, refuse("a repeating task needs a deadline (due): the first time it's due")
		}
		r, err := c.ruleIn(in.Repeat, *due)
		if err != nil {
			return taskCreatedOut{}, err
		}
		td.Props.Set(ics.Prop(ical.PropRecurrenceRule, r))
	}
	if in.Priority != "" {
		v, ok := priorityValue[strings.ToLower(in.Priority)]
		if !ok {
			return taskCreatedOut{}, refuse("priority: high, medium or low")
		}
		td.Props.Set(ics.Prop(ical.PropPriority, v))
	}
	setTags(td, in.Tags)
	if parent.UID != "" {
		setParent(td, parent.UID)
	}
	if n := strings.TrimSpace(in.Notes); n != "" {
		td.Props.SetText(ical.PropDescription, n)
	}
	touch(td)

	// The note's name, as the extension will pick it: free in the tasks folder, else with " (2)"…
	out := taskCreatedOut{UID: uid, Collection: name(col)}
	base := noteName(title)
	folder := strings.Trim(c.TasksFolder, "/")
	note := func(b string) string {
		if folder == "" {
			return b + ".md"
		}
		return folder + "/" + b + ".md"
	}
	out.Note = note(base)
	if c.Vault != nil {
		for i := 2; c.Vault.Exists(out.Note); i++ {
			out.Note = note(fmt.Sprintf("%s (%d)", base, i))
		}
		if out.Note != note(base) {
			out.Warning = fmt.Sprintf("a note %q already exists: this task's will be %q", note(base), out.Note)
		}
	}
	out.Link = "[[" + basename(out.Note) + "]]"
	if _, err := c.Davis.Put(ctx, hrefIn(col, uid), ics.Encode(cal), ""); err != nil {
		return taskCreatedOut{}, dav(err)
	}
	c.Log.Info("connector: task created", "uid", uid)
	return out, nil
}

type taskUpdateIn struct {
	Task       string    `json:"task" jsonschema:"its uid, title or note"`
	Title      *string   `json:"title,omitempty" jsonschema:"a new title: the extension renames the note"`
	Done       *bool     `json:"done,omitempty"`
	Due        *string   `json:"due,omitempty" jsonschema:"the deadline, local time; \"\" takes it off (and the repetition)"`
	Repeat     *string   `json:"repeat,omitempty" jsonschema:"an RRULE; \"\" takes it off"`
	Priority   *string   `json:"priority,omitempty" jsonschema:"\"\" takes it off"`
	Tags       *[]string `json:"tags,omitempty" jsonschema:"its tags, the whole list (an empty list takes them off)"`
	Parent     *string   `json:"parent,omitempty" jsonschema:"the parent task (uid, title or note); \"\" makes it a task of its own"`
	Collection string    `json:"collection,omitempty" jsonschema:"move it to this collection"`
	Notes      *string   `json:"notes,omitempty" jsonschema:"markdown: the reminder's notes, which become its note's body, while it has no note yet (then vault_edit); \"\" takes them off"`
}

type taskChangedOut struct {
	UID   string `json:"uid"`
	Done  string `json:"done"`
	Due   string `json:"due,omitempty" jsonschema:"a repeating task ticked: its next deadline"`
	Note  string `json:"note,omitempty"`
	Link  string `json:"link,omitempty"`
	Later string `json:"later,omitempty" jsonschema:"what the extension does at its next sync"`
}

func (c *Connector) updateTask(ctx context.Context, in taskUpdateIn) (taskChangedOut, error) {
	t, err := c.findTask(ctx, in.Task)
	if err != nil {
		return taskChangedOut{}, err
	}
	it := t.item
	td := master(it.cal, ical.CompToDo)
	out := taskChangedOut{UID: t.UID, Done: "changed"}
	notes := c.index()
	var later []string
	if in.Notes != nil {
		// The reminder's notes are the task's text only until its note is made (TaskSync.applyItem); the body is
		// the vault's afterwards, and the extension reads the notes no more.
		if p := notes.tasks[t.UID]; p != "" {
			return out, refuse("its note %q exists: change its body there (vault_read, vault_edit)", p)
		}
		if madeInObsidian(td) {
			return out, refuse("Obsidian has just made its note, which is on its way to this server: try again in a minute " +
				"(tasks_list then shows it), and change its body with vault_edit")
		}
		if n := strings.TrimSpace(*in.Notes); n != "" {
			td.Props.SetText(ical.PropDescription, n)
		} else {
			td.Props.Del(ical.PropDescription)
		}
	}
	if in.Title != nil {
		title := strings.TrimSpace(*in.Title)
		if title == "" {
			return out, refuse("a task needs a title")
		}
		td.Props.SetText(ical.PropSummary, title)
		if noteName(title) != noteName(t.Title) {
			later = append(later, fmt.Sprintf("the note is renamed %q", noteName(title)))
			out.Link = "[[" + noteName(title) + "]]"
		}
	}
	if in.Due != nil {
		if strings.TrimSpace(*in.Due) == "" {
			c.setDue(it.cal, td, nil)
			if td.Props.Get(ical.PropRecurrenceRule) != nil {
				td.Props.Del(ical.PropRecurrenceRule)
				later = append(later, "it doesn't repeat any more (a repetition needs a deadline)")
			}
		} else {
			w, err := c.parseWhen(*in.Due, "due")
			if err != nil {
				return out, err
			}
			c.setDue(it.cal, td, &w)
			if r := td.Props.Get(ical.PropRecurrenceRule); r != nil && in.Repeat == nil {
				if fitted, err := c.ruleIn(c.ruleOut(r.Value), w); err == nil {
					r.Value = fitted
				}
			}
		}
	}
	if in.Repeat != nil {
		if strings.TrimSpace(*in.Repeat) == "" {
			td.Props.Del(ical.PropRecurrenceRule)
		} else {
			due, ok := c.localOf(td.Props.Get(ical.PropDue))
			if !ok {
				return out, refuse("a repeating task needs a deadline (due)")
			}
			r, err := c.ruleIn(*in.Repeat, due)
			if err != nil {
				return out, err
			}
			td.Props.Set(ics.Prop(ical.PropRecurrenceRule, r))
		}
	}
	if in.Priority != nil {
		p := strings.ToLower(strings.TrimSpace(*in.Priority))
		switch {
		case p == "" || p == "none":
			td.Props.Del(ical.PropPriority)
		case priorityValue[p] == "":
			return out, refuse("priority: high, medium or low")
		case p != t.Priority:
			td.Props.Set(ics.Prop(ical.PropPriority, priorityValue[p]))
		}
	}
	if in.Tags != nil {
		setTags(td, *in.Tags)
	}
	if in.Parent != nil {
		if strings.TrimSpace(*in.Parent) == "" {
			setParent(td, "")
		} else {
			p, err := c.findTask(ctx, *in.Parent)
			if err != nil {
				return out, err
			}
			if p.UID == t.UID {
				return out, refuse("a task can't be its own parent")
			}
			setParent(td, p.UID)
		}
	}
	// A repeating task ticked comes back at its next date at once, as the extension moves it on (TaskSync.moveOn),
	// instead of staying done on the phone until an Obsidian syncs. Its note keeps the time it was done.
	var next *when
	completion := ""
	if in.Done != nil {
		due, hasDue := c.localOf(td.Props.Get(ical.PropDue))
		rule := c.ruleOf(td)
		switch {
		case *in.Done && rule != "" && hasDue:
			completion = time.Now().In(c.Loc).Format(timeLayout)
			if t.Done && t.Completed != "" {
				completion = t.Completed // ticked on the phone, not moved on yet
			}
			if n, ok := c.nextOccurrence(due, rule, time.Now()); ok {
				c.setDue(it.cal, td, &n)
				setDone(td, false)
				next = &n
				out.Done, out.Due = "ticked: it repeats, back for "+n.String(), n.String()
			} else {
				setDone(td, true)
				completion = ""
				out.Done = "ticked"
				later = append(later, "that was its last time (its repetition has ended): it stays done")
			}
		case *in.Done:
			setDone(td, true)
			out.Done = "ticked"
		default:
			setDone(td, false)
			out.Done = "unticked"
		}
	}
	touch(td)
	out.Note = notes.tasks[t.UID]
	moved := ""
	if in.Collection != "" {
		to, err := c.collection(ctx, in.Collection, ical.CompToDo)
		if err != nil {
			return out, err
		}
		if !davis.SamePath(to.Href, it.coll.Href) {
			if err := c.moveTo(ctx, it, to); err != nil {
				return out, err
			}
			moved = name(to)
		}
	}
	if moved == "" {
		if err := c.save(ctx, it); err != nil {
			return out, err
		}
		c.Log.Info("connector: task changed", "uid", t.UID)
	} else {
		c.Log.Info("connector: task moved", "uid", t.UID)
		out.Done += ", moved to " + moved
	}
	if next != nil && out.Note != "" {
		if err := c.movedOn(out.Note, *next, completion); err != nil {
			later = append(later, fmt.Sprintf("its note's list of times done (completions) misses %s: %v", completion, plain(err)))
		}
	}
	out.Later = strings.Join(later, "; ")
	return out, nil
}

// movedOn writes into a repeating task's note what the extension writes when it moves the task on
// (TaskSync.moveOn): not done, its next deadline, the time it was done added to completions. The extension then
// finds the note already in step with the reminder.
func (c *Connector) movedOn(p string, next when, at string) error {
	for try := 0; ; try++ {
		data, err := c.Vault.Read(p)
		if err != nil {
			return vaultErr(err)
		}
		if !vault.ReadMeta(data).Task() {
			return nil
		}
		text, err := vault.EditProps(string(data),
			vault.Edit{Key: "done", Value: false},
			vault.Edit{Key: "due", Value: next.String()},
			vault.Edit{Key: "completed", Delete: true},
			vault.Edit{Key: "completions", Value: at, Append: true})
		if err != nil {
			return err
		}
		err = c.Vault.Write(p, []byte(text), vault.Version(data), "task_update")
		if !errors.Is(err, vault.ErrChanged) || try == 2 {
			return vaultErr(err)
		}
	}
}

// nextOccurrence is when a repeating task ticked comes back, as the extension finds it (when.ts nextOccurrence):
// the rule's first date after its deadline and after today (a whole-day task) or now, on the wall clock. ok false:
// the rule has ended, and the task stays done. rule is ours (UNTIL its last day, ruleOut).
func (c *Connector) nextOccurrence(due when, rule string, now time.Time) (when, bool) {
	wall := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	}
	start := wall(due.t)
	if m := untilPart.FindStringSubmatch(rule); m != nil && len(m[2]) == 8 && !due.day {
		rule = untilPart.ReplaceAllString(rule, "${1}UNTIL="+m[2]+"T235959") // that last day included
	}
	opt, err := rrule.StrToROptionInLocation(rule, time.UTC)
	if err != nil {
		return when{}, false
	}
	opt.Dtstart = start
	r, err := rrule.NewRRule(*opt)
	if err != nil {
		return when{}, false
	}
	floor := wall(now.In(c.Loc))
	if due.day {
		floor = time.Date(floor.Year(), floor.Month(), floor.Day(), 0, 0, 0, 0, time.UTC)
	}
	it := r.Iterator()
	for k := 0; k < 5000; k++ {
		t, ok := it()
		if !ok {
			return when{}, false
		}
		if t.After(start) && t.After(floor) {
			return when{t: time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, c.Loc), day: due.day}, true
		}
	}
	return when{}, false
}

type taskRef struct {
	Task string `json:"task" jsonschema:"its uid, title or note"`
}

type taskDeleteIn struct {
	Task       string `json:"task" jsonschema:"its uid, title or note"`
	DeleteNote bool   `json:"deleteNote,omitempty" jsonschema:"delete its note too (to the server's trash: vault_restore brings it back, unlinked)"`
}

type taskDeletedOut struct {
	Done      string `json:"done"`
	UID       string `json:"uid"`
	Note      string `json:"note,omitempty" jsonschema:"its note, deleted with it"`
	RestoreID string `json:"restoreId,omitempty" jsonschema:"vault_restore brings the note back"`
}

// deleteTask deletes the reminder, and its note if asked. The note goes first: a device then sees it deleted, and
// its extension's own delete of the reminder, 10 seconds later, finds it gone (spec §10.2). The other way round, a
// device reading the calendar first unlinks the note while it's being deleted, and LiveSync may bring it back.
func (c *Connector) deleteTask(ctx context.Context, in taskDeleteIn) (taskDeletedOut, error) {
	t, err := c.findTask(ctx, in.Task)
	if err != nil {
		return taskDeletedOut{}, err
	}
	out := taskDeletedOut{UID: t.UID}
	note := c.index().tasks[t.UID]
	if in.DeleteNote && note != "" {
		b, err := c.Vault.Delete(note, "task_delete")
		if err != nil {
			return out, vaultErr(err)
		}
		out.Note, out.RestoreID = note, b.ID
	}
	if err := c.Davis.Delete(ctx, t.item.Href, t.item.ETag); err != nil {
		if out.RestoreID != "" {
			c.Vault.Restore(out.RestoreID)
		}
		return taskDeletedOut{}, dav(err)
	}
	c.Log.Info("connector: task deleted", "uid", t.UID)
	seen := madeInObsidian(master(t.item.cal, ical.CompToDo))
	switch {
	case out.Note != "":
		out.Done = "deleted, with its note"
	case note == "" && !seen:
		out.Done = "deleted (it had no note yet: none will be made)"
	case in.DeleteNote:
		out.Done = "deleted; its note isn't on this server yet: once it is (done and unlinked), vault_delete can remove it"
	default:
		out.Done = "deleted (its note stays, done and unlinked once Obsidian syncs; vault_delete can remove it)"
	}
	return out, nil
}

// liveTasks are the UIDs of the tasks on the calendar server.
func (c *Connector) liveTasks(ctx context.Context) (map[string]bool, error) {
	all, err := c.allTasks(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, t := range all {
		live[t.UID] = true
	}
	return live, nil
}

type scheduleIn struct {
	Task       string `json:"task" jsonschema:"its uid, title or note"`
	Start      string `json:"start" jsonschema:"local time: YYYY-MM-DDTHH:mm"`
	Minutes    int    `json:"minutes"`
	Collection string `json:"collection,omitempty" jsonschema:"default: the task's"`
}

func (c *Connector) scheduleTask(ctx context.Context, in scheduleIn) (createdOut, error) {
	t, err := c.findTask(ctx, in.Task)
	if err != nil {
		return createdOut{}, err
	}
	if in.Minutes < 5 || in.Minutes > 12*60 {
		return createdOut{}, refuse("minutes: between 5 and 720")
	}
	s, err := c.parseWhen(in.Start, "start")
	if err != nil {
		return createdOut{}, err
	}
	if s.day {
		return createdOut{}, refuse("a session has a time: YYYY-MM-DDTHH:mm")
	}
	want := in.Collection
	if want == "" {
		want = t.Collection
	}
	col, err := c.collection(ctx, want, ical.CompEvent)
	if err != nil {
		return createdOut{}, err
	}
	e := when{t: s.t.Add(time.Duration(in.Minutes) * time.Minute)}
	uid, err := c.putEvent(ctx, col, newEvent{title: t.Title, start: s, end: e, related: t.UID})
	if err != nil {
		return createdOut{}, err
	}
	return createdOut{UID: uid, Collection: name(col), Start: s.String(), End: e.String()}, nil
}
