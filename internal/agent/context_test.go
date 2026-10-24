package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"openalbert/internal/domain"
)

func TestCompactContextBoundsPayloadAndPreservesWholeToolExchanges(t *testing.T) {
	original := []domain.Message{{Role: "system", Content: "System rules"}, {Role: "user", Content: "Latest user request must remain"}}
	for i := 0; i < 24; i++ {
		first, second := fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", i)
		original = append(original, domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: first, Name: "web_read", Arguments: json.RawMessage(`{"url":"https://example.com/a"}`)}, {ID: second, Name: "web_read", Arguments: json.RawMessage(`{"url":"https://example.com/b"}`)}}}, domain.Message{Role: "tool", ToolCallID: first, Content: strings.Repeat("A", 35000)}, domain.Message{Role: "tool", ToolCallID: second, Content: strings.Repeat("B", 35000)})
	}
	compact := CompactContext(original)
	raw, err := json.Marshal(compact)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 750000 {
		t.Fatalf("context still exceeds Temporal budget:%d", len(raw))
	}
	if len(compact) >= len(original) {
		t.Fatal("oversized history not compacted")
	}
	if compact[0].Content != "System rules" || compact[1].Content != "Latest user request must remain" {
		t.Fatal("system/latest request lost")
	}
	pending := map[string]bool{}
	for _, m := range compact {
		if m.Role != "tool" && len(pending) > 0 {
			t.Fatal("tool group was split")
		}
		for _, c := range m.ToolCalls {
			pending[c.ID] = true
		}
		if m.Role == "tool" {
			if !pending[m.ToolCallID] {
				t.Fatal("orphan result after compaction")
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) > 0 {
		t.Fatal("context ends with unfinished exchange")
	}
	if len(original[3].Content) != 35000 {
		t.Fatal("compaction mutated persisted history")
	}
}
func TestCompactContextRetainsNewestUserRequest(t *testing.T) {
	messages := []domain.Message{{Role: "system", Content: "System"}}
	for i := 0; i < 40; i++ {
		messages = append(messages, domain.Message{Role: "user", Content: fmt.Sprintf("Old request%d", i)}, domain.Message{Role: "assistant", Content: strings.Repeat("old", 12000)})
	}
	messages = append(messages, domain.Message{Role: "user", Content: "Newest actual user request"})
	compact := CompactContext(messages)
	if compact[len(compact)-1].Content != "Newest actual user request" {
		t.Fatal("latest user request dropped")
	}
	raw, _ := json.Marshal(compact)
	if len(raw) > 750000 {
		t.Fatalf("context exceeds budget:%d", len(raw))
	}
}
func TestCompactContextBoundsSingleOversizedImageRequest(t *testing.T) {
	messages := []domain.Message{{Role: "system", Content: "Rules"}, {Role: "user", Content: "Explain my image", Images: []string{"data:image/jpeg;base64," + strings.Repeat("a", 800000)}}}
	compact := CompactContext(messages)
	raw, _ := json.Marshal(compact)
	if len(raw) > 750000 {
		t.Fatalf("single user image bypasses context budget:%d", len(raw))
	}
	if len(compact) < 2 || !strings.Contains(compact[1].Content, "Explain my image") {
		t.Fatal("latest user text lost while compacting image")
	}
}
func TestContextSanitizationDropsIncompleteChainBeforeNewUser(t *testing.T) {
	messages := []domain.Message{{Role: "system", Content: "rules"}, {Role: "user", Content: "Old request"}, {Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "first", Name: "web_read"}, {ID: "missing", Name: "web_read"}}}, {Role: "tool", ToolCallID: "first", Content: "Partial result"}, {Role: "user", Content: "Request after interruption"}}
	out := CompactContext(validHistory(messages))
	if len(out) != 3 || out[2].Content != "Request after interruption" {
		t.Fatalf("interrupted history not repaired:%+v", out)
	}
	for _, msg := range out {
		if msg.Role == "tool" || len(msg.ToolCalls) > 0 {
			t.Fatal("incomplete exchange remained")
		}
	}
}
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 100, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func TestArtifactViewRestoresStoredImageForNewSessionWithScopedBounds(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	data := testPNG(t, 1200, 600)
	if err := repo.SaveArtifact(ctx, domain.Artifact{ID: "old-image", ConversationID: conv.ID, Name: "photo.png", MIME: "image/png", Data: data}); err != nil {
		t.Fatal(err)
	}
	if err := repo.StartSession(ctx, domain.Session{ID: "new-session", ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	view, err := a.ArtifactView(ctx, conv.ID, "old-image")
	if err != nil {
		t.Fatal(err)
	}
	if view.Role != "user" || len(view.Images) != 1 || !strings.Contains(view.Content, "untrusted data") {
		t.Fatalf("wrong image data message:%+v", view)
	}
	if !strings.HasPrefix(view.Images[0], "data:image/jpeg;base64,") {
		t.Fatal("image not normalized to JPEG")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(view.Images[0], "data:image/jpeg;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || format != "jpeg" || cfg.Width != 512 || cfg.Height != 256 {
		t.Fatalf("saved image not safely resized:%+v %s %v", cfg, format, err)
	}
	raw, _ := json.Marshal(view)
	if len(raw) > 750000 {
		t.Fatalf("artifact image exceeds context budget:%d", len(raw))
	}
	if _, err := a.ArtifactView(ctx, "another-conversation", "old-image"); err == nil {
		t.Fatal("image leaked across conversations")
	}
	saved, ok, err := repo.Artifact(ctx, conv.ID, "old-image")
	if err != nil || !ok || !bytes.Equal(saved.Data, data) {
		t.Fatal("viewing mutated original artifact")
	}
}
func TestArtifactViewIgnoresTextAndInvalidImageBytes(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	for _, art := range []domain.Artifact{{ID: "text", ConversationID: conv.ID, Name: "note.txt", MIME: "text/plain", Data: []byte("content")}, {ID: "broken", ConversationID: conv.ID, Name: "broken.png", MIME: "image/png", Data: []byte("not a PNG")}} {
		if err := repo.SaveArtifact(ctx, art); err != nil {
			t.Fatal(err)
		}
		view, err := a.ArtifactView(ctx, conv.ID, art.ID)
		if err != nil || len(view.Images) != 0 || view.Role != "" {
			t.Fatalf("non-image/invalid data unexpectedly viewed:%+v %v", view, err)
		}
	}
}
