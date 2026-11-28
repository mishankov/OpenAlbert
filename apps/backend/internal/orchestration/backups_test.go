package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mishankov/OpenAlbert/shared/backup"
	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
)

func TestBackupEventRunsWithoutUserMessageAndQueuesFailureNotice(t *testing.T) {
	a, repo, _, owner := newFixture(t)
	a.OwnerUserID = owner.OwnerID
	dir := t.TempDir()
	a.Backups = &backup.Reader{Dir: dir}
	now := time.Now().UTC()
	if err := backup.AtomicJSON(filepath.Join(dir, "status.json"), backup.State{Version: 1, Enabled: true, Phase: "failed", ErrorCode: "upload_failed", UpdatedAt: now, HeartbeatAt: now}); err != nil {
		t.Fatal(err)
	}
	a.Model = &scriptedModel{run: func(_ int, messages []domain.Message) (domain.Message, error) {
		found := false
		for _, m := range messages {
			found = found || strings.Contains(m.Content, "Host backup event")
		}
		if !found || !strings.Contains(messages[0].Content, "upload_failed") {
			t.Fatal("event or live state absent")
		}
		return reply("Не удалось загрузить резервную копию в S3."), nil
	}}
	r := &Runtime{Repo: repo, Activities: a, MaxSteps: 3, Clock: NewFakeClock(now)}
	if err := r.RunBackupEvent(context.Background(), BackupEventInput{ConversationID: owner.ID, Event: backup.Event{ID: "run-failed", Kind: "failed", At: now}, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	out, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(out) != 1 || out[0].Conversation.ID != owner.ID {
		t.Fatalf("failure notification not queued: %+v %v", out, err)
	}
	offset, _ := repo.PollOffset(context.Background())
	if offset != 1 {
		t.Fatal("backup event changed Telegram offset")
	}
}

func TestBackupDispatchDeduplicatesRestartsAndChoosesOwnerOnly(t *testing.T) {
	a, repo, _, owner := newFixture(t)
	a.OwnerUserID = owner.OwnerID
	dir := t.TempDir()
	a.Backups = &backup.Reader{Dir: dir}
	_, err := repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "other", Conversation: domain.Conversation{ID: "personal:8", Private: true, OwnerID: 8, ChatID: 8}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	e := backup.Event{ID: "stable-failure", Kind: "failed", At: time.Now().UTC()}
	if err := backup.AtomicJSON(filepath.Join(dir, "events", e.ID+".json"), e); err != nil {
		t.Fatal(err)
	}
	var dispatched []BackupEventInput
	hook := &activityHooks{
		dispatchBackup: func(in BackupEventInput) bool {
			dispatched = append(dispatched, in)
			return true
		},
	}
	r := &Runtime{Repo: repo, Activities: a, MaxSteps: 3, hooks: hook}
	if err := r.DispatchBackupEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A new runtime instance represents a process restart; its in-memory state is empty.
	r = &Runtime{Repo: repo, Activities: a, MaxSteps: 3, hooks: hook}
	if err := r.DispatchBackupEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(dispatched) != 1 || dispatched[0].ConversationID != owner.ID || dispatched[0].Event.ID != e.ID {
		t.Fatalf("unexpected dispatched backup events: %+v", dispatched)
	}
	// Claiming dispatch is persisted separately from turn completion and inbox state.
	raw, ok, err := repo.LoadCheckpoint(context.Background(), "backup-dispatch/"+agent.ID(owner.ID, e.ID))
	if err != nil || !ok || string(raw) != string(json.RawMessage(`true`)) {
		t.Fatal("missing durable dispatch marker")
	}
}

func TestBackupFailureAlertSurvivesModelOutageAndIsIdempotent(t *testing.T) {
	a, repo, _, owner := newFixture(t)
	a.OwnerUserID = owner.OwnerID
	dir := t.TempDir()
	a.Backups = &backup.Reader{Dir: dir}
	now := time.Now().UTC()
	if err := backup.AtomicJSON(filepath.Join(dir, "status.json"), backup.State{Version: 1, Enabled: true, Phase: "failed", UpdatedAt: now, HeartbeatAt: now}); err != nil {
		t.Fatal(err)
	}
	a.Model = &scriptedModel{run: func(int, []domain.Message) (domain.Message, error) {
		return domain.Message{}, errors.New("model unavailable")
	}}
	event := backup.Event{ID: "outage", Kind: "failed", At: now}
	clk := NewFakeClock(now)
	r := &Runtime{Repo: repo, Activities: a, MaxSteps: 3, Clock: clk}
	var runErr error
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		runErr = r.RunBackupEvent(context.Background(), BackupEventInput{ConversationID: owner.ID, Event: event, MaxSteps: 3})
	}()
	waitWithFakeClock(t, r, clk)
	if runErr != nil {
		t.Fatal(runErr)
	}
	for i := 0; i < 2; i++ {
		if err := a.BackupAlert(context.Background(), agent.BackupAlertInput{ConversationID: owner.ID, Event: event}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(out) != 1 || !strings.Contains(out[0].Text, "резервное копирование") {
		t.Fatalf("model outage lost or duplicated alert: %+v %v", out, err)
	}
}
