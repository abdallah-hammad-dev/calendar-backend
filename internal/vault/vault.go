// Package vault is the Obsidian vault as the VPS holds it (spec §10.3): a folder livesync-bridge keeps identical to
// my LiveSync database, read and written here as plain files. Every change is atomic (a temporary file renamed
// into place), keeps the version it replaces (history) and puts what it deletes in a trash: neither is synced.
//
// The directory given to Open holds:
//
//	notes/    the vault, the bridge's folder
//	history/  the versions changes replaced, a batch per change, kept HistoryLife
//	trash/    what was deleted, a batch per delete, kept until restored
//	tmp/      files being written, renamed into notes/ once whole (the same volume, never synced)
//
// Paths are relative to notes/, with "/" (as Obsidian writes them). Hidden files and folders (.obsidian/…) are
// neither shown nor written: LiveSync doesn't sync them here, nor does the bridge.
package vault

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	notes   = "notes"
	history = "history"
	trash   = "trash"
	tmp     = "tmp"

	// HistoryLife is how long the versions a change replaced are kept.
	HistoryLife = 30 * 24 * time.Hour
	// MaxText is the largest file read or written as text.
	MaxText = 2 << 20
)

var (
	ErrNotFound = errors.New("no such note")
	ErrExists   = errors.New("already exists")
	// ErrChanged: the file isn't the version the caller read any more.
	ErrChanged = errors.New("changed since it was read")
)

// Vault is the vault on disk. Changes go one at a time.
type Vault struct {
	root *os.Root
	// Retouch: after a write that made a new folder, and after a move, the file is written again this much later.
	// The bridge's watcher misses a file written right after its folder was made, and every file moved into a new
	// folder or under a renamed one: the devices then lose the old path and never get the new one (spec §10.3,
	// bench/TESTS.md 2026-11-04). 0: never.
	Retouch time.Duration

	mu    sync.Mutex
	cache map[string]cached
	now   func() time.Time
}

type cached struct {
	mod  time.Time
	size int64
	meta Meta
}

// Open opens the vault's directory, making its parts if needed.
func Open(dir string) (*Vault, error) {
	for _, d := range []string{notes, history, trash, tmp} {
		if err := os.MkdirAll(dir+"/"+d, 0o777); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Vault{root: root, Retouch: 3 * time.Second, cache: map[string]cached{}, now: time.Now}, nil
}

func (v *Vault) Close() error { return v.root.Close() }

// Clean checks a path given by Claude and returns it as the vault writes it: relative, "/", no "..", nothing
// hidden, no character a device's file system refuses.
func Clean(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return "", nil
	}
	c := path.Clean(p)
	for _, seg := range strings.Split(c, "/") {
		switch {
		case seg == "..":
			return "", fmt.Errorf("%q: paths stay inside the vault", p)
		case strings.HasPrefix(seg, "."):
			return "", fmt.Errorf("%q: hidden files and folders (.obsidian…) aren't synced here", p)
		case strings.ContainsAny(seg, `:*?"<>|`) || strings.ContainsFunc(seg, func(r rune) bool { return r < 32 }):
			return "", fmt.Errorf(`%q: a name can't hold : * ? " < > | (Windows and iOS refuse them)`, p)
		case strings.TrimSpace(seg) != seg:
			return "", fmt.Errorf("%q: a name can't start or end with a space", p)
		}
	}
	return c, nil
}

func n(p string) string {
	if p == "" {
		return notes
	}
	return notes + "/" + p
}

// File is a file of the vault.
type File struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Walk lists the files and folders under folder ("" = the whole vault), hidden ones left out.
func (v *Vault) Walk(folder string) (files []File, folders []string, err error) {
	start := n(folder)
	if fi, err := v.root.Stat(start); err != nil || !fi.IsDir() {
		return nil, nil, fmt.Errorf("%q: %w", folder, ErrNotFound)
	}
	err = fs.WalkDir(v.root.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, notes), "/")
		if strings.HasPrefix(d.Name(), ".") && rel != "" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != start {
				folders = append(folders, rel)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // gone meanwhile
		}
		files = append(files, File{Path: rel, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	return files, folders, err
}

// Stat says whether p is a file, a folder or nothing.
func (v *Vault) Stat(p string) (fs.FileInfo, error) {
	fi, err := v.root.Lstat(n(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return fi, err
}

func (v *Vault) Exists(p string) bool {
	_, err := v.root.Lstat(n(p))
	return err == nil
}

// Version names a file's content: what Write checks against.
func Version(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// Read is a file's content.
func (v *Vault) Read(p string) ([]byte, error) {
	fi, err := v.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", p, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%q is a folder", p)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%q isn't a file", p)
	}
	return v.root.ReadFile(n(p))
}

// IsText tells a file that can be read and edited as text.
func IsText(data []byte) bool {
	return len(data) <= MaxText && utf8.Valid(data) && !bytes.ContainsRune(data, 0)
}

// --- changes

// Write puts content at p. version "" makes a new file (ErrExists if there is one); otherwise the file must still
// be that version (ErrChanged). The version replaced goes to the history, under why.
func (v *Vault) Write(p string, content []byte, version, why string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	old, err := v.current(p, version)
	if err != nil {
		return err
	}
	var batch *Batch
	if old != nil {
		if bytes.Equal(old, content) {
			return nil
		}
		if batch, err = v.save(history, why, map[string][]byte{p: old}); err != nil {
			return err
		}
	}
	made, err := v.put(p, content)
	if err != nil {
		if batch != nil {
			v.root.RemoveAll(history + "/" + batch.ID)
		}
		return err
	}
	if made {
		v.retouch(p, content)
	}
	return nil
}

// current is p's content if it is at version (nil: there is no file, as version "" asks).
func (v *Vault) current(p, version string) ([]byte, error) {
	data, err := v.Read(p)
	switch {
	case errors.Is(err, ErrNotFound) && version == "":
		return nil, nil
	case errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("%q: %w", p, ErrNotFound)
	case err != nil:
		return nil, err
	case version == "":
		return nil, fmt.Errorf("%q %w", p, ErrExists)
	case Version(data) != version:
		return nil, fmt.Errorf("%q %w", p, ErrChanged)
	}
	return data, nil
}

// put writes p whole, atomically. made: a folder had to be made for it.
func (v *Vault) put(p string, content []byte) (made bool, err error) {
	if dir := path.Dir(p); dir != "." {
		if fi, err := v.root.Stat(n(dir)); err != nil {
			made = true
			if err := v.root.MkdirAll(n(dir), 0o777); err != nil {
				return false, err
			}
		} else if !fi.IsDir() {
			return false, fmt.Errorf("%q is a file, not a folder", dir)
		}
	}
	t := tmp + "/" + randomID()
	if err := v.root.WriteFile(t, content, 0o666); err != nil {
		return false, err
	}
	if err := v.root.Rename(t, n(p)); err != nil {
		v.root.Remove(t)
		return false, err
	}
	return made, nil
}

func (v *Vault) retouch(p string, content []byte) {
	if v.Retouch <= 0 {
		return
	}
	version := Version(content)
	time.AfterFunc(v.Retouch, func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		if data, err := v.Read(p); err == nil && Version(data) == version {
			v.put(p, data)
		}
	})
}

// Move renames a file or a folder and rewrites, in every other note, the links that pointed into it (spec §10.3:
// a device only sees a note gone and another made, and updates nothing). rewritten are the notes whose links
// changed. A folder's files keep their names under the new folder.
func (v *Vault) Move(from, to string) (rewritten []string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if from == "" || to == "" {
		return nil, errors.New("the vault itself can't be moved")
	}
	fi, err := v.Stat(from)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", from, err)
	}
	if v.Exists(to) && !strings.EqualFold(from, to) {
		return nil, fmt.Errorf("%q %w", to, ErrExists)
	}
	if fi.IsDir() && (to == from || strings.HasPrefix(to+"/", from+"/")) {
		return nil, fmt.Errorf("a folder can't move into itself")
	}
	all, _, err := v.Walk("")
	if err != nil {
		return nil, err
	}
	moves := map[string]string{}
	if fi.IsDir() {
		for _, f := range all {
			if strings.HasPrefix(f.Path, from+"/") {
				moves[f.Path] = to + strings.TrimPrefix(f.Path, from)
			}
		}
	} else {
		moves[from] = to
	}
	paths := make([]string, len(all))
	for i, f := range all {
		paths[i] = f.Path
	}
	texts := map[string]string{}
	for _, p := range paths {
		if strings.EqualFold(path.Ext(p), ".md") {
			if data, err := v.root.ReadFile(n(p)); err == nil && IsText(data) {
				texts[p] = string(data)
			}
		}
	}
	changed := RewriteLinks(paths, moves, texts) // by the note's old path

	// The history keeps the versions of the notes whose links change, under their old paths.
	if len(changed) > 0 {
		old := map[string][]byte{}
		for p := range changed {
			old[p] = []byte(texts[p])
		}
		if _, err := v.save(history, "links rewritten after a move", old); err != nil {
			return nil, err
		}
	}
	if dir := path.Dir(to); dir != "." {
		if err := v.root.MkdirAll(n(dir), 0o777); err != nil {
			return nil, err
		}
	}
	if err := v.root.Rename(n(from), n(to)); err != nil {
		return nil, err
	}
	for p, text := range changed {
		at := p
		if m, ok := moves[p]; ok {
			at = m
		}
		if _, err := v.put(at, []byte(text)); err != nil {
			return rewritten, err
		}
		rewritten = append(rewritten, at)
	}
	for _, at := range moves {
		if data, err := v.root.ReadFile(n(at)); err == nil {
			v.retouch(at, data)
		}
	}
	sort.Strings(rewritten)
	return rewritten, nil
}

// Delete puts a file or a folder in the trash (gone from every device, back with Restore).
func (v *Vault) Delete(p, why string) (Batch, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	fi, err := v.Stat(p)
	if err != nil {
		return Batch{}, fmt.Errorf("%q: %w", p, err)
	}
	files := map[string][]byte{}
	if fi.IsDir() {
		list, _, err := v.Walk(p)
		if err != nil {
			return Batch{}, err
		}
		for _, f := range list {
			if files[f.Path], err = v.root.ReadFile(n(f.Path)); err != nil {
				return Batch{}, err
			}
		}
	} else if files[p], err = v.root.ReadFile(n(p)); err != nil {
		return Batch{}, err
	}
	b, err := v.save(trash, why, files)
	if err != nil {
		return Batch{}, err
	}
	b.Folder = fi.IsDir()
	if err := v.root.RemoveAll(n(p)); err != nil {
		return *b, err
	}
	return *b, nil
}

// --- history and trash

// Batch is what one change replaced (history) or one delete removed (trash).
type Batch struct {
	ID     string    `json:"id"`
	Trash  bool      `json:"trash"`
	At     time.Time `json:"at"`
	Why    string    `json:"why"`
	Paths  []string  `json:"paths"`
	Folder bool      `json:"folder,omitempty"`
}

func (v *Vault) save(where, why string, files map[string][]byte) (*Batch, error) {
	at := v.now().UTC()
	// Sorted by time to the microsecond, then a random part.
	b := &Batch{ID: fmt.Sprintf("%s-%06d-%s", at.Format("20060102-150405"), at.Nanosecond()/1000, randomID()[:4]),
		Trash: where == trash, At: at, Why: why}
	for p := range files {
		b.Paths = append(b.Paths, p)
	}
	sort.Strings(b.Paths)
	dir := where + "/" + b.ID
	if err := v.root.Mkdir(dir, 0o777); err != nil {
		return nil, err
	}
	for i, p := range b.Paths {
		if err := v.root.WriteFile(fmt.Sprintf("%s/%d", dir, i), files[p], 0o666); err != nil {
			return nil, err
		}
	}
	meta, _ := json.Marshal(b)
	if err := v.root.WriteFile(dir+"/meta.json", meta, 0o666); err != nil {
		return nil, err
	}
	if where == history {
		v.prune()
	}
	return b, nil
}

// prune drops history batches older than HistoryLife.
func (v *Vault) prune() {
	entries, err := fs.ReadDir(v.root.FS(), history)
	if err != nil {
		return
	}
	limit := v.now().UTC().Add(-HistoryLife).Format("20060102-150405")
	for _, e := range entries {
		if e.Name() < limit {
			v.root.RemoveAll(history + "/" + e.Name())
		}
	}
}

// Batches lists the history and the trash, the last first.
func (v *Vault) Batches() ([]Batch, error) {
	var out []Batch
	for _, where := range []string{trash, history} {
		entries, err := fs.ReadDir(v.root.FS(), where)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			data, err := v.root.ReadFile(where + "/" + e.Name() + "/meta.json")
			if err != nil {
				continue
			}
			var b Batch
			if json.Unmarshal(data, &b) == nil {
				out = append(out, b)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// Batch finds one batch, with its files' contents.
func (v *Vault) Batch(id string) (Batch, map[string][]byte, error) {
	if strings.ContainsAny(id, "/.\\") || id == "" {
		return Batch{}, nil, fmt.Errorf("no saved change %q", id)
	}
	for _, where := range []string{trash, history} {
		data, err := v.root.ReadFile(where + "/" + id + "/meta.json")
		if err != nil {
			continue
		}
		var b Batch
		if err := json.Unmarshal(data, &b); err != nil {
			return b, nil, err
		}
		files := map[string][]byte{}
		for i, p := range b.Paths {
			if files[p], err = v.root.ReadFile(fmt.Sprintf("%s/%s/%d", where, id, i)); err != nil {
				return b, nil, err
			}
		}
		return b, files, nil
	}
	return Batch{}, nil, fmt.Errorf("no saved change %q (vault_restore without an id lists them)", id)
}

// Restore puts a batch's files back where they were: a deleted file or folder (refused where something is in
// its place now), or the versions a change replaced (the current ones go to the history first). A trash batch
// leaves the trash.
func (v *Vault) Restore(id string) (Batch, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	b, files, err := v.Batch(id)
	if err != nil {
		return b, err
	}
	if b.Trash {
		for _, p := range b.Paths {
			if v.Exists(p) {
				return b, fmt.Errorf("%q %w: move or delete it first", p, ErrExists)
			}
		}
	} else {
		current := map[string][]byte{}
		for _, p := range b.Paths {
			if data, err := v.root.ReadFile(n(p)); err == nil {
				current[p] = data
			}
		}
		if len(current) > 0 {
			if _, err := v.save(history, "replaced by a restore of "+id, current); err != nil {
				return b, err
			}
		}
	}
	for _, p := range b.Paths {
		if _, err := v.put(p, files[p]); err != nil {
			return b, err
		}
	}
	if b.Trash {
		v.root.RemoveAll(trash + "/" + id)
	}
	return b, nil
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
