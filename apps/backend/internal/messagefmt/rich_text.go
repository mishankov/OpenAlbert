package messagefmt

import (
	"encoding/json"
	"strings"
)

// RichMessageText extracts visible content from Telegram's received RichMessage
// blocks. Formatting metadata and media file identifiers are not model input.
func RichMessageText(data []byte) string {
	var message struct {
		Blocks []any `json:"blocks"`
	}
	if json.Unmarshal(data, &message) != nil {
		return ""
	}
	return strings.TrimSpace(richBlocks(message.Blocks, 0))
}

func richBlocks(blocks []any, depth int) string {
	if depth > 64 {
		return ""
	}
	var parts []string
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := block["type"].(string)
		var text string
		switch kind {
		case "anchor":
			continue
		case "divider":
			text = "---"
		case "list":
			items, _ := block["items"].([]any)
			var lines []string
			for _, value := range items {
				item, ok := value.(map[string]any)
				if !ok {
					continue
				}
				body, _ := item["blocks"].([]any)
				content := richBlocks(body, depth+1)
				if content == "" {
					continue
				}
				label, _ := item["label"].(string)
				if label == "" {
					label = "-"
				}
				if checked, _ := item["has_checkbox"].(bool); checked {
					label = "[ ]"
					if value, _ := item["is_checked"].(bool); value {
						label = "[x]"
					}
				}
				lines = append(lines, label+" "+strings.ReplaceAll(content, "\n", "\n  "))
			}
			text = strings.Join(lines, "\n")
		case "table":
			rows, _ := block["cells"].([]any)
			var lines []string
			for _, value := range rows {
				row, _ := value.([]any)
				cells := make([]string, len(row))
				for i, value := range row {
					cells[i] = richText(value, depth+1)
				}
				lines = append(lines, strings.Join(cells, " | "))
			}
			text = strings.Join(lines, "\n")
		case "buttons":
			buttons, _ := block["buttons"].([]any)
			for _, button := range buttons {
				text = joinRich(text, richText(button, depth+1))
			}
		default:
			// These are visible fields shared by text, quotation, details and
			// media blocks. Unknown block types can expose text without leaking
			// arbitrary metadata into the conversation.
			text = richText(block["summary"], depth+1)
			text = joinRich(text, richText(block["text"], depth+1))
			text = joinRich(text, richText(block["expression"], depth+1))
			children, _ := block["blocks"].([]any)
			text = joinRich(text, richBlocks(children, depth+1))
			if kind == "pre" && text != "" {
				text = "```\n" + text + "\n```"
			}
			if text == "" {
				switch kind {
				case "photo", "video", "audio", "voice_note", "animation", "document", "map":
					text = "[" + kind + "]"
				}
			}
		}
		text = joinRich(text, richText(block["caption"], depth+1))
		text = joinRich(text, richText(block["credit"], depth+1))
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func richText(value any, depth int) string {
	if depth > 64 {
		return ""
	}
	switch value := value.(type) {
	case string:
		return value
	case []any:
		var text strings.Builder
		for _, item := range value {
			text.WriteString(richText(item, depth+1))
		}
		return text.String()
	case map[string]any:
		text := richText(value["text"], depth+1)
		if kind, _ := value["type"].(string); kind == "anchor" {
			return ""
		}
		if text == "" {
			text = richText(value["alternative_text"], depth+1)
		}
		if text == "" {
			text = richText(value["expression"], depth+1)
		}
		if button, ok := value["button"]; ok {
			text = richText(button, depth+1)
		}
		if url, _ := value["url"].(string); url != "" && url != text {
			if text == "" {
				text = url
			} else {
				text += " (" + url + ")"
			}
		}
		return joinRich(text, richText(value["credit"], depth+1))
	}
	return ""
}

func joinRich(left, right string) string {
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	return left + "\n" + right
}
