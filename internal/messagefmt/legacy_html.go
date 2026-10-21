package messagefmt

import (
	"html"
	"sort"
	"strings"
	"unicode/utf16"
)

// LegacyHTML preserves the already-rendered text/entities in old durable outbox
// entries while delivering them through Rich Messages. Literal code is never parsed again.
func LegacyHTML(m Message) string {
	byteAt := map[int]int{0: 0}
	units := 0
	for i, r := range m.Text {
		byteAt[units] = i
		units += utf16.RuneLen(r)
	}
	byteAt[units] = len(m.Text)
	type span struct {
		start, end  int
		open, close string
	}
	spans := []span{}
	points := []int{0, units}
	for _, e := range m.Entities {
		end := e.Offset + e.Length
		if _, ok := byteAt[e.Offset]; !ok {
			continue
		}
		if _, ok := byteAt[end]; !ok || e.Length <= 0 {
			continue
		}
		var open, close string
		switch e.Type {
		case "bold":
			open, close = "<b>", "</b>"
		case "italic":
			open, close = "<i>", "</i>"
		case "underline":
			open, close = "<u>", "</u>"
		case "strikethrough":
			open, close = "<s>", "</s>"
		case "code":
			open, close = "<code>", "</code>"
		case "pre":
			open, close = "<pre>", "</pre>"
			if e.Language != "" {
				open, close = `<pre><code class="language-`+html.EscapeString(e.Language)+`">`, "</code></pre>"
			}
		case "text_link":
			if linkURL([]byte(e.URL)) != "" {
				open, close = `<a href="`+html.EscapeString(e.URL)+`">`, "</a>"
			}
		}
		if open != "" {
			spans = append(spans, span{e.Offset, end, open, close})
			points = append(points, e.Offset, end)
		}
	}
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].start == spans[j].start {
			return spans[i].end > spans[j].end
		}
		return spans[i].start < spans[j].start
	})
	sort.Ints(points)
	unique := points[:0]
	for _, p := range points {
		if len(unique) == 0 || unique[len(unique)-1] != p {
			unique = append(unique, p)
		}
	}
	points = unique
	var out strings.Builder
	active := []int{}
	for i, start := range points {
		if i > 0 && start == points[i-1] {
			continue
		}
		next := []int{}
		for index, s := range spans {
			if s.start <= start && start < s.end {
				next = append(next, index)
			}
		}
		common := 0
		for common < len(active) && common < len(next) && active[common] == next[common] {
			common++
		}
		for j := len(active) - 1; j >= common; j-- {
			out.WriteString(spans[active[j]].close)
		}
		for _, index := range next[common:] {
			out.WriteString(spans[index].open)
		}
		active = next
		if i+1 < len(points) {
			out.WriteString(html.EscapeString(m.Text[byteAt[start]:byteAt[points[i+1]]]))
		}
	}
	return out.String()
}
