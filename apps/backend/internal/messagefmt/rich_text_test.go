package messagefmt

import (
	"strings"
	"testing"
)

func TestRichMessageText(t *testing.T) {
	for _, tc := range []struct {
		name, json, want string
	}{
		{"escaped Unicode and inline styles", `{"blocks":[{"type":"paragraph","text":["\u041f\u0440\u0438",{"type":"bold","text":["вет",{"type":"italic","text":", мир"}]},"! \ud83d\ude00"]}]}`, "Привет, мир! 😀"},
		{"paragraph boundaries", `{"blocks":[{"type":"heading","text":"Заголовок","size":1},{"type":"paragraph","text":"Первый"},{"type":"paragraph","text":"Второй"}]}`, "Заголовок\n\nПервый\n\nВторой"},
		{"link and emoji", `{"blocks":[{"type":"paragraph","text":[{"type":"url","text":"Сайт","url":"https://example.com"}," ",{"type":"custom_emoji","alternative_text":"😀","custom_emoji_id":"hidden"}]}]}`, "Сайт (https://example.com) 😀"},
		{"identical link", `{"blocks":[{"text":{"type":"url","text":"https://example.com","url":"https://example.com"}}]}`, "https://example.com"},
		{"list and checkbox", `{"blocks":[{"type":"list","items":[{"label":"1.","blocks":[{"type":"paragraph","text":"Раз"}]},{"has_checkbox":true,"is_checked":true,"blocks":[{"text":"Два"}]}]}]}`, "1. Раз\n[x] Два"},
		{"nested blocks and credit", `{"blocks":[{"type":"details","summary":"Подробнее","blocks":[{"type":"blockquote","blocks":[{"text":"Цитата"}],"credit":"Автор"}]}]}`, "Подробнее\nЦитата\nАвтор"},
		{"table", `{"blocks":[{"type":"table","cells":[[{"text":"A"},{"text":"B"}],[{}, {"text":{"type":"bold","text":"C"}}]],"caption":"Таблица"}]}`, "A | B\n | C\nТаблица"},
		{"code and math", `{"blocks":[{"type":"pre","language":"go","text":"x := 1\n  y := 2"},{"type":"mathematical_expression","expression":"x^2"}]}`, "```\nx := 1\n  y := 2\n```\n\nx^2"},
		{"media caption", `{"blocks":[{"type":"photo","photo":[{"file_id":"hidden"}],"caption":{"text":"Фото","credit":"Автор"}}]}`, "[photo]\nФото\nАвтор"},
		{"unknown block visible fields only", `{"blocks":[{"type":"future","text":"Текст","metadata":"hidden","file_id":"hidden"}]}`, "Текст"},
		{"anchors are invisible", `{"blocks":[{"type":"anchor","name":"hidden"},{"type":"paragraph","text":[{"type":"anchor","name":"hidden"},"Видно"]}]}`, "Видно"},
		{"buttons", `{"blocks":[{"type":"buttons","buttons":[{"text":"Открыть","url":"https://example.com","callback_data":"hidden"}]}]}`, "Открыть (https://example.com)"},
		{"null", `null`, ""},
		{"empty", `{"blocks":[]}`, ""},
		{"wrong shape", `{"blocks":"hidden"}`, ""},
		{"malformed", `{"blocks":[`, ""},
		{"non-block values", `{"blocks":[null,42,"hidden"]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RichMessageText([]byte(tc.json)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRichMessageTextDepthLimit(t *testing.T) {
	text := `"deep"`
	for range 100 {
		text = `{"type":"bold","text":` + text + `}`
	}
	data := `{"blocks":[{"text":` + text + `},{"text":"Visible sibling"}]}`
	if got := RichMessageText([]byte(data)); got != "Visible sibling" || strings.Contains(got, "deep") {
		t.Fatalf("depth limit did not preserve sibling: %q", got)
	}
}
