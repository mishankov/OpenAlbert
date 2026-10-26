// Command testserver runs the real desktop HTTP handlers against isolated test
// data. It is used only by tools/checks/check-contract.py, never in deployment.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"time"

	"openalbert/apps/backend/internal/desktopapi"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
)

const token = "contract-test-only-not-a-real-owner-token"

type memory struct{}

func (memory) ListMemory(context.Context, string) ([]domain.MemoryEntry, error) {
	return []domain.MemoryEntry{{ID: "preferences.md", Content: "Предпочитает macOS.", UpdatedAt: time.Now().UTC()}}, nil
}

func main() {
	ctx := context.Background()
	repo := storage.NewMemory()
	conv := domain.Conversation{ID: "personal:123", OwnerID: 123, ChatID: 123, Private: true}
	now := time.Now().UTC()
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "telegram:fixture", Conversation: conv, Text: "Начало разговора", CreatedAt: now}, 1); err != nil {
		log.Fatal(err)
	}
	conv.DeliveryChannel = "desktop"
	if err := repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: "fixture-answer", Conversation: conv, Text: "## Снимок\n\n**Готово**.\n\n```go\nfmt.Println(42)\n```", EnqueuedAt: now.Add(time.Millisecond)}); err != nil {
		log.Fatal(err)
	}
	if _, err := repo.CreateTask(ctx, domain.Task{ID: "task-fixture", ConversationID: conv.ID, Title: "Приложение macOS", Goal: "Проверить API", Status: "waiting", CreatedAt: now, UpdatedAt: now}); err != nil {
		log.Fatal(err)
	}
	if err := repo.SaveArtifact(ctx, domain.Artifact{ID: "folder/result #1?.txt", ConversationID: conv.ID, Name: "legacy.txt", MIME: "text/plain", Data: []byte("Opaque artifact ID"), CreatedAt: now}); err != nil {
		log.Fatal(err)
	}
	// A different scope must never appear in the public owner's API.
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "telegram:foreign", Conversation: domain.Conversation{ID: "personal:456", OwnerID: 456, Private: true}, Text: "FOREIGN_SECRET", CreatedAt: now}, 2); err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	api := &desktopapi.API{Repo: repo, Memory: memory{}, OwnerID: 123, Token: token}
	if err = json.NewEncoder(log.Writer()).Encode(map[string]string{"url": "http://" + listener.Addr().String()}); err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.Serve(listener))
}
