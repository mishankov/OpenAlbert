package desktopapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
)

func TestStatusReportsOwnerActivityLifecycle(t *testing.T) {
	t.Run("memory", func(t *testing.T) { statusActivityContract(t, storage.NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
		if url == "" {
			t.Skip("set OPENALBERT_TEST_DATABASE_URL to a disposable database")
		}
		repo, err := storage.Open(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		statusActivityContract(t, repo)
	})
}
func statusActivityContract(t *testing.T, repo domain.Repository) {
	t.Helper()
	api := newAPI(t, repo)
	handler := api.Handler()
	ctx := context.Background()
	check := func(state, text string) {
		t.Helper()
		w := call(t, handler, "GET", "/api/v1/status", "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		status := decode[struct {
			Activity domain.ConversationActivity `json:"activity"`
		}](t, w)
		if status.Activity.State != state || status.Activity.Text != text {
			t.Fatalf("activity %+v, want %s %q", status.Activity, state, text)
		}
		if strings.Contains(w.Body.String(), "PRIVATE_TRACE") || strings.Contains(w.Body.String(), "FOREIGN_PROGRESS") {
			t.Fatal("activity leaked nonpublic data", w.Body.String())
		}
		if state == "idle" && strings.Contains(w.Body.String(), `"text"`) {
			t.Fatal("idle status retained a stale stage")
		}
	}
	other := domain.Conversation{ID: api.scope() + "-foreign"}
	foreignTurn := api.scope() + "-foreign-turn"
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: foreignTurn + "-incoming", Conversation: other, Text: "PRIVATE_TRACE"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := repo.StartProgress(ctx, domain.Progress{TurnID: foreignTurn, Conversation: other, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReportProgress(ctx, foreignTurn, 1, "FOREIGN_PROGRESS"); err != nil {
		t.Fatal(err)
	}
	check("idle", "")
	for _, terminal := range []string{"waiting_delivery", "failed", "cancelled"} {
		uuid := id()
		body := fmt.Sprintf(`{"client_message_id":%q,"text":"hello"}`, uuid)
		for i := 0; i < 2; i++ {
			w := call(t, handler, "POST", "/api/v1/messages", body)
			if w.Code != 202 {
				t.Fatal(w.Code, w.Body.String())
			}
		}
		check("queued", "Обрабатываю сообщение…")
		conversation, ok, err := repo.Conversation(ctx, api.scope())
		if err != nil || !ok {
			t.Fatal("conversation missing", err)
		}
		turn := api.scope() + "-" + terminal
		if err = repo.StartProgress(ctx, domain.Progress{TurnID: turn, Conversation: conversation, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err = repo.ReportProgress(ctx, turn, 1, "Ищу подходящие варианты…"); err != nil {
			t.Fatal(err)
		}
		check("processing", "Ищу подходящие варианты…")
		if err = repo.MarkIncomingHandled(ctx, "desktop:"+uuid); err != nil {
			t.Fatal(err)
		}
		if err = repo.EndProgress(ctx, turn, terminal); err != nil {
			t.Fatal(err)
		}
		check("idle", "")
	}
}

type failedActivityRepository struct{ domain.Repository }

func (failedActivityRepository) ConversationActivity(context.Context, string) (domain.ConversationActivity, error) {
	return domain.ConversationActivity{}, errors.New("PRIVATE_TRACE postgres secret")
}
func TestStatusDoesNotPretendIdleWhenActivityStorageFails(t *testing.T) {
	api := newAPI(t, failedActivityRepository{Repository: storage.NewMemory()})
	w := call(t, api.Handler(), "GET", "/api/v1/status", "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"unavailable"`) || strings.Contains(w.Body.String(), "PRIVATE_TRACE") {
		t.Fatal("storage failure misrepresented or leaked", w.Code, w.Body.String())
	}
}
