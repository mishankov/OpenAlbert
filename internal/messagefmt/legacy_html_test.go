package messagefmt

import (
	"strings"
	"testing"

	"jarvis/internal/domain"
)

func TestLegacyRichHTMLPreservesLiteralCodeUnicodeAndOverlappingStyles(t *testing.T) {
	m := Message{Text: "😀 **literal** <tag>", Entities: []domain.TextEntity{{Type: "bold", Offset: 0, Length: 2}, {Type: "pre", Offset: 3, Length: 11}}}
	want := "<b>😀</b> <pre>**literal**</pre> &lt;tag&gt;"
	if got := LegacyHTML(m); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	m = Message{Text: "abcd", Entities: []domain.TextEntity{{Type: "bold", Offset: 0, Length: 3}, {Type: "italic", Offset: 1, Length: 3}}}
	if got := LegacyHTML(m); got != "<b>a<i>bc</i></b><i>d</i>" {
		t.Fatalf("crossing spans were not balanced: %s", got)
	}
	for _, m := range []Message{Markdown("**Hello _world_**"), Markdown("```go\nx < y\n```"), Markdown("[link](https://example.com/?a=1&b=2)")} {
		if got := LegacyHTML(m); got == "" || strings.Contains(got, "< y") {
			t.Fatalf("unsafe or missing HTML: %q", got)
		}
	}
}
