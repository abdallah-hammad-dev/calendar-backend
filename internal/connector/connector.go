// Package connector is the Claude connector's MCP server (spec §10): Claude's own chat (claude.ai, its apps) works
// on my vault and my calendar as Claude Code does in Claude Workspace. Tasks and events are written into Davis,
// whose clients (the iPhone, the extension in Obsidian) make what follows from them, task notes included; notes are
// written into the vault the VPS mirrors with livesync-bridge (internal/vault).
//
// Logs keep each call's tool and outcome, and UIDs: never contents, inputs, outputs or note paths (they are note
// titles).
package connector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/vault"
)

// Connector holds what the tools work on.
type Connector struct {
	Davis *davis.Client
	Home  string // my calendar home on Davis
	Me    string // my address there (my ATTENDEE)
	Loc   *time.Location
	Store *store.Store // the accounts mirrored into each collection (nil: not said)
	Vault *vault.Vault // nil: no vault mirrored here; the note tools say so
	// The extension's settings that matter here (they don't sync): where it makes task and event notes, and how it
	// writes an event note's day.
	TasksFolder, EventsFolder, EventDateFormat string
	// Wake starts a mirror pass (an answer to relay). May be nil.
	Wake    func()
	Log     *slog.Logger
	Version string

	mu     sync.Mutex
	cols   []davis.Calendar // collections, read at colsAt
	colsAt time.Time
}

// colsLife: the collections are read again after this (each Davis request costs ~200 ms: its password check).
const colsLife = 30 * time.Second

// Handler is the MCP endpoint (streamable HTTP, one request per call: nothing kept between them).
func (c *Connector) Handler() http.Handler {
	srv := c.Server()
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 8 << 20})
}

// Server is the MCP server with every tool.
func (c *Connector) Server() *mcp.Server {
	v := c.Version
	if v == "" {
		v = "dev"
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "calendar-backend", Title: "Mon coffre et mon calendrier", Version: v},
		&mcp.ServerOptions{Instructions: c.instructions()})
	c.calendarTools(s)
	c.taskTools(s)
	c.noteTools(s)
	return s
}

func (c *Connector) instructions() string {
	return fmt.Sprintf(`This connector is the user's Obsidian vault (synced to all their devices) and their calendar and reminders
(a CalDAV server, synced to their iPhone, and into Obsidian by the Calendar & Tasks extension). Work as Claude Code
does in their vault: read before you change, keep their folders, structure and style, link notes the Obsidian way.

- Collections ("Général", "École"…) are contexts: each holds events and tasks. Pick the one that fits what the user
  says (collections_list says what each holds); ask when it isn't clear.
- A task is a reminder: make it with task_create, never by writing a note. The extension makes its note (in
  "%[1]s/", named after the title, task_create's notes as its body) at its next sync. task_create returns its link:
  put it where it belongs, as a line "- [[Title]]" (drawn with its checkbox) or inline. The link resolves once the
  note exists. Until then the task's text is its reminder's notes: tasks_list shows them, task_update (notes)
  changes them; afterwards it is the note's body (vault_edit).
- A task note's properties (done, due, repeat, priority, tags, parent, collection) and its name are the reminder's:
  change them with task_update, never in the note. Its body is free, like any note's.
- A deadline (due) isn't time to work on it: that is a work session, task_schedule.
- An event's notes: event_note finds or makes its note, then write in it like any note.
- Times are the user's, in %[2]s: "YYYY-MM-DD" for a whole day, "YYYY-MM-DDTHH:mm" otherwise.
- Events copied from the user's other accounts (school, Google…: "account" in calendar_list) can only be answered
  (rsvp) or deleted.
- What you change reaches the user's devices at their next sync (the iPhone within minutes, Obsidian when it syncs).
- Deleted notes go to a trash on the server, and every change keeps the version it replaced for 30 days:
  vault_restore lists and restores them. Still, ask before deleting anything.`, c.TasksFolder, c.Loc)
}

// --- tools

// refusal is an answer for Claude (a bad input, a rule): said as it is, logged as "refused".
type refusal struct{ msg string }

func (r refusal) Error() string { return r.msg }

func refuse(format string, args ...any) error { return refusal{fmt.Sprintf(format, args...)} }

// davisError is a failure of Davis: its text (hrefs, statuses) can be logged.
type davisError struct{ err error }

func (e davisError) Error() string { return "the calendar server: " + e.err.Error() }
func (e davisError) Unwrap() error { return e.err }

func dav(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, davis.ErrPrecondition) {
		return refuse("it changed on the server meanwhile (another device): read it again, then retry")
	}
	return davisError{err}
}

var (
	yes = true
	no  = false
)

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &no}
}

func changes(title string, destructive bool) *mcp.ToolAnnotations {
	d := &no
	if destructive {
		d = &yes
	}
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: d, OpenWorldHint: &no}
}

// add registers a tool whose calls are logged without their inputs or outputs.
func add[In, Out any](c *Connector, s *mcp.Server, t *mcp.Tool, h func(context.Context, In) (Out, error)) {
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		out, err := h(ctx, in)
		attrs := []any{"tool", t.Name, "ms", time.Since(start).Milliseconds()}
		var r refusal
		var d davisError
		switch {
		case err == nil:
			c.Log.Info("connector call", attrs...)
		case errors.As(err, &r):
			c.Log.Info("connector call refused", attrs...)
		case errors.As(err, &d):
			c.Log.Warn("connector call failed", append(attrs, "err", d.err)...)
		default:
			c.Log.Warn("connector call failed", attrs...) // a vault error names the note: not logged
			err = errors.New("the server failed: " + plain(err))
		}
		return nil, out, err
	})
}

// plain is a vault error as Claude can act on it, without the server's own paths.
func plain(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "/notes/"); i >= 0 {
		msg = msg[i+len("/notes/"):]
	}
	return msg
}

// --- collections

type collectionOut struct {
	Name     string   `json:"name"`
	Holds    []string `json:"holds" jsonschema:"events, tasks"`
	Events   int      `json:"events" jsonschema:"events in the next 30 days"`
	Tasks    int      `json:"openTasks" jsonschema:"tasks not done"`
	Accounts []string `json:"accounts,omitempty" jsonschema:"the user's other accounts whose events are copied into it (read-only there)"`
}

type collectionsOut struct {
	TimeZone    string          `json:"timeZone"`
	Now         string          `json:"now" jsonschema:"the user's local time"`
	Collections []collectionOut `json:"collections"`
}

func (c *Connector) collections(ctx context.Context) ([]davis.Calendar, error) {
	c.mu.Lock()
	if time.Since(c.colsAt) < colsLife {
		defer c.mu.Unlock()
		return c.cols, nil
	}
	c.mu.Unlock()
	all, err := c.Davis.Calendars(ctx, c.Home)
	if err != nil {
		return nil, dav(err)
	}
	var out []davis.Calendar
	for _, cal := range all {
		if holds(cal, "VEVENT") || holds(cal, "VTODO") {
			out = append(out, cal)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName) })
	c.mu.Lock()
	c.cols, c.colsAt = out, time.Now()
	c.mu.Unlock()
	return out, nil
}

func holds(cal davis.Calendar, comp string) bool {
	if len(cal.Components) == 0 {
		return true // the server says nothing: anything goes
	}
	for _, x := range cal.Components {
		if strings.EqualFold(x, comp) {
			return true
		}
	}
	return false
}

func name(cal davis.Calendar) string {
	if cal.DisplayName != "" {
		return cal.DisplayName
	}
	return strings.TrimSuffix(cal.Href[strings.LastIndex(strings.TrimSuffix(cal.Href, "/"), "/")+1:], "/")
}

// collection is the collection Claude names, by its name (any case) or its address, among those holding comp
// ("": any).
func (c *Connector) collection(ctx context.Context, want, comp string) (davis.Calendar, error) {
	all, err := c.collections(ctx)
	if err != nil {
		return davis.Calendar{}, err
	}
	want = strings.TrimSpace(want)
	var names []string
	for _, cal := range all {
		if comp != "" && !holds(cal, comp) {
			continue
		}
		if strings.EqualFold(name(cal), want) || davis.SamePath(cal.Href, want) {
			return cal, nil
		}
		names = append(names, name(cal))
	}
	if want == "" {
		return davis.Calendar{}, refuse("say which collection: %s", strings.Join(names, ", "))
	}
	return davis.Calendar{}, refuse("no collection %q here; there are: %s", want, strings.Join(names, ", "))
}

func (c *Connector) accountsByCollection(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	if c.Store == nil {
		return out
	}
	list, err := c.Store.Accounts(ctx)
	if err != nil {
		return out
	}
	for _, a := range list {
		key := strings.TrimSuffix(a.Collection, "/")
		out[key] = append(out[key], a.Name)
	}
	return out
}

func (c *Connector) listCollections(ctx context.Context, _ struct{}) (collectionsOut, error) {
	all, err := c.collections(ctx)
	if err != nil {
		return collectionsOut{}, err
	}
	accounts := c.accountsByCollection(ctx)
	now := time.Now().In(c.Loc)
	out := collectionsOut{TimeZone: c.Loc.String(), Now: now.Format("2006-01-02T15:04 (Monday)"), Collections: []collectionOut{}}
	for _, cal := range all {
		co := collectionOut{Name: name(cal), Holds: []string{}, Accounts: accounts[strings.TrimSuffix(cal.Href, "/")]}
		if holds(cal, "VEVENT") {
			co.Holds = append(co.Holds, "events")
			items, err := c.Davis.Events(ctx, cal.Href, now, now.AddDate(0, 0, 30))
			if err != nil {
				return out, dav(err)
			}
			for _, it := range items {
				if cal, err := parse(it.Data); err == nil {
					co.Events += len(components(cal, "VEVENT"))
				}
			}
		}
		if holds(cal, "VTODO") {
			co.Holds = append(co.Holds, "tasks")
			items, err := c.Davis.Todos(ctx, cal.Href)
			if err != nil {
				return out, dav(err)
			}
			for _, it := range items {
				if t, ok := c.readTask(it.Data); ok && !t.Done {
					co.Tasks++
				}
			}
		}
		out.Collections = append(out.Collections, co)
	}
	return out, nil
}
