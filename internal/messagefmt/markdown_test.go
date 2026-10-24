package messagefmt

import (
	"openalbert/internal/domain"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func entityText(text string, e domain.TextEntity) string {
	u := utf16.Encode([]rune(text))
	return string(utf16.Decode(u[e.Offset : e.Offset+e.Length]))
}
func TestMarkdownScreenshot(t *testing.T) {
	m := Markdown("Вот содержимое:\n\n**SOUL.md**\n```\n# OpenAlbert\nТы — внимательный помощник.\n```\n\n**USER.md**\n```md\n# User profile\n- Денис живёт в Москве.\n```\n\nПара замечаний: `USER.md` обновляется.")
	if strings.Contains(m.Text, "```") || strings.Contains(m.Text, "**") {
		t.Fatalf("raw Markdown: %q", m.Text)
	}
	var bold, pre, code []string
	for _, e := range m.Entities {
		switch e.Type {
		case "bold":
			bold = append(bold, entityText(m.Text, e))
		case "pre":
			pre = append(pre, entityText(m.Text, e))
		case "code":
			code = append(code, entityText(m.Text, e))
		}
	}
	if !reflect.DeepEqual(bold, []string{"SOUL.md", "USER.md"}) || len(pre) != 2 || !strings.HasPrefix(pre[0], "# OpenAlbert\n") || !strings.Contains(pre[1], "- Денис живёт в Москве.") || !reflect.DeepEqual(code, []string{"USER.md"}) {
		t.Fatalf("missing formatting: %+v", m)
	}
}
func TestMarkdownUnicodeLinksAndLiteralCharacters(t *testing.T) {
	m := Markdown("😀 **жирный _курсив_** и [ссылка](https://example.com/a_b?q=1&x=2)\n\n`<b>**код**</b>` &amp; \\*текст\\* <tag>")
	if !strings.Contains(m.Text, "<b>**код**</b> & *текст* <tag>") {
		t.Fatalf("literal content lost: %q", m.Text)
	}
	found := map[string]bool{}
	for _, e := range m.Entities {
		s := entityText(m.Text, e)
		found[e.Type] = true
		switch e.Type {
		case "bold":
			if e.Offset != 3 || s != "жирный курсив" {
				t.Fatalf("UTF-16 bold range: %+v %q", e, s)
			}
		case "text_link":
			if e.URL != "https://example.com/a_b?q=1&x=2" || s != "ссылка" {
				t.Fatalf("bad link: %+v %q", e, s)
			}
		}
	}
	for _, kind := range []string{"bold", "italic", "text_link", "code"} {
		if !found[kind] {
			t.Fatalf("missing %s", kind)
		}
	}
	for _, input := range []string{"ordinary_a_b and x.y!", "**незакрытый", "[ссылка](javascript:alert)", "<script>alert(1)</script>", "\\&amp;"} {
		rendered := Markdown(input)
		if rendered.Text == "" {
			t.Fatalf("lost text %q", input)
		}
		for _, e := range rendered.Entities {
			if strings.HasPrefix(e.URL, "javascript:") {
				t.Fatal("unsafe link entity")
			}
		}
	}
	if m := Markdown("\\&amp;"); m.Text != "&amp;" {
		t.Fatalf("escaped entity decoded twice: %q", m.Text)
	}
}
func TestMarkdownSplitsLongCodeWithoutLosingFormatting(t *testing.T) {
	code := strings.Repeat("😀 fmt.Println(\"**literal** <tag>\")\n", 250)
	rendered := Markdown("**Начало**\n\n```go\n" + code + "```\n\n_Конец_")
	parts := Split(rendered, 4000)
	if len(parts) < 3 {
		t.Fatalf("expected chunks, got %d", len(parts))
	}
	var joined strings.Builder
	preChunks := 0
	for _, part := range parts {
		joined.WriteString(part.Text)
		if !utf8.ValidString(part.Text) || len(utf16.Encode([]rune(part.Text))) > 4000 {
			t.Fatal("invalid/oversized chunk")
		}
		for _, e := range part.Entities {
			s := entityText(part.Text, e)
			if strings.ContainsRune(s, utf8.RuneError) {
				t.Fatal("entity split surrogate pair")
			}
			if e.Type == "pre" {
				preChunks++
				if e.Language != "go" {
					t.Fatal("lost code language")
				}
			}
		}
	}
	if joined.String() != rendered.Text || preChunks < 3 || !strings.Contains(joined.String(), code) {
		t.Fatal("chunking lost code or formatting")
	}
}
func TestMarkdownHeadingsListsAndCodeInsideEmphasis(t *testing.T) {
	m := Markdown("## Заголовок\n\n3. первый\n4. второй\n\n- пункт\n\n**слово `код` ещё**\n\n~~старое~~")
	if !strings.Contains(m.Text, "3. первый\n4. второй") || !strings.Contains(m.Text, "• пункт") {
		t.Fatalf("lists lost: %q", m.Text)
	}
	for _, a := range m.Entities {
		for _, b := range m.Entities {
			if a.Type == "bold" && b.Type == "code" && a.Offset < b.Offset+b.Length && b.Offset < a.Offset+a.Length {
				t.Fatal("Telegram cannot nest code in bold")
			}
		}
	}
}
