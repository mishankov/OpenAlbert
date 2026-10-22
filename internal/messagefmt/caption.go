package messagefmt

import (
	"unicode/utf16"
	"unicode/utf8"
)

// FitsCaption checks rendered text, not Markdown source. Use UTF-16 units as a
// conservative limit consistent with Telegram entity offsets and text splitting.
func FitsCaption(m Message) bool {
	if !utf8.ValidString(m.Text) {
		return false
	}
	units := 0
	for _, r := range m.Text {
		units += utf16.RuneLen(r)
		if units > 1024 {
			return false
		}
	}
	return true
}
