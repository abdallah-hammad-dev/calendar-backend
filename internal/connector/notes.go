package connector

import (
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/vault"
)

// The vault tools are Claude Code's file tools on the vault the VPS mirrors (spec §10.3), with the extension's
// rules: a task note's properties and name are its reminder's, an event note's times its event's.

var (
	taskProps  = []string{"type", "caldav_uid", "done", "due", "repeat", "priority", "tags", "parent", "collection", "created", "completed", "completions"}
	eventProps = []string{"type", "start", "end", "caldav_uid", "recurrence_id"}
)

const (
	defaultList   = 200
	defaultSearch = 100
	defaultLines  = 2000
)

func (c *Connector) noteTools(s *mcp.Server) {
	add(c, s, &mcp.Tool{Name: "vault_list", Annotations: readOnly("List notes"),
		Description: "The vault's folders, and its files (newest first) under a folder, optionally matching a glob (\"*.md\", " +
			"\"Projets/**/*.md\"; without a \"/\" it matches file names). Hidden folders (.obsidian) aren't there."},
		c.vaultList)
	add(c, s, &mcp.Tool{Name: "vault_search", Annotations: readOnly("Search notes"),
		Description: "Searches the vault's notes for a regular expression (RE2; case-insensitive unless caseSensitive): " +
			"matching lines with their line numbers, and files whose path matches."},
		c.vaultSearch)
	add(c, s, &mcp.Tool{Name: "vault_read", Annotations: readOnly("Read a note"),
		Description: "Reads a note (or any text file of the vault): its content, and its version, which vault_write needs to " +
			"write it whole again. Long notes: offset and limit, in lines."},
		c.vaultRead)
	add(c, s, &mcp.Tool{Name: "vault_write", Annotations: changes("Write a note", true),
		Description: "Writes a note whole: a new one (folders are made), or an existing one again (pass the version " +
			"vault_read gave: refused if it changed since). For a change inside a note, vault_edit. Never a task note: " +
			"tasks are made with task_create; a task note's properties and an event note's times aren't changed here."},
		c.vaultWrite)
	add(c, s, &mcp.Tool{Name: "vault_edit", Annotations: changes("Edit a note", true),
		Description: "Replaces exact text in a note, as Claude Code's Edit: oldText must be there once (or replaceAll). In a " +
			"task note only the body changes here (its properties are task_update's)."},
		c.vaultEdit)
	add(c, s, &mcp.Tool{Name: "vault_move", Annotations: changes("Move or rename a note", true),
		Description: "Moves or renames a note, a file or a folder, and rewrites the links to it in every other note (as " +
			"Obsidian does). A task note can change folder, not name (rename the task with task_update)."},
		c.vaultMove)
	add(c, s, &mcp.Tool{Name: "vault_delete", Annotations: changes("Delete a note", true),
		Description: "Deletes a note, a file or a folder: gone from every device, kept in the server's trash (vault_restore " +
			"brings it back). A task's note goes with its task (task_delete, deleteNote), or once its task is deleted. Ask the " +
			"user first."},
		c.vaultDelete)
	add(c, s, &mcp.Tool{Name: "vault_restore", Annotations: changes("Restore a note", true),
		Description: "Without an id: what can be restored, the last first: deleted notes (the trash) and the versions changes " +
			"replaced (kept 30 days), optionally for one path. With an id: shows it (show) or restores it."},
		c.vaultRestore)
	add(c, s, &mcp.Tool{Name: "event_note", Annotations: changes("Get an event's note", false),
		Description: "The note of an event (one per occurrence of a series: pass its recurrenceId), made as the extension " +
			"makes it if there is none. Then read and write in it like any note (its start and end stay the event's)."},
		c.eventNote)
}

func (c *Connector) vaultOn() error {
	if c.Vault == nil {
		return refuse("the vault isn't mirrored on this server yet (livesync-bridge, spec §10.3): only the calendar and tasks are here")
	}
	return nil
}

// noteIndex are the notes the extension keeps in step with Davis, by UID.
type noteIndex struct {
	tasks  map[string]string // uid → path
	events map[string]string // uid + "\n" + recurrence id → path
}

func (c *Connector) index() noteIndex {
	idx := noteIndex{tasks: map[string]string{}, events: map[string]string{}}
	if c.Vault == nil {
		return idx
	}
	notes, err := c.Vault.Notes()
	if err != nil {
		return idx
	}
	for _, n := range notes {
		switch {
		case n.Task() && n.UID != "":
			idx.tasks[n.UID] = n.Path
		case n.Event() && n.UID != "":
			idx.events[n.UID+"\n"+n.RID] = n.Path
		}
	}
	return idx
}

func clean(p, what string) (string, error) {
	c, err := vault.Clean(p)
	if err != nil {
		return "", refusal{err.Error()}
	}
	if c == "" && what != "" {
		return "", refuse("%s is required", what)
	}
	return c, nil
}

// vaultErr says a vault error as Claude can act on it.
func vaultErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, vault.ErrNotFound):
		return refuse("%s (vault_list or vault_search find notes)", err)
	case errors.Is(err, vault.ErrExists), errors.Is(err, vault.ErrChanged):
		return refuse("%s: read it again (vault_read)", err)
	}
	return err
}

// --- reading

type listIn struct {
	Folder  string `json:"folder,omitempty" jsonschema:"default: the whole vault"`
	Pattern string `json:"pattern,omitempty" jsonschema:"a glob: *.md, Projets/**/*.md…"`
	Limit   int    `json:"limit,omitempty" jsonschema:"files shown, default 200"`
}

type listFilesOut struct {
	Folders   []string     `json:"folders"`
	Files     []vault.File `json:"files"`
	Total     int          `json:"total"`
	Truncated bool         `json:"truncated,omitempty"`
}

func globRegexp(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	brace := 0
	for i := 0; i < len(g); i++ {
		switch ch := g[i]; {
		case ch == '*' && i+1 < len(g) && g[i+1] == '*':
			i++
			if i+1 < len(g) && g[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?")
			} else {
				b.WriteString(".*")
			}
		case ch == '*':
			b.WriteString("[^/]*")
		case ch == '?':
			b.WriteString("[^/]")
		case ch == '{':
			brace++
			b.WriteString("(?:")
		case ch == '}' && brace > 0:
			brace--
			b.WriteString(")")
		case ch == ',' && brace > 0:
			b.WriteString("|")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	re, err := regexp.Compile("(?i)^" + b.String() + "$")
	if err != nil {
		return nil, refuse("pattern %q: %v", g, err)
	}
	return re, nil
}

func matcher(pattern, folder string) (func(string) bool, error) {
	if pattern == "" {
		return func(string) bool { return true }, nil
	}
	re, err := globRegexp(pattern)
	if err != nil {
		return nil, err
	}
	return func(p string) bool {
		if !strings.Contains(pattern, "/") {
			return re.MatchString(path.Base(p))
		}
		rel := strings.TrimPrefix(p, folder+"/")
		if folder == "" {
			rel = p
		}
		return re.MatchString(rel)
	}, nil
}

func (c *Connector) vaultList(ctx context.Context, in listIn) (listFilesOut, error) {
	if err := c.vaultOn(); err != nil {
		return listFilesOut{}, err
	}
	folder, err := clean(in.Folder, "")
	if err != nil {
		return listFilesOut{}, err
	}
	match, err := matcher(in.Pattern, folder)
	if err != nil {
		return listFilesOut{}, err
	}
	files, folders, err := c.Vault.Walk(folder)
	if err != nil {
		return listFilesOut{}, vaultErr(err)
	}
	out := listFilesOut{Folders: folders, Files: []vault.File{}}
	if out.Folders == nil {
		out.Folders = []string{}
	}
	for _, f := range files {
		if match(f.Path) {
			out.Files = append(out.Files, f)
		}
	}
	sort.SliceStable(out.Files, func(i, j int) bool { return out.Files[i].Modified.After(out.Files[j].Modified) })
	out.Total = len(out.Files)
	limit := in.Limit
	if limit <= 0 {
		limit = defaultList
	}
	if len(out.Files) > limit {
		out.Files, out.Truncated = out.Files[:limit], true
	}
	return out, nil
}

type searchIn struct {
	Query         string `json:"query" jsonschema:"a regular expression (RE2)"`
	Folder        string `json:"folder,omitempty"`
	Pattern       string `json:"pattern,omitempty" jsonschema:"only files matching this glob (default: every text file)"`
	CaseSensitive bool   `json:"caseSensitive,omitempty"`
	FilesOnly     bool   `json:"filesOnly,omitempty" jsonschema:"only the files that match, not their lines"`
	Limit         int    `json:"limit,omitempty" jsonschema:"default 100"`
}

type match struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty" jsonschema:"1-based; none: the path matches"`
	Text string `json:"text,omitempty"`
}

type searchOut struct {
	Matches   []match `json:"matches"`
	Truncated bool    `json:"truncated,omitempty"`
}

func (c *Connector) vaultSearch(ctx context.Context, in searchIn) (searchOut, error) {
	if err := c.vaultOn(); err != nil {
		return searchOut{}, err
	}
	if in.Query == "" {
		return searchOut{}, refuse("query is required")
	}
	q := in.Query
	if !in.CaseSensitive {
		q = "(?i)" + q
	}
	re, err := regexp.Compile(q)
	if err != nil {
		return searchOut{}, refuse("query: %v (RE2 syntax)", err)
	}
	folder, err := clean(in.Folder, "")
	if err != nil {
		return searchOut{}, err
	}
	keep, err := matcher(in.Pattern, folder)
	if err != nil {
		return searchOut{}, err
	}
	files, _, err := c.Vault.Walk(folder)
	if err != nil {
		return searchOut{}, vaultErr(err)
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].Modified.After(files[j].Modified) })
	limit := in.Limit
	if limit <= 0 {
		limit = defaultSearch
	}
	out := searchOut{Matches: []match{}}
	for _, f := range files {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if !keep(f.Path) {
			continue
		}
		if re.MatchString(f.Path) {
			out.Matches = append(out.Matches, match{Path: f.Path})
		}
		if f.Size > vault.MaxText {
			continue
		}
		data, err := c.Vault.Read(f.Path)
		if err != nil || !vault.IsText(data) {
			continue
		}
		found := false
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if in.FilesOnly {
				if !found && !re.MatchString(f.Path) {
					out.Matches = append(out.Matches, match{Path: f.Path})
				}
				found = true
				break
			}
			if r := []rune(line); len(r) > 300 {
				line = string(r[:300]) + "…"
			}
			out.Matches = append(out.Matches, match{Path: f.Path, Line: i + 1, Text: line})
			if len(out.Matches) >= limit {
				break
			}
		}
		if len(out.Matches) >= limit {
			out.Matches, out.Truncated = out.Matches[:limit], true
			break
		}
	}
	return out, nil
}

type readIn struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty" jsonschema:"the first line, 1-based"`
	Limit  int    `json:"limit,omitempty" jsonschema:"lines, default 2000"`
}

type readOut struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Version string `json:"version" jsonschema:"what vault_write needs to write it whole again"`
	Lines   int    `json:"lines"`
	From    int    `json:"from,omitempty" jsonschema:"the first line shown, when not all are"`
	Kind    string `json:"kind,omitempty" jsonschema:"task or event: a note the extension keeps in step with the calendar"`
	UID     string `json:"uid,omitempty" jsonschema:"its task's or event's uid"`
	About   string `json:"about,omitempty"`
}

func (c *Connector) vaultRead(ctx context.Context, in readIn) (readOut, error) {
	if err := c.vaultOn(); err != nil {
		return readOut{}, err
	}
	p, err := clean(in.Path, "path")
	if err != nil {
		return readOut{}, err
	}
	data, err := c.Vault.Read(p)
	if err != nil {
		return readOut{}, vaultErr(err)
	}
	if !vault.IsText(data) {
		return readOut{}, refuse("%q isn't a text file (%d bytes): it can be moved or deleted, not read here", p, len(data))
	}
	lines := strings.Split(string(data), "\n")
	out := readOut{Path: p, Content: string(data), Version: vault.Version(data), Lines: len(lines)}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLines
	}
	if in.Offset > 1 || len(lines) > limit {
		from := max(in.Offset, 1)
		if from > len(lines) {
			return out, refuse("the note has %d lines", len(lines))
		}
		to := min(from-1+limit, len(lines))
		out.Content, out.From = strings.Join(lines[from-1:to], "\n"), from
	}
	m := vault.ReadMeta(data)
	switch {
	case m.Task():
		out.Kind, out.UID = "task", m.UID
		out.About = "A task's note: its properties and its name are its reminder's (task_update); its body is free."
	case m.Event():
		out.Kind, out.UID = "event", m.UID
		out.About = "An event's note: start, end and the ids are the event's (event_update); the rest is free."
	}
	return out, nil
}

// --- writing

// guard refuses a new version of a note that takes from Davis what is Davis's (spec §10.2).
func guard(old *vault.Meta, next vault.Meta) error {
	if old != nil && old.Task() {
		for _, k := range taskProps {
			if !reflect.DeepEqual(old.Props[k], next.Props[k]) {
				return refuse("%q is the task's: change it with task_update; write freely in the note's body", k)
			}
		}
		return nil
	}
	if old != nil && old.Event() {
		for _, k := range eventProps {
			if !reflect.DeepEqual(old.Props[k], next.Props[k]) {
				return refuse("%q is the event's (event_update changes the event); the rest of its note is free", k)
			}
		}
		return nil
	}
	if next.Task() {
		return refuse("a task isn't written as a note: make it with task_create, the extension makes its note")
	}
	if next.Event() {
		return refuse("an event's note is made with event_note")
	}
	if next.BadProps && (old == nil || !old.BadProps) {
		return refuse("the properties (the YAML between the --- lines) don't read: fix them")
	}
	return nil
}

func isNote(p string) bool { return strings.EqualFold(path.Ext(p), ".md") }

type writeIn struct {
	Path    string `json:"path" jsonschema:"where, e.g. Projets/Idée.md"`
	Content string `json:"content"`
	Version string `json:"version,omitempty" jsonschema:"to write an existing note again: the version vault_read gave"`
}

type writeOut struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Created bool   `json:"created"`
}

func (c *Connector) vaultWrite(ctx context.Context, in writeIn) (writeOut, error) {
	if err := c.vaultOn(); err != nil {
		return writeOut{}, err
	}
	p, err := clean(in.Path, "path")
	if err != nil {
		return writeOut{}, err
	}
	if len(in.Content) > vault.MaxText {
		return writeOut{}, refuse("a note holds at most %d bytes here", vault.MaxText)
	}
	var old *vault.Meta
	if in.Version != "" {
		data, err := c.Vault.Read(p)
		if err != nil {
			return writeOut{}, vaultErr(err)
		}
		m := vault.ReadMeta(data)
		old = &m
	} else if c.Vault.Exists(p) {
		return writeOut{}, refuse("%q exists: read it (vault_read) and pass its version to write it again, or edit it (vault_edit)", p)
	}
	if isNote(p) {
		if err := guard(old, vault.ReadMeta([]byte(in.Content))); err != nil {
			return writeOut{}, err
		}
	}
	if err := c.Vault.Write(p, []byte(in.Content), in.Version, "vault_write"); err != nil {
		return writeOut{}, vaultErr(err)
	}
	return writeOut{Path: p, Version: vault.Version([]byte(in.Content)), Created: old == nil}, nil
}

type editIn struct {
	Path       string `json:"path"`
	OldText    string `json:"oldText" jsonschema:"the exact text to replace"`
	NewText    string `json:"newText"`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema:"every occurrence (else oldText must be there once)"`
}

type editOut struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Replaced int    `json:"replaced"`
}

func (c *Connector) vaultEdit(ctx context.Context, in editIn) (editOut, error) {
	if err := c.vaultOn(); err != nil {
		return editOut{}, err
	}
	p, err := clean(in.Path, "path")
	if err != nil {
		return editOut{}, err
	}
	if in.OldText == "" {
		return editOut{}, refuse("oldText is required (to add a note, vault_write)")
	}
	data, err := c.Vault.Read(p)
	if err != nil {
		return editOut{}, vaultErr(err)
	}
	if !vault.IsText(data) {
		return editOut{}, refuse("%q isn't a text file", p)
	}
	text := string(data)
	n := strings.Count(text, in.OldText)
	switch {
	case n == 0:
		return editOut{}, refuse("oldText isn't in %q (it may have changed: read it again)", p)
	case n > 1 && !in.ReplaceAll:
		return editOut{}, refuse("oldText is in %q %d times: give more of the text around it, or replaceAll", p, n)
	}
	next := strings.ReplaceAll(text, in.OldText, in.NewText)
	if !in.ReplaceAll {
		next = strings.Replace(text, in.OldText, in.NewText, 1)
		n = 1
	}
	if isNote(p) {
		m := vault.ReadMeta(data)
		if err := guard(&m, vault.ReadMeta([]byte(next))); err != nil {
			return editOut{}, err
		}
	}
	if err := c.Vault.Write(p, []byte(next), vault.Version(data), "vault_edit"); err != nil {
		return editOut{}, vaultErr(err)
	}
	return editOut{Path: p, Version: vault.Version([]byte(next)), Replaced: n}, nil
}

type moveIn struct {
	From string `json:"from"`
	To   string `json:"to" jsonschema:"the new path (a note keeps its .md)"`
}

type moveOut struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Rewritten []string `json:"linksRewrittenIn"`
}

// linkedTasks are the task notes under p (a note or a folder) still linked to a reminder.
func (c *Connector) linkedTasks(p string) ([]vault.Note, error) {
	notes, err := c.Vault.Notes()
	if err != nil {
		return nil, err
	}
	var out []vault.Note
	for _, n := range notes {
		if n.Task() && n.UID != "" && (n.Path == p || strings.HasPrefix(n.Path, p+"/")) {
			out = append(out, n)
		}
	}
	return out, nil
}

func (c *Connector) vaultMove(ctx context.Context, in moveIn) (moveOut, error) {
	if err := c.vaultOn(); err != nil {
		return moveOut{}, err
	}
	from, err := clean(in.From, "from")
	if err != nil {
		return moveOut{}, err
	}
	to, err := clean(in.To, "to")
	if err != nil {
		return moveOut{}, err
	}
	fi, err := c.Vault.Stat(from)
	if err != nil {
		return moveOut{}, vaultErr(fmt.Errorf("%q: %w", from, err))
	}
	if !fi.IsDir() && isNote(from) && !isNote(to) {
		return moveOut{}, refuse("a note keeps its .md: %q", to+".md")
	}
	tasks, err := c.linkedTasks(from)
	if err != nil {
		return moveOut{}, err
	}
	if !fi.IsDir() && len(tasks) > 0 && path.Base(from) != path.Base(to) {
		return moveOut{}, refuse("a task note's name is its task's title: rename the task with task_update (it can change folder here)")
	}
	re, err := c.Vault.Move(from, to)
	if err != nil {
		return moveOut{}, vaultErr(err)
	}
	if re == nil {
		re = []string{}
	}
	return moveOut{From: from, To: to, Rewritten: re}, nil
}

type pathIn struct {
	Path string `json:"path"`
}

type deleteOut struct {
	Deleted []string `json:"deleted"`
	ID      string   `json:"restoreId" jsonschema:"vault_restore brings it back"`
}

func (c *Connector) vaultDelete(ctx context.Context, in pathIn) (deleteOut, error) {
	if err := c.vaultOn(); err != nil {
		return deleteOut{}, err
	}
	p, err := clean(in.Path, "path")
	if err != nil {
		return deleteOut{}, err
	}
	tasks, err := c.linkedTasks(p)
	if err != nil {
		return deleteOut{}, err
	}
	if len(tasks) > 0 {
		// A note still linked to a reminder deleted elsewhere (the phone) can go: Obsidian would only unlink it.
		live, err := c.liveTasks(ctx)
		if err != nil {
			return deleteOut{}, err
		}
		var names []string
		for _, t := range tasks {
			if live[t.UID] {
				names = append(names, basename(t.Path))
			}
		}
		if len(names) > 0 {
			return deleteOut{}, refuse("still a task's note: %s. Delete the task with its note (task_delete, deleteNote), or "+
				"tick it (task_update done)", strings.Join(names, ", "))
		}
	}
	b, err := c.Vault.Delete(p, "vault_delete")
	if err != nil {
		return deleteOut{}, vaultErr(err)
	}
	return deleteOut{Deleted: b.Paths, ID: b.ID}, nil
}

type restoreIn struct {
	ID   string `json:"id,omitempty" jsonschema:"what to restore (from the list)"`
	Path string `json:"path,omitempty" jsonschema:"without id: only what concerns paths holding this"`
	Show bool   `json:"show,omitempty" jsonschema:"with id: show its content instead of restoring it"`
}

type savedOut struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind" jsonschema:"deleted (in the trash) or replaced (a version a change replaced)"`
	At    string   `json:"at"`
	Why   string   `json:"why"`
	Paths []string `json:"paths"`
}

type restoreOut struct {
	Saved    []savedOut        `json:"saved,omitempty"`
	Restored []string          `json:"restored,omitempty"`
	Content  map[string]string `json:"content,omitempty"`
}

func (c *Connector) vaultRestore(ctx context.Context, in restoreIn) (restoreOut, error) {
	if err := c.vaultOn(); err != nil {
		return restoreOut{}, err
	}
	if in.ID == "" {
		all, err := c.Vault.Batches()
		if err != nil {
			return restoreOut{}, err
		}
		out := restoreOut{Saved: []savedOut{}}
		for _, b := range all {
			if in.Path != "" && !strings.Contains(strings.ToLower(strings.Join(b.Paths, "\n")), strings.ToLower(in.Path)) {
				continue
			}
			kind := "replaced"
			if b.Trash {
				kind = "deleted"
			}
			out.Saved = append(out.Saved, savedOut{ID: b.ID, Kind: kind, At: b.At.In(c.Loc).Format(timeLayout), Why: b.Why, Paths: b.Paths})
			if len(out.Saved) >= 50 {
				break
			}
		}
		return out, nil
	}
	b, files, err := c.Vault.Batch(in.ID)
	if err != nil {
		return restoreOut{}, refusal{err.Error()}
	}
	if in.Show {
		out := restoreOut{Content: map[string]string{}}
		for p, data := range files {
			if vault.IsText(data) {
				out.Content[p] = string(data)
			} else {
				out.Content[p] = fmt.Sprintf("(%d bytes, not text)", len(data))
			}
		}
		return out, nil
	}
	var live map[string]bool
	var unlink []string // notes of tasks deleted since: back as the extension leaves them, done and unlinked
	for _, p := range b.Paths {
		if !isNote(p) {
			continue
		}
		next := vault.ReadMeta(files[p])
		var old *vault.Meta
		if data, err := c.Vault.Read(p); err == nil {
			m := vault.ReadMeta(data)
			old = &m
		}
		if old == nil && next.Task() && next.UID != "" {
			if live == nil {
				if live, err = c.liveTasks(ctx); err != nil {
					return restoreOut{}, err
				}
			}
			if live[next.UID] {
				return restoreOut{}, refuse("%q was the note of a task still there: its task makes it again", p)
			}
			unlink = append(unlink, p)
		}
		if old != nil {
			if err := guard(old, next); err != nil {
				return restoreOut{}, refuse("%q can't be restored whole: %v. Show it (show) and write its body back with vault_edit", p, err)
			}
		}
	}
	if _, err := c.Vault.Restore(in.ID); err != nil {
		return restoreOut{}, vaultErr(err)
	}
	for _, p := range unlink {
		data, err := c.Vault.Read(p)
		if err != nil {
			return restoreOut{}, vaultErr(err)
		}
		text, err := vault.EditProps(string(data), vault.Edit{Key: "done", Value: true},
			vault.Edit{Key: "caldav_uid", Delete: true}, vault.Edit{Key: "collection", Delete: true})
		if err != nil {
			return restoreOut{}, err
		}
		if err := c.Vault.Write(p, []byte(text), vault.Version(data), "vault_restore"); err != nil {
			return restoreOut{}, vaultErr(err)
		}
	}
	return restoreOut{Restored: b.Paths}, nil
}

// --- an event's note

type eventNoteOut struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Link    string `json:"link"`
}

// noteDate is an event's day as the extension's setting writes it in a note's name (event-notes.ts).
func noteDate(day time.Time, format string) string {
	switch format {
	case "YYYY-MM-DD":
		return day.Format("2006-01-02")
	case "DD.MM.YYYY":
		return day.Format("02.01.2006")
	}
	return day.Format("02-01-2006")
}

func (c *Connector) eventNote(ctx context.Context, in eventRef) (eventNoteOut, error) {
	if err := c.vaultOn(); err != nil {
		return eventNoteOut{}, err
	}
	it, err := c.find(ctx, in.UID, ical.CompEvent)
	if err != nil {
		return eventNoteOut{}, err
	}
	series := master(it.cal, ical.CompEvent)
	recurring := series.Props.Get(ical.PropRecurrenceRule) != nil || series.Props.Get(ical.PropRecurrenceDates) != nil
	id := ""
	if in.RecurrenceID != "" {
		if !recurring {
			return eventNoteOut{}, refuse("this event doesn't repeat: leave out recurrenceId")
		}
		if id, err = c.normRID(in.RecurrenceID); err != nil {
			return eventNoteOut{}, err
		}
	} else if recurring {
		return eventNoteOut{}, refuse("a series has a note per occurrence: pass the occurrence's recurrenceId (calendar_list)")
	}
	if p := c.index().events[in.UID+"\n"+id]; p != "" {
		return eventNoteOut{Path: p, Link: "[[" + basename(p) + "]]"}, nil
	}
	ev := series
	if id != "" {
		// Its exception, or one made for the occurrence (not saved): its own times.
		ev = c.exception(it.cal, series, id)
	}
	start, ok := c.localOf(ev.Props.Get(ical.PropDateTimeStart))
	if !ok {
		return eventNoteOut{}, refuse("this event has no start")
	}
	end, hasEnd := c.localOf(ev.Props.Get(ical.PropDateTimeEnd))
	title := text(ev, ical.PropSummary)
	if title == "" {
		title = "Event"
	}
	name := noteDate(start.t, c.EventDateFormat) + " " + noteName(title)
	names := []string{name}
	if !start.day {
		h := fmt.Sprintf("%dh", start.t.Hour())
		if m := start.t.Minute(); m != 0 {
			h += fmt.Sprintf("%02d", m)
		}
		names = append(names, name+" "+h)
	}
	folder := strings.Trim(c.EventsFolder, "/")
	at := func(b string) string {
		if folder == "" {
			return b + ".md"
		}
		return folder + "/" + b + ".md"
	}
	p := ""
	for _, nm := range names {
		if !c.Vault.Exists(at(nm)) {
			p = at(nm)
			break
		}
	}
	for i := 2; p == ""; i++ {
		if cand := at(fmt.Sprintf("%s (%d)", names[0], i)); !c.Vault.Exists(cand) {
			p = cand
		}
	}
	pairs := []any{"type", "event", "start", start.String()}
	if hasEnd {
		pairs = append(pairs, "end", end.String())
	}
	pairs = append(pairs, "caldav_uid", in.UID)
	if id != "" {
		pairs = append(pairs, "recurrence_id", id)
	}
	if err := c.Vault.Write(p, []byte(vault.Frontmatter(pairs...)), "", "event_note"); err != nil {
		return eventNoteOut{}, vaultErr(err)
	}
	return eventNoteOut{Path: p, Created: true, Link: "[[" + basename(p) + "]]"}, nil
}
