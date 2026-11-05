package vault

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
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

// Edit is one property change for EditProps: Value set (where the property is, else added last), Delete takes it
// off, Append adds Value to its list (a single value becomes a list; none, a list of one).
type Edit struct {
	Key    string
	Value  any
	Delete bool
	Append bool
}

// EditProps changes a note's properties as the extension does through Obsidian's processFrontMatter (spec §10.2:
// a repeating task moved on): only the keys edited change; the others, their order, their spelling and the body
// stay. A note without properties gets them.
func EditProps(text string, edits ...Edit) (string, error) {
	y, body, ok := Split(text)
	if !ok {
		body = text
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		return "", fmt.Errorf("the note's properties don't read: %w", err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return "", errors.New("the note's properties aren't names with values")
	}
	for _, e := range edits {
		at := -1
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == e.Key {
				at = i
				break
			}
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Value: e.Key}
		switch {
		case e.Delete:
			if at >= 0 {
				m.Content = append(m.Content[:at], m.Content[at+2:]...)
			}
		case e.Append:
			item := valueNode(e.Value)
			switch {
			case at < 0:
				m.Content = append(m.Content, key, list(item))
			case m.Content[at+1].Kind == yaml.SequenceNode:
				m.Content[at+1].Content = append(m.Content[at+1].Content, item)
			case m.Content[at+1].ShortTag() == "!!null":
				m.Content[at+1] = list(item)
			default:
				m.Content[at+1] = list(m.Content[at+1], item)
			}
		case at < 0:
			m.Content = append(m.Content, key, valueNode(e.Value))
		default:
			m.Content[at+1] = valueNode(e.Value)
		}
	}
	if len(m.Content) == 0 {
		return "---\n---\n" + body, nil
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	enc.Close()
	return "---\n" + b.String() + "---\n" + body, nil
}

func list(items ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}
}

// localTime is a day or a local date-time, as the extension writes them (due, completions).
var localTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(T\d{2}:\d{2})?$`)

// valueNode is v as a YAML value; a day or a local time is written plain, as Obsidian writes it (yaml.v3 would
// quote a day, which reads as a timestamp).
func valueNode(v any) *yaml.Node {
	if s, ok := v.(string); ok && localTime.MatchString(s) {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: s}
	}
	var n yaml.Node
	n.Encode(v)
	return &n
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
