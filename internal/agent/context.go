package agent

import (
	"encoding/json"
	"openalbert/internal/domain"
)

// CompactContext bounds Temporal Activity payloads while retaining the latest user
// request and complete tool exchanges. Older details remain in persisted history.
func CompactContext(messages []domain.Message) []domain.Message {
	out := append([]domain.Message(nil), messages...)
	imageBudget := 400000
	for i := len(out) - 1; i >= 0; i-- {
		kept := []string{}
		for _, image := range out[i].Images {
			if len(image) <= imageBudget {
				kept = append(kept, image)
				imageBudget -= len(image)
			}
		}
		out[i].Images = kept
	}
	for i := range out {
		out[i].Content = clip(out[i].Content, 32000)
	}
	for len(out) > 2 {
		raw, _ := json.Marshal(out)
		if len(raw) <= 750000 {
			break
		}
		index := 1
		if out[index].Role == "user" {
			anotherUser := false
			for i := index + 1; i < len(out); i++ {
				if out[i].Role == "user" {
					anotherUser = true
					break
				}
			}
			if !anotherUser {
				index++
			}
		}
		if index >= len(out) {
			break
		}
		end := index + 1
		if len(out[index].ToolCalls) > 0 {
			for end < len(out) && out[end].Role == "tool" {
				end++
			}
		}
		out = append(out[:index], out[end:]...)
	}
	return out
}
