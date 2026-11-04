package vault

import (
	"bytes"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Meta is what the connector needs of a note's properties: the extension's own (spec §10.2: a task note's and an
// event note's are Davis's), and the rest to compare.
type Meta struct {
	Type  string         // "task", "event" or ""
	UID   string         // caldav_uid
	RID   string         // recurrence_id
	Props map[string]any // every property (nil: none, or unreadable)
	// BadProps: the note starts with properties that can't be read (YAML Obsidian would show as invalid).
	BadProps bool
}

// Task is a note the extension keeps in step with a reminder.
func (m Meta) Task() bool { return m.Type == "task" }

// Event is an event's note.
func (m Meta) Event() bool { return m.Type == "event" }

// Split cuts a note into its properties' YAML (without the --- lines; ok false when it has none) and its body.
func Split(text string) (yaml string, body string, ok bool) {
	rest, found := strings.CutPrefix(text, "---\n")
	if !found {
		if rest, found = strings.CutPrefix(text, "---\r\n"); !found {
			return "", text, false
		}
	}
	for i := 0; i <= len(rest); {
		j := strings.IndexByte(rest[i:], '\n')
		line := rest[i:]
		if j >= 0 {
			line = rest[i : i+j]
		}
		if t := strings.TrimRight(line, "\r \t"); t == "---" {
			end := len(rest)
			if j >= 0 {
				end = i + j + 1
			}
			return rest[:i], rest[end:], true
		}
		if j < 0 {
			break
		}
		i += j + 1
	}
	return "", text, false
}

// ReadMeta reads a note's properties.
func ReadMeta(data []byte) Meta {
	y, _, ok := Split(string(data))
	if !ok {
		return Meta{}
	}
	var props map[string]any
	if err := yaml.Unmarshal([]byte(y), &props); err != nil {
		return Meta{BadProps: true}
	}
	m := Meta{Props: props}
	m.Type, _ = props["type"].(string)
	m.UID = scalar(props["caldav_uid"])
	m.RID = scalar(props["recurrence_id"])
	return m
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		b, _ := yaml.Marshal(x)
		return strings.TrimSpace(string(b))
	}
}

// Meta is p's properties, cached until the file changes.
func (v *Vault) Meta(p string) (Meta, error) {
	fi, err := v.Stat(p)
	if err != nil {
		return Meta{}, err
	}
	v.mu.Lock()
	c, ok := v.cache[p]
	v.mu.Unlock()
	if ok && c.mod.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.meta, nil
	}
	var m Meta
	if strings.EqualFold(pathExt(p), ".md") {
		data, err := v.root.ReadFile(n(p))
		if err != nil {
			return Meta{}, err
		}
		m = ReadMeta(data)
	}
	v.mu.Lock()
	v.cache[p] = cached{mod: fi.ModTime(), size: fi.Size(), meta: m}
	v.mu.Unlock()
	return m, nil
}

// Note is a note with its properties.
type Note struct {
	File
	Meta
}

// Notes are the vault's markdown notes, with their properties.
func (v *Vault) Notes() ([]Note, error) {
	files, _, err := v.Walk("")
	if err != nil {
		return nil, err
	}
	out := make([]Note, 0, len(files))
	for _, f := range files {
		if !strings.EqualFold(pathExt(f.Path), ".md") {
			continue
		}
		m, err := v.Meta(f.Path)
		if err != nil {
			continue // gone meanwhile
		}
		out = append(out, Note{File: f, Meta: m})
	}
	return out, nil
}

func pathExt(p string) string {
	i := strings.LastIndexByte(p, '.')
	if i < 0 || strings.Contains(p[i:], "/") {
		return ""
	}
	return p[i:]
}

// Frontmatter writes properties as a note's YAML block, in the order given (name, value pairs), the way the
// extension makes a note (spec §10.2: an event's note).
func Frontmatter(pairs ...any) string {
	doc := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i+1 < len(pairs); i += 2 {
		var val yaml.Node
		val.Encode(pairs[i+1])
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: pairs[i].(string)}, &val)
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	enc.Encode(doc)
	return "---\n" + b.String() + "---\n"
}
