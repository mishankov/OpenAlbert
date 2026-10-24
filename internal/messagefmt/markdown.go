// Package messagefmt renders Markdown into Telegram text and UTF-16 entities.
// Rendering happens before chunking so long code blocks keep their formatting.
package messagefmt

import (
	"bufio"
	"bytes"
	"html"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"openalbert/internal/domain"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

type Message struct {
	Text     string
	Entities []domain.TextEntity
}
type renderer struct {
	source   []byte
	b        strings.Builder
	units    int
	entities []domain.TextEntity
}

func (r *renderer) write(s string) {
	r.b.WriteString(s)
	for _, c := range s {
		r.units += utf16.RuneLen(c)
	}
}
func (r *renderer) children(n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		r.node(c)
	}
}
func decode(s []byte) string {
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	gmhtml.DefaultWriter.Write(w, s)
	_ = w.Flush()
	return html.UnescapeString(b.String())
}
func (r *renderer) gap(n int) {
	s := r.b.String()
	if s == "" {
		return
	}
	trailing := len(s) - len(strings.TrimRight(s, "\n"))
	if trailing < n {
		r.write(strings.Repeat("\n", n-trailing))
	}
}
func (r *renderer) mark(kind string, start int, extra string) {
	// Telegram does not allow text styling or links to overlap code/pre entities.
	spans := [][2]int{{start, r.units}}
	for _, e := range r.entities {
		if e.Type != "code" && e.Type != "pre" {
			continue
		}
		var next [][2]int
		for _, s := range spans {
			if e.Offset >= s[1] || e.Offset+e.Length <= s[0] {
				next = append(next, s)
				continue
			}
			if s[0] < e.Offset {
				next = append(next, [2]int{s[0], e.Offset})
			}
			if e.Offset+e.Length < s[1] {
				next = append(next, [2]int{e.Offset + e.Length, s[1]})
			}
		}
		spans = next
	}
	for _, s := range spans {
		if s[1] <= s[0] {
			continue
		}
		e := domain.TextEntity{Type: kind, Offset: s[0], Length: s[1] - s[0]}
		if kind == "pre" {
			e.Language = extra
		}
		if kind == "text_link" {
			e.URL = extra
		}
		r.entities = append(r.entities, e)
	}
}
func linkURL(raw []byte) string {
	s := decode(raw)
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto", "tg":
		return s
	}
	return ""
}
func (r *renderer) node(node ast.Node) {
	switch n := node.(type) {
	case *ast.Text:
		if n.IsRaw() {
			r.write(string(n.Segment.Value(r.source)))
		} else {
			r.write(decode(n.Segment.Value(r.source)))
		}
		if n.SoftLineBreak() || n.HardLineBreak() {
			r.write("\n")
		}
	case *ast.String:
		if n.IsCode() || n.IsRaw() {
			r.write(string(n.Value))
		} else {
			r.write(decode(n.Value))
		}
	case *ast.Emphasis:
		start := r.units
		r.children(n)
		kind := "italic"
		if n.Level == 2 {
			kind = "bold"
		}
		r.mark(kind, start, "")
	case *extast.Strikethrough:
		start := r.units
		r.children(n)
		r.mark("strikethrough", start, "")
	case *ast.CodeSpan:
		start := r.units
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			s := c.(*ast.Text).Segment
			r.write(strings.ReplaceAll(string(s.Value(r.source)), "\n", " "))
		}
		r.mark("code", start, "")
	case *ast.FencedCodeBlock:
		r.gap(2)
		start := r.units
		for i := 0; i < n.Lines().Len(); i++ {
			s := n.Lines().At(i)
			r.write(string(s.Value(r.source)))
		}
		r.mark("pre", start, string(n.Language(r.source)))
		r.gap(2)
	case *ast.CodeBlock:
		r.gap(2)
		start := r.units
		for i := 0; i < n.Lines().Len(); i++ {
			s := n.Lines().At(i)
			r.write(string(s.Value(r.source)))
		}
		r.mark("pre", start, "")
		r.gap(2)
	case *ast.Heading:
		r.gap(2)
		start := r.units
		r.children(n)
		r.mark("bold", start, "")
		r.gap(2)
	case *ast.Paragraph:
		r.children(n)
		if _, ok := n.Parent().(*ast.ListItem); ok {
			r.gap(1)
		} else {
			r.gap(2)
		}
	case *ast.List:
		r.gap(1)
		r.children(n)
		r.gap(2)
	case *ast.ListItem:
		r.gap(1)
		depth := 0
		for p := n.Parent().Parent(); p != nil; p = p.Parent() {
			if _, ok := p.(*ast.List); ok {
				depth++
			}
		}
		r.write(strings.Repeat("  ", depth))
		list := n.Parent().(*ast.List)
		if list.IsOrdered() {
			index := list.Start
			for p := n.PreviousSibling(); p != nil; p = p.PreviousSibling() {
				index++
			}
			r.write(strconv.Itoa(index) + ". ")
		} else {
			r.write("• ")
		}
		r.children(n)
		r.gap(1)
	case *ast.Blockquote:
		r.gap(2)
		r.write("│ ")
		r.children(n)
		r.gap(2)
	case *ast.Link:
		start := r.units
		r.children(n)
		if dest := linkURL(n.Destination); dest != "" {
			r.mark("text_link", start, dest)
		}
	case *ast.AutoLink:
		start := r.units
		r.write(string(n.Label(r.source)))
		dest := n.URL(r.source)
		if n.AutoLinkType == ast.AutoLinkEmail && !bytes.HasPrefix(dest, []byte("mailto:")) {
			dest = append([]byte("mailto:"), dest...)
		}
		if u := linkURL(dest); u != "" {
			r.mark("text_link", start, u)
		}
	case *ast.Image:
		r.children(n)
		if dest := linkURL(n.Destination); dest != "" {
			r.write(" (" + dest + ")")
		}
	case *ast.RawHTML:
		for i := 0; i < n.Segments.Len(); i++ {
			s := n.Segments.At(i)
			r.write(string(s.Value(r.source)))
		}
	case *ast.HTMLBlock:
		for i := 0; i < n.Lines().Len(); i++ {
			s := n.Lines().At(i)
			r.write(string(s.Value(r.source)))
		}
		if n.HasClosure() {
			r.write(string(n.ClosureLine.Value(r.source)))
		}
		r.gap(2)
	case *ast.ThematicBreak:
		r.gap(2)
		r.write("────────")
		r.gap(2)
	default:
		r.children(node)
	}
}
func Markdown(source string) Message {
	r := renderer{source: []byte(source)}
	md := goldmark.New(goldmark.WithExtensions(extension.Strikethrough))
	r.node(md.Parser().Parse(text.NewReader(r.source)))
	rendered := strings.TrimRight(r.b.String(), "\n")
	if rendered == "" {
		return Message{Text: source}
	}
	units := len(utf16.Encode([]rune(rendered)))
	var entities []domain.TextEntity
	for _, e := range r.entities {
		e.Length = min(e.Length, units-e.Offset)
		if e.Length > 0 {
			entities = append(entities, e)
		}
	}
	sort.SliceStable(entities, func(i, j int) bool {
		if entities[i].Offset == entities[j].Offset {
			return entities[i].Length > entities[j].Length
		}
		return entities[i].Offset < entities[j].Offset
	})
	// A bold Markdown heading may itself contain bold text. Telegram needs only
	// the enclosing span, not redundant nested entities of the same type.
	unique := make([]domain.TextEntity, 0, len(entities))
	for _, e := range entities {
		redundant := false
		for _, parent := range unique {
			if parent.Type == e.Type && parent.URL == e.URL && parent.Offset <= e.Offset && parent.Offset+parent.Length >= e.Offset+e.Length {
				redundant = true
				break
			}
		}
		if !redundant {
			unique = append(unique, e)
		}
	}
	return Message{Text: rendered, Entities: unique}
}

// Split preserves all text, clips spans to each chunk and never splits a surrogate pair.
func Split(m Message, limit int) []Message {
	if limit < 2 {
		panic("message limit must allow a UTF-16 surrogate pair")
	}
	var out []Message
	for offset := 0; len(m.Text) > 0; {
		end, units := len(m.Text), 0
		for i, c := range m.Text {
			size := utf16.RuneLen(c)
			if units+size > limit {
				end = i
				break
			}
			units += size
		}
		part := Message{Text: m.Text[:end]}
		for _, e := range m.Entities {
			start, stop := max(e.Offset, offset), min(e.Offset+e.Length, offset+units)
			if stop > start {
				e.Offset = start - offset
				e.Length = stop - start
				part.Entities = append(part.Entities, e)
			}
		}
		out = append(out, part)
		m.Text = m.Text[end:]
		offset += units
	}
	return out
}
