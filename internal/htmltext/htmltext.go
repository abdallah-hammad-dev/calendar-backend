// Package htmltext turns the HTML of an event's description (Google's, sometimes Outlook's) into the plain text
// iCalendar holds (spec §5.2), keeping each link's target: a meeting link in the notes must still be found.
package htmltext

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var looksLikeHTML = regexp.MustCompile(`<(?:[a-zA-Z][a-zA-Z0-9]*|/[a-zA-Z]|!--)[^>]*>|&(?:[a-zA-Z]+|#[0-9]+|#x[0-9a-fA-F]+);`)

// IsHTML tells whether s is HTML rather than plain text: Google gives either, depending on where the event was made.
func IsHTML(s string) bool { return looksLikeHTML.MatchString(s) }

// Plain is s as plain text: converted when it is HTML, as is otherwise.
func Plain(s string) string {
	if IsHTML(s) {
		return Text(s)
	}
	return strings.TrimSpace(s)
}

// blocks start and end on their own line.
var blocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Ul: true, atom.Ol: true, atom.Table: true, atom.Tr: true, atom.Blockquote: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true, atom.Pre: true,
	atom.Hr: true, atom.Body: true,
}

// hidden elements' contents are left out.
var hidden = map[atom.Atom]bool{atom.Head: true, atom.Title: true, atom.Script: true, atom.Style: true}

// Text converts HTML to plain text: whitespace collapsed as a browser does, a line per block and <br>, "- " before
// list items, and each link's target after its text when the text doesn't already show it.
func Text(s string) string {
	var out strings.Builder
	z := html.NewTokenizer(strings.NewReader(s))
	skip, pre := 0, 0
	space := false // a collapsed space is waiting to be written
	// A paragraph holding only a no-break space is Outlook's blank line.
	paraStart, paraBlank := -1, false
	type link struct {
		href  string
		start int // where its text starts in out
	}
	var links []link

	newline := func() {
		space = false
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteString("\n")
		}
	}
	write := func(text string) {
		if pre > 0 {
			out.WriteString(text)
			return
		}
		for _, r := range text {
			if r == '\u00a0' && paraStart == out.Len() {
				paraBlank = true
			}
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == ' ' {
				space = true
				continue
			}
			if space && out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
				out.WriteByte(' ')
			}
			space = false
			out.WriteRune(r)
		}
	}

	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return tidy(out.String())
		case html.TextToken:
			if skip == 0 {
				write(string(z.Text()))
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			name, hasAttr := z.TagName()
			a := atom.Lookup(name)
			end := tt == html.EndTagToken
			if hidden[a] {
				if tt == html.StartTagToken {
					skip++
				} else if end && skip > 0 {
					skip--
				}
				continue
			}
			if skip > 0 {
				continue
			}
			switch {
			case a == atom.Br:
				space = false
				out.WriteString("\n")
			case a == atom.Li && !end:
				newline()
				out.WriteString("- ")
			case a == atom.Td || a == atom.Th:
				if !end {
					space = true
				}
			case a == atom.A && !end:
				href := ""
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					if string(k) == "href" {
						href = strings.TrimSpace(string(v))
					}
				}
				links = append(links, link{href: href, start: out.Len()})
			case a == atom.A && end && len(links) > 0:
				l := links[len(links)-1]
				links = links[:len(links)-1]
				text := out.String()[l.start:]
				if shown := linkTarget(l.href); shown != "" && !strings.Contains(text, shown) {
					if strings.TrimSpace(text) == "" {
						write(shown)
					} else {
						out.WriteString(" (" + shown + ")")
					}
				}
			case blocks[a]:
				if a == atom.P && end && paraBlank && paraStart == out.Len() {
					out.WriteString("\n")
				}
				if a == atom.Pre {
					if end && pre > 0 {
						pre--
					} else if !end {
						pre++
					}
				}
				newline()
				paraStart, paraBlank = -1, false
				if a == atom.P && !end {
					paraStart = out.Len()
				}
			}
		}
	}
}

// linkTarget is what a link's target reads as in text: web, mail and phone links ("mailto:" dropped), nothing for
// anchors and scripts.
func linkTarget(href string) string {
	lower := strings.ToLower(href)
	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "tel:"):
		return href
	case strings.HasPrefix(lower, "mailto:"):
		return href[len("mailto:"):]
	}
	return ""
}

var blankLines = regexp.MustCompile(`\n{3,}`)

// tidy drops spaces at line ends and runs of blank lines.
func tidy(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
