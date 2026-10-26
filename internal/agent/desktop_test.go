package agent

import (
	"context"
	"encoding/json"
	"openalbert/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestDesktopReplyRoutingAndFailureBeforePreparation(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	desktop := domain.Incoming{ID: "desktop-message", Source: "desktop", Conversation: conv, Text: "hello", CreatedAt: time.Now().UTC()}
	telegram := domain.Incoming{ID: "telegram-message", Conversation: conv, Text: "telegram", CreatedAt: time.Now().UTC()}
	for _, in := range []domain.Incoming{desktop, telegram} {
		if _, err := repo.AcceptIncoming(ctx, in, 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		ids     []string
		channel string
	}{{[]string{desktop.ID}, "desktop"}, {[]string{desktop.ID, telegram.ID}, ""}, {[]string{telegram.ID, desktop.ID}, "desktop"}} {
		routed, err := a.replyConversation(ctx, conv, test.ids)
		if err != nil || routed.DeliveryChannel != test.channel {
			t.Fatalf("route %+v %v", routed, err)
		}
	}
	// prepareTurn can fail in PrepareAudio before Activities.Prepare is called.
	// Workflow Finish still durably records the original IncomingIDs before NotifyFailure.
	input := RunInput{Key: "failed-before-prepare", SessionID: "failed-session", ConversationID: conv.ID, IncomingIDs: []string{desktop.ID}}
	if err := a.Finish(ctx, FinishInput{Turn: Turn{Input: input}, Failed: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.NotifyFailure(ctx, conv.ID, input.Key); err != nil {
		t.Fatal(err)
	}
	if err := a.AcknowledgeStop(ctx, conv.ID, "stop-session", []string{desktop.ID}); err != nil {
		t.Fatal(err)
	}
	out, err := repo.PendingOutgoing(ctx, 100)
	if err != nil || len(out) != 2 {
		t.Fatal(out, err)
	}
	for _, o := range out {
		if o.Conversation.DeliveryChannel != "desktop" {
			t.Fatal("desktop failure/stop routed to Telegram", o)
		}
	}
	canonical, _, _ := repo.Conversation(ctx, conv.ID)
	if canonical.DeliveryChannel != "" {
		t.Fatal("canonical conversation gained a transport route")
	}
}
func TestDesktopPrepareUsesUploadedArtifactAndTaskContext(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	a.Messenger = nil
	art := domain.Artifact{ID: "uploaded", ConversationID: conv.ID, Name: "note.txt", MIME: "text/plain", Data: []byte("stored attachment")}
	if err := repo.SaveArtifact(ctx, art); err != nil {
		t.Fatal(err)
	}
	task, err := repo.CreateTask(ctx, domain.Task{ID: "task", ConversationID: conv.ID, Title: "Тема", Goal: "Цель", Status: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	in := domain.Incoming{ID: "desktop-artifact", Source: "desktop", Conversation: conv, Text: "Продолжай", TaskContextID: task.ID, Attachments: []domain.Attachment{{ArtifactID: art.ID, FileID: "must-not-download-from-telegram", Name: art.Name, MIME: art.MIME}}}
	if _, err = repo.AcceptIncoming(ctx, in, 0); err != nil {
		t.Fatal(err)
	}
	turn, err := a.Prepare(ctx, RunInput{Key: "desktop-prepare", SessionID: "desktop-session", ConversationID: conv.ID, IncomingIDs: []string{in.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Conversation.DeliveryChannel != "desktop" {
		t.Fatal("missing desktop reply route")
	}
	messages, err := repo.Messages(ctx, "desktop-session")
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Content, "Task under discussion: id=task") || !strings.Contains(messages[0].Content, "stored attachment") {
		t.Fatal("missing agent task/attachment context", messages, err)
	}
	// Caption conversion must retain source Markdown for the desktop renderer.
	tool(t, a, turn.Conversation, "desktop-caption", "caption", "send_message", `{"text":"**Результат**","artifact_id":"uploaded"}`)
	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 1 || out[0].Markdown != "**Результат**" {
		t.Fatal("caption Markdown lost", out, err)
	}
	raw, _ := json.Marshal(in)
	if strings.Contains(string(raw), "Task under discussion") {
		t.Fatal("visible inbox was rewritten with model context")
	}
}
