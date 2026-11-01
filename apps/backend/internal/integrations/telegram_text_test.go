package integrations

import (
	"encoding/json"
	"testing"
)

func TestTelegramMessageTextPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, text, caption, rich, want string
	}{
		{"text", "Обычный текст", "Подпись", `{"blocks":[{"text":"Rich"}]}`, "Обычный текст"},
		{"caption", "", "Подпись", `{"blocks":[{"text":"Rich"}]}`, "Подпись"},
		{"rich", "", "", `{"blocks":[{"type":"paragraph","text":"\u041f\u0440\u0438\u0432\u0435\u0442"}]}`, "Привет"},
		{"no raw JSON fallback", "", "", `{"blocks":[{"type":"future","metadata":"hidden"}]}`, ""},
		{"no text", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &telegramMessage{Text: tc.text, Caption: tc.caption, RichMessage: json.RawMessage(tc.rich)}
			if got := telegramMessageText(m); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
