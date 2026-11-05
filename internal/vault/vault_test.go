package vault

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, files map[string]string) (*Vault, string) {
	t.Helper()
	dir := t.TempDir()
	for p, text := range files {
		full := filepath.Join(dir, "notes", p)
		os.MkdirAll(filepath.Dir(full), 0o777)
		if err := os.WriteFile(full, []byte(text), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	v, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v.Retouch = 0
	t.Cleanup(func() { v.Close() })
	return v, dir
}

func read(t *testing.T, v *Vault, p string) string {
	t.Helper()
	data, err := v.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"Notes/a.md": "Notes/a.md", "/Notes//a.md": "Notes/a.md", `Notes\a.md`: "Notes/a.md", "a/./b.md": "a/b.md", "": "",
	} {
		if got, err := Clean(in); err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"../x.md", "a/../../x.md", ".obsidian/app.json", "a/.trash/x.md", "a:b.md", "what?.md", "a /x.md"} {
		if _, err := Clean(bad); err == nil {
			t.Errorf("Clean(%q) accepted", bad)
		}
	}
}

func TestWriteVersionsAndHistory(t *testing.T) {
	v, dir := open(t, map[string]string{"a.md": "one", ".obsidian/app.json": "{}"})
	files, folders, err := v.Walk("")
	if err != nil || len(files) != 1 || files[0].Path != "a.md" || len(folders) != 0 {
		t.Fatalf("walk: %v %v %v", files, folders, err)
	}
	if err := v.Write("a.md", []byte("x"), "", "w"); !errors.Is(err, ErrExists) {
		t.Fatalf("new over an existing note: %v", err)
	}
	if err := v.Write("a.md", []byte("x"), "0000", "w"); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale version: %v", err)
	}
	if err := v.Write("a.md", []byte("two"), Version([]byte("one")), "edit"); err != nil {
		t.Fatal(err)
	}
	if err := v.Write("Projets/b.md", []byte("new"), "", "write"); err != nil {
		t.Fatal(err)
	}
	if read(t, v, "a.md") != "two" || read(t, v, "Projets/b.md") != "new" {
		t.Fatal("not written")
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	batches, _ := v.Batches()
	if len(batches) != 1 || batches[0].Trash || batches[0].Paths[0] != "a.md" {
		t.Fatalf("history: %+v", batches)
	}
	if _, err := v.Restore(batches[0].ID); err != nil || read(t, v, "a.md") != "one" {
		t.Fatalf("restore a version: %v", err)
	}
	// The version the restore replaced is kept too.
	if batches, _ = v.Batches(); len(batches) != 2 {
		t.Fatalf("after restore: %+v", batches)
	}

	// Old history goes.
	v.now = func() time.Time { return time.Now().Add(HistoryLife + time.Hour) }
	v.Write("a.md", []byte("three"), Version([]byte("one")), "edit")
	if batches, _ = v.Batches(); len(batches) != 1 {
		t.Fatalf("history not pruned: %+v", batches)
	}
}

func TestDeleteAndRestore(t *testing.T) {
	v, _ := open(t, map[string]string{"P/a.md": "a", "P/sub/b.md": "b", "c.md": "c"})
	b, err := v.Delete("P", "delete")
	if err != nil || !b.Folder || len(b.Paths) != 2 {
		t.Fatalf("delete folder: %+v %v", b, err)
	}
	if v.Exists("P/a.md") || v.Exists("P") {
		t.Fatal("still there")
	}
	v.Write("P/a.md", []byte("other"), "", "w")
	if _, err := v.Restore(b.ID); !errors.Is(err, ErrExists) {
		t.Fatalf("restore over a new note: %v", err)
	}
	v.Delete("P/a.md", "delete")
	if _, err := v.Restore(b.ID); err != nil || read(t, v, "P/sub/b.md") != "b" || read(t, v, "P/a.md") != "a" {
		t.Fatalf("restore: %v", err)
	}
	if _, _, err := v.Batch(b.ID); err == nil {
		t.Fatal("a restored delete stays in the trash")
	}
	if _, _, err := v.Batch("../x"); err == nil {
		t.Fatal("batch id outside")
	}
}

func TestMeta(t *testing.T) {
	v, _ := open(t, map[string]string{
		"Tasks/Buy.md":  "---\ntype: task\ncaldav_uid: abc\ndone: false\n---\nbody",
		"Cal/ev.md":     "---\r\ntype: event\r\ncaldav_uid: e1\r\nrecurrence_id: \"20261103\"\r\n---\r\n",
		"plain.md":      "no properties\n---\n",
		"bad.md":        "---\ntype: [task\n---\n",
		"img.png":       "\x89PNG",
		"Tasks/Num.md":  "---\ntype: task\ncaldav_uid: 123\n---\n",
		"Tasks/Open.md": "---\ntype: task\n",
	})
	notes, err := v.Notes()
	if err != nil || len(notes) != 6 {
		t.Fatalf("notes: %v %v", notes, err)
	}
	by := map[string]Meta{}
	for _, n := range notes {
		by[n.Path] = n.Meta
	}
	if m := by["Tasks/Buy.md"]; !m.Task() || m.UID != "abc" || m.Props["done"] != false {
		t.Errorf("task: %+v", m)
	}
	if m := by["Cal/ev.md"]; !m.Event() || m.RID != "20261103" {
		t.Errorf("event: %+v", m)
	}
	if m := by["Tasks/Num.md"]; m.UID != "123" {
		t.Errorf("number uid: %+v", m)
	}
	if m := by["bad.md"]; !m.BadProps {
		t.Errorf("bad: %+v", m)
	}
	if m := by["plain.md"]; m.Props != nil || m.Type != "" {
		t.Errorf("plain: %+v", m)
	}
	if m := by["Tasks/Open.md"]; m.Props != nil {
		t.Errorf("unclosed: %+v", m)
	}
	fm := Frontmatter("type", "event", "start", "2026-11-03", "end", "2026-11-04", "caldav_uid", "x", "recurrence_id", "20261103")
	want := "---\ntype: event\nstart: \"2026-11-03\"\nend: \"2026-11-04\"\ncaldav_uid: x\nrecurrence_id: \"20261103\"\n---\n"
	if fm != want {
		t.Errorf("frontmatter:\n%s", fm)
	}
	if m := ReadMeta([]byte(fm)); m.RID != "20261103" || m.Props["start"] != "2026-11-03" {
		t.Errorf("read back: %+v", m)
	}
}

func TestMoveRewritesLinks(t *testing.T) {
	v, _ := open(t, map[string]string{
		"Projets/Idée.md": "# Idée\nsee [[Autre]]",
		"Autre.md":        "x",
		"Journal/j.md": strings.Join([]string{
			"[[Idée]] and [[Idée#Plan|le plan]] and ![[Idée]]",
			"[[Projets/Idée]] and [[idée]]",
			"[md](../Projets/Id%C3%A9e.md) and [root](Projets/Idée.md)",
			"[web](https://x.test/Idée.md) [[Other]]",
			"```",
			"[[Idée]] in code",
			"```",
		}, "\n"),
		"Journal/k.md": "nothing to change [[Autre]]",
	})
	re, err := v.Move("Projets/Idée.md", "Archive/Idée 2026.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(re) != 1 || re[0] != "Journal/j.md" {
		t.Fatalf("rewritten: %v", re)
	}
	got := read(t, v, "Journal/j.md")
	want := strings.Join([]string{
		"[[Idée 2026]] and [[Idée 2026#Plan|le plan]] and ![[Idée 2026]]",
		"[[Archive/Idée 2026]] and [[Idée 2026]]",
		"[md](../Archive/Idée%202026.md) and [root](Archive/Idée%202026.md)",
		"[web](https://x.test/Idée.md) [[Other]]",
		"```",
		"[[Idée]] in code",
		"```",
	}, "\n")
	if got != want {
		t.Fatalf("links:\n%s\nwant:\n%s", got, want)
	}
	if read(t, v, "Archive/Idée 2026.md") != "# Idée\nsee [[Autre]]" || v.Exists("Projets/Idée.md") {
		t.Fatal("not moved")
	}
	batches, _ := v.Batches()
	if len(batches) != 1 || batches[0].Paths[0] != "Journal/j.md" {
		t.Fatalf("history of the links: %+v", batches)
	}
}

func TestMoveFolderKeepsNamesAndAmbiguity(t *testing.T) {
	v, _ := open(t, map[string]string{
		"A/Note.md":     "[x](sub/Deep.md) [[Note]]",
		"A/sub/Deep.md": "d",
		"B/Note.md":     "other note with the same name",
		"Index.md":      "[[A/Note]] [[Deep]]",
	})
	re, err := v.Move("A", "Z/A")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, v, "Index.md"); got != "[[Z/A/Note]] [[Deep]]" {
		t.Fatalf("index: %q (rewritten %v)", got, re)
	}
	// Its own relative link and bare self-link still hold: unchanged.
	if got := read(t, v, "Z/A/Note.md"); got != "[x](sub/Deep.md) [[Note]]" {
		t.Fatalf("moved note: %q", got)
	}
	if _, err := v.Move("Z", "Z/in"); err == nil {
		t.Fatal("folder moved into itself")
	}
	if _, err := v.Move("Index.md", "B/Note.md"); !errors.Is(err, ErrExists) {
		t.Fatalf("move over a note: %v", err)
	}
}

func TestRetouch(t *testing.T) {
	v, dir := open(t, nil)
	v.Retouch = 50 * time.Millisecond
	if err := v.Write("New/a.md", []byte("x"), "", "w"); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, "notes", "New", "a.md")
	before, _ := os.Stat(full)
	time.Sleep(200 * time.Millisecond)
	after, _ := os.Stat(full)
	if os.SameFile(before, after) {
		t.Fatal("a file in a new folder wasn't written again")
	}

	// Moved: every file, a folder's too.
	if _, err := v.Move("New", "Moved/Here"); err != nil {
		t.Fatal(err)
	}
	full = filepath.Join(dir, "notes", "Moved", "Here", "a.md")
	before, _ = os.Stat(full)
	time.Sleep(200 * time.Millisecond)
	after, _ = os.Stat(full)
	if os.SameFile(before, after) {
		t.Fatal("a moved file wasn't written again")
	}
	if data, _ := os.ReadFile(full); string(data) != "x" {
		t.Fatalf("moved file: %q", data)
	}
}

// A repeating task moved on, as the extension's processFrontMatter writes it (TaskSync.moveOn): the keys edited in
// place, the rest as it was, days and times plain.
func TestEditProps(t *testing.T) {
	note := "---\ntype: task\ndone: false\ndue: 2026-11-10\nrepeat: every tuesday\ntags:\n  - atlas\nparent: \"[[Projet]]\"\n" +
		"completed: 2026-11-10T09:00\ncaldav_uid: u-1\n---\n# Corps\n\n- [ ] une étape\n"
	got, err := EditProps(note,
		Edit{Key: "done", Value: false},
		Edit{Key: "due", Value: "2026-11-17"},
		Edit{Key: "completed", Delete: true},
		Edit{Key: "completions", Value: "2026-11-10T09:00", Append: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "---\ntype: task\ndone: false\ndue: 2026-11-17\nrepeat: every tuesday\ntags:\n  - atlas\nparent: \"[[Projet]]\"\n" +
		"caldav_uid: u-1\ncompletions:\n  - 2026-11-10T09:00\n---\n# Corps\n\n- [ ] une étape\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	again, _ := EditProps(got, Edit{Key: "completions", Value: "2026-11-17T08:30", Append: true})
	if !strings.Contains(again, "completions:\n  - 2026-11-10T09:00\n  - 2026-11-17T08:30\n---") {
		t.Fatalf("appended: %s", again)
	}
	one, _ := EditProps("---\ncompletions: 2026-11-03\n---\n", Edit{Key: "completions", Value: "2026-11-10", Append: true})
	if one != "---\ncompletions:\n  - 2026-11-03\n  - 2026-11-10\n---\n" {
		t.Fatalf("one value: %q", one)
	}
	unlinked, _ := EditProps("---\ntype: task\ncaldav_uid: u\ncollection: Général\n---\nb", Edit{Key: "done", Value: true},
		Edit{Key: "caldav_uid", Delete: true}, Edit{Key: "collection", Delete: true})
	if unlinked != "---\ntype: task\ndone: true\n---\nb" {
		t.Fatalf("unlinked: %q", unlinked)
	}
	if m := ReadMeta([]byte(got)); m.UID != "u-1" || !m.Task() {
		t.Fatalf("reads back: %+v", m)
	}
	if _, err := EditProps("---\n- a\n---\n", Edit{Key: "done", Value: true}); err == nil {
		t.Fatal("a list as properties")
	}
}
