package vault

import (
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Links as Obsidian writes them: wikilinks [[target#heading|alias]] and markdown links [text](target), each maybe an
// embed (!). RewriteLinks updates them after a move as Obsidian does for a move made in it.

var (
	wikiLink = regexp.MustCompile(`\[\[([^\[\]|#\n]*)(#[^\[\]|\n]*)?(\|[^\[\]\n]*)?\]\]`)
	mdLink   = regexp.MustCompile(`\[([^\]\n]*)\]\((<[^>\n]+>|[^)\s]+)\)`)
	scheme   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	fence    = regexp.MustCompile("(?m)^[ \t]*(```|~~~)")
)

// files is the vault's file list for link resolution.
type files struct {
	all    []string
	byPath map[string]string   // lower-case path → path
	byName map[string][]string // lower-case name (a note's without .md) → paths
}

func newFiles(paths []string) *files {
	f := &files{all: paths, byPath: map[string]string{}, byName: map[string][]string{}}
	for _, p := range paths {
		f.byPath[strings.ToLower(p)] = p
		name := strings.ToLower(path.Base(p))
		f.byName[name] = append(f.byName[name], p)
		if strings.HasSuffix(name, ".md") {
			f.byName[strings.TrimSuffix(name, ".md")] = append(f.byName[strings.TrimSuffix(name, ".md")], p)
		}
	}
	for _, l := range f.byName {
		sort.Slice(l, func(i, j int) bool {
			if len(l[i]) != len(l[j]) {
				return len(l[i]) < len(l[j])
			}
			return l[i] < l[j]
		})
	}
	return f
}

// how a link reached its file: by its path from the vault's root, relative to the note, or by its name.
type how int

const (
	byNone how = iota
	byRoot
	byRelative
	byName
)

// resolve finds the file a link's target names from the note at source, the way Obsidian does: its path from
// the root, then from the note's folder, then by name (the note's own folder first, else the shortest path).
func (f *files) resolve(target, source string) (string, how) {
	t := strings.TrimPrefix(target, "/")
	if t == "" {
		return "", byNone
	}
	for _, c := range []string{t, t + ".md"} {
		if p, ok := f.byPath[strings.ToLower(c)]; ok {
			return p, byRoot
		}
	}
	if dir := path.Dir(source); dir != "." {
		for _, c := range []string{path.Join(dir, t), path.Join(dir, t) + ".md"} {
			if p, ok := f.byPath[strings.ToLower(c)]; ok {
				return p, byRelative
			}
		}
	}
	cands := f.byName[strings.ToLower(path.Base(t))]
	var fit []string
	for _, c := range cands {
		lc := strings.ToLower(c)
		lt := strings.ToLower(t)
		if !strings.Contains(t, "/") || strings.HasSuffix(strings.TrimSuffix(lc, ".md"), "/"+strings.TrimSuffix(lt, ".md")) {
			fit = append(fit, c)
		}
	}
	if len(fit) == 0 {
		return "", byNone
	}
	for _, c := range fit {
		if path.Dir(c) == path.Dir(source) {
			return c, byName
		}
	}
	return fit[0], byName
}

// RewriteLinks returns, for every note in texts (by its path before the moves) whose links change, its new text.
// paths are every file of the vault before the moves; moves maps old paths to new ones.
func RewriteLinks(paths []string, moves map[string]string, texts map[string]string) map[string]string {
	before := newFiles(paths)
	after := make([]string, len(paths))
	for i, p := range paths {
		after[i] = p
		if m, ok := moves[p]; ok {
			after[i] = m
		}
	}
	now := newFiles(after)
	out := map[string]string{}
	for src, text := range texts {
		srcNow := src
		if m, ok := moves[src]; ok {
			srcNow = m
		}
		_, sourceMoved := moves[src]
		changed := false
		next := outsideCode(text, func(part string) string {
			part = wikiLink.ReplaceAllStringFunc(part, func(m string) string {
				g := wikiLink.FindStringSubmatch(m)
				target := strings.TrimSpace(g[1])
				dest, how := before.resolve(target, src)
				if how == byNone {
					return m
				}
				destNow := dest
				if d, ok := moves[dest]; ok {
					destNow = d
				} else if !(sourceMoved && how == byRelative) {
					return m // neither the note it names nor, for a relative link, the note it is in moved
				}
				var nt string
				if !strings.Contains(target, "/") {
					nt = stem(destNow, target)
					if strings.EqualFold(nt, target) {
						nt = target // the same name as written, maybe in another case
					}
					if p, _ := now.resolve(nt, srcNow); p != destNow {
						nt = noExt(destNow, target)
					}
				} else {
					nt = noExt(destNow, target)
				}
				if nt == target {
					return m
				}
				changed = true
				return "[[" + nt + g[2] + g[3] + "]]"
			})
			return mdLink.ReplaceAllStringFunc(part, func(m string) string {
				g := mdLink.FindStringSubmatch(m)
				raw := g[2]
				angle := strings.HasPrefix(raw, "<")
				raw = strings.TrimSuffix(strings.TrimPrefix(raw, "<"), ">")
				if scheme.MatchString(raw) || strings.HasPrefix(raw, "#") {
					return m
				}
				target, frag, _ := strings.Cut(raw, "#")
				dec, err := url.PathUnescape(target)
				if err != nil {
					return m
				}
				dest, how := before.resolve(dec, src)
				if how == byNone || how == byName {
					return m
				}
				destNow := dest
				if d, ok := moves[dest]; ok {
					destNow = d
				} else if !(sourceMoved && how == byRelative) {
					return m
				}
				var nt string
				if how == byRelative {
					nt = relative(path.Dir(srcNow), destNow)
				} else {
					nt = destNow
					if strings.HasPrefix(dec, "/") {
						nt = "/" + nt
					}
				}
				if !strings.HasSuffix(strings.ToLower(dec), ".md") && strings.HasSuffix(strings.ToLower(destNow), ".md") {
					nt = strings.TrimSuffix(nt, path.Ext(nt))
				}
				if !angle {
					nt = strings.ReplaceAll(nt, " ", "%20")
				}
				if frag != "" {
					nt += "#" + frag
				}
				if angle {
					nt = "<" + nt + ">"
				}
				if nt == g[2] {
					return m
				}
				changed = true
				return "[" + g[1] + "](" + nt + ")"
			})
		})
		if changed && next != text {
			out[src] = next
		}
	}
	return out
}

// stem is a file's name as a bare link writes it: a note without .md (unless the link had it), others whole.
func stem(p, like string) string {
	name := path.Base(p)
	if strings.HasSuffix(strings.ToLower(name), ".md") && !strings.HasSuffix(strings.ToLower(like), ".md") {
		return strings.TrimSuffix(name, path.Ext(name))
	}
	return name
}

// noExt is a file's path as a link with a path writes it.
func noExt(p, like string) string {
	if strings.HasSuffix(strings.ToLower(p), ".md") && !strings.HasSuffix(strings.ToLower(like), ".md") {
		return strings.TrimSuffix(p, path.Ext(p))
	}
	return p
}

func relative(fromDir, to string) string {
	if fromDir == "." {
		return to
	}
	a := strings.Split(fromDir, "/")
	b := strings.Split(to, "/")
	i := 0
	for i < len(a) && i < len(b)-1 && a[i] == b[i] {
		i++
	}
	return strings.Repeat("../", len(a)-i) + strings.Join(b[i:], "/")
}

// outsideCode applies fn to the text outside fenced code blocks (links there aren't links).
func outsideCode(text string, fn func(string) string) string {
	var b strings.Builder
	rest := text
	for {
		loc := fence.FindStringIndex(rest)
		if loc == nil {
			b.WriteString(fn(rest))
			return b.String()
		}
		b.WriteString(fn(rest[:loc[0]]))
		marker := strings.TrimSpace(rest[loc[0]:loc[1]])
		after := rest[loc[1]:]
		closing := regexp.MustCompile("(?m)^[ \t]*" + regexp.QuoteMeta(marker))
		nl := strings.IndexByte(after, '\n')
		if nl < 0 {
			b.WriteString(rest[loc[0]:])
			return b.String()
		}
		end := closing.FindStringIndex(after[nl:])
		if end == nil {
			b.WriteString(rest[loc[0]:])
			return b.String()
		}
		stop := loc[1] + nl + end[1]
		b.WriteString(rest[loc[0]:stop])
		rest = rest[stop:]
	}
}
