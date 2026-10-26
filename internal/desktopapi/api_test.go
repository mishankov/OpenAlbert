package desktopapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/internal/domain"
	"openalbert/internal/knowledge"
	"openalbert/internal/storage"
)

const testToken = "desktop-test-token-with-at-least-32-bytes"

func call(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func newAPI(t *testing.T, repo domain.Repository) *API {
	t.Helper()
	k, err := knowledge.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &API{Repo: repo, Memory: k, OwnerID: time.Now().UnixNano(), Token: testToken}
}
func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return v
}
func id() string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
}

func TestAuthenticationAndOrigins(t *testing.T) {
	a := newAPI(t, storage.NewMemory())
	h := a.Handler()
	for _, test := range []struct {
		token, origin string
		want          int
	}{{"", "", 401}, {"wrong", "", 401}, {testToken, "https://other.example", 403}, {testToken, "", 200}} {
		req := httptest.NewRequest("GET", "/api/v1/status", nil)
		if test.token != "" {
			req.Header.Set("Authorization", "Bearer "+test.token)
		}
		req.Header.Set("Origin", test.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != test.want {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
	a.Token = ""
	mux := http.NewServeMux()
	a.Register(mux)
	if w := call(t, mux, "GET", "/api/v1/status", ""); w.Code != 404 {
		t.Fatal("disabled API is registered")
	}
}
func TestAPIContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { apiContract(t, storage.NewMemory()) })
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
		apiContract(t, repo)
	})
}
func apiContract(t *testing.T, repo domain.Repository) {
	a := newAPI(t, repo)
	h := a.Handler()
	ctx := context.Background()
	scope := a.scope()
	conv := domain.Conversation{ID: scope, OwnerID: a.OwnerID, ChatID: a.OwnerID, Private: true}
	// Internal model content and other scopes must never be part of this feed.
	_, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: id(), Conversation: domain.Conversation{ID: scope + "-group"}, Text: "private group"}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.StartSession(ctx, domain.Session{ID: scope + "-session", ConversationID: scope}); err != nil {
		t.Fatal(err)
	}
	if err = repo.AppendMessage(ctx, scope+"-session", "secret", domain.Message{Role: "assistant", Content: "internal draft", Reasoning: "secret reasoning"}); err != nil {
		t.Fatal(err)
	}
	firstID := id()
	body := fmt.Sprintf(`{"client_message_id":%q,"text":"Привет","attachment_ids":[]}`, firstID)
	w := call(t, h, "POST", "/api/v1/messages", body)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	first := decode[struct {
		Message Message `json:"message"`
	}](t, w).Message
	if first.ID != "desktop:"+firstID || first.Text != "Привет" || first.Delivery != "accepted" {
		t.Fatalf("accepted message: %+v", first)
	}
	w = call(t, h, "POST", "/api/v1/messages", body)
	retry := decode[struct {
		Message Message `json:"message"`
	}](t, w).Message
	if w.Code != 202 || !first.CreatedAt.Equal(retry.CreatedAt) {
		t.Fatal("retry did not return original acceptance")
	}
	if w = call(t, h, "POST", "/api/v1/messages", strings.Replace(body, "Привет", "Другой текст", 1)); w.Code != 409 {
		t.Fatal("ID reuse accepted", w.Code, w.Body.String())
	}
	offset, _ := repo.PollOffset(ctx)
	if offset < 9 {
		t.Fatal("desktop input rewound Telegram offset")
	}
	// Desktop output is visible while queued; Telegram output only when delivered.
	desktopConv := conv
	desktopConv.DeliveryChannel = "desktop"
	for _, out := range []domain.Outgoing{{ID: scope + "-desktop", Conversation: desktopConv, Text: "**Готово**"}, {ID: scope + "-telegram", Conversation: conv, Text: "Телеграм"}} {
		if err = repo.EnqueueOutgoing(ctx, out); err != nil {
			t.Fatal(err)
		}
	}
	type page struct {
		Messages []Message `json:"messages"`
		HasMore  bool      `json:"has_more"`
		Next     *string   `json:"next_before"`
	}
	w = call(t, h, "GET", "/api/v1/messages?limit=1", "")
	latest := decode[page](t, w)
	if len(latest.Messages) != 1 || latest.Messages[0].Text != "**Готово**" || latest.Messages[0].Format != "markdown" || !latest.HasMore || latest.Next == nil {
		t.Fatalf("latest page: %+v %s", latest, w.Body.String())
	}
	older := decode[page](t, call(t, h, "GET", "/api/v1/messages?before="+*latest.Next, ""))
	if len(older.Messages) != 1 || older.Messages[0].ID != first.ID || older.HasMore {
		t.Fatalf("older page: %+v", older)
	}
	for _, state := range []string{"sending", "sent"} {
		if err = repo.SetOutgoingState(ctx, scope+"-telegram", state); err != nil {
			t.Fatal(err)
		}
	}
	w = call(t, h, "GET", "/api/v1/messages", "")
	all := decode[page](t, w)
	if len(all.Messages) != 3 || strings.Contains(w.Body.String(), "internal draft") || strings.Contains(w.Body.String(), "private group") {
		t.Fatal("public history leaked or omitted data", w.Body.String())
	}
	// Stop is durable/idempotent and does not become a user bubble.
	stopID := id()
	stopBody := fmt.Sprintf(`{"client_message_id":%q}`, stopID)
	for i := 0; i < 2; i++ {
		if w = call(t, h, "POST", "/api/v1/stop", stopBody); w.Code != 202 {
			t.Fatal(w.Body.String())
		}
	}
	all = decode[page](t, call(t, h, "GET", "/api/v1/messages", ""))
	if len(all.Messages) != 3 {
		t.Fatal("stop leaked into history")
	}
	stopped, ok, err := repo.Incoming(ctx, "desktop:"+stopID)
	if err != nil || !ok || stopped.Command != "stop" || stopped.Source != "desktop" {
		t.Fatal("stop not dispatched", stopped, err)
	}
	canonical, ok, err := repo.Conversation(ctx, scope)
	if err != nil || !ok || canonical.DeliveryChannel != "" {
		t.Fatal("ephemeral delivery route persisted")
	}
	// Owner task context is separate from visible text.
	task, err := repo.CreateTask(ctx, domain.Task{ID: scope + "-task", ConversationID: scope, Title: "Проверить", Goal: "Цель", Status: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	taskBody := fmt.Sprintf(`{"client_message_id":%q,"text":"Продолжай","task_id":%q}`, id(), task.ID)
	w = call(t, h, "POST", "/api/v1/messages", taskBody)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	taskMessage := decode[struct {
		Message Message `json:"message"`
	}](t, w).Message
	if taskMessage.Text != "Продолжай" || taskMessage.TaskID == nil || *taskMessage.TaskID != task.ID {
		t.Fatal("task context lost")
	}
	filesContract(t, a)
}
func filesContract(t *testing.T, a *API) {
	t.Helper()
	h := a.Handler()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "заметка.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("hello file"))
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/attachments", &body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	art := decode[struct {
		Attachment Attachment `json:"attachment"`
	}](t, w).Attachment
	if art.SizeBytes != 10 || art.Name != "заметка.txt" {
		t.Fatalf("bad attachment %+v", art)
	}
	w = call(t, h, "GET", "/api/v1/attachments/"+art.ID+"/content", "")
	if w.Code != 200 || w.Body.String() != "hello file" || w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("download failure", w.Code, w.Body.String())
	}
	w = call(t, h, "POST", "/api/v1/messages", fmt.Sprintf(`{"client_message_id":%q,"attachment_ids":[%q]}`, id(), art.ID))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	foreign := domain.Artifact{ID: "foreign-" + id(), ConversationID: "foreign", Name: "secret", Data: []byte("secret")}
	if err = a.Repo.SaveArtifact(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	if w = call(t, h, "GET", "/api/v1/attachments/"+foreign.ID+"/content", ""); w.Code != 404 {
		t.Fatal("cross-scope download")
	}
	if w = call(t, h, "POST", "/api/v1/messages", fmt.Sprintf(`{"client_message_id":%q,"attachment_ids":[%q]}`, id(), foreign.ID)); w.Code != 404 {
		t.Fatal("cross-scope attachment")
	}
	w = call(t, h, "GET", "/api/v1/files", "")
	if strings.Contains(w.Body.String(), foreign.ID) || !strings.Contains(w.Body.String(), art.ID) {
		t.Fatal("files scope failure", w.Body.String())
	}
}
func TestConcurrentIdempotency(t *testing.T) {
	a := newAPI(t, storage.NewMemory())
	h := a.Handler()
	uuid := id()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := call(t, h, "POST", "/api/v1/messages", fmt.Sprintf(`{"client_message_id":%q,"text":"hello"}`, uuid))
			if w.Code != 202 {
				t.Errorf("status %d", w.Code)
			}
		}()
	}
	wg.Wait()
	records, err := a.Repo.PublicTimeline(context.Background(), a.scope(), 0, 100)
	if err != nil || len(records) != 1 {
		t.Fatal("duplicate concurrent turn", len(records), err)
	}
}
func TestInvalidRequests(t *testing.T) {
	a := newAPI(t, storage.NewMemory())
	h := a.Handler()
	for _, test := range []struct {
		method, path, body string
		status             int
	}{{"POST", "/api/v1/messages", `{}`, 400}, {"POST", "/api/v1/messages", fmt.Sprintf(`{"client_message_id":%q,"text":""}`, id()), 400}, {"POST", "/api/v1/messages", fmt.Sprintf(`{"client_message_id":%q,"text":"hello","conversation_id":"other"}`, id()), 400}, {"POST", "/api/v1/messages", `{} {}`, 400}, {"GET", "/api/v1/messages?limit=101", "", 400}, {"GET", "/api/v1/messages?before=bad", "", 400}, {"POST", "/api/v1/messages", fmt.Sprintf(`{"client_message_id":%q,"text":%q}`, id(), strings.Repeat("a", 32001)), 400}} {
		w := call(t, h, test.method, test.path, test.body)
		if w.Code != test.status {
			t.Fatalf("%s %s: %d %s", test.method, test.path, w.Code, w.Body.String())
		}
	}
}

type metadataOnlyRepository struct {
	domain.Repository
	downloads int
	lists     int
}

func (r *metadataOnlyRepository) Artifact(ctx context.Context, scope, id string) (domain.Artifact, bool, error) {
	r.downloads++
	return r.Repository.Artifact(ctx, scope, id)
}
func (r *metadataOnlyRepository) ListArtifacts(ctx context.Context, scope string) ([]domain.Artifact, error) {
	r.lists++
	return r.Repository.ListArtifacts(ctx, scope)
}
func TestTimelineAndAcceptanceNeverDownloadArtifactBodies(t *testing.T) {
	base := storage.NewMemory()
	spy := &metadataOnlyRepository{Repository: base}
	a := newAPI(t, spy)
	ctx := context.Background()
	art := domain.Artifact{ID: "metadata-only", ConversationID: a.scope(), Name: "large.bin", MIME: "application/octet-stream", SizeBytes: MaxAttachmentBytes}
	if err := base.SaveArtifact(ctx, art); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"client_message_id":%q,"text":"file","attachment_ids":["metadata-only"]}`, id())
	w := call(t, a.Handler(), "POST", "/api/v1/messages", body)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	if spy.downloads != 0 {
		t.Fatal("acceptance downloaded attachment bodies")
	}
	spy.lists = 0
	w = call(t, a.Handler(), "GET", "/api/v1/messages", "")
	if w.Code != 200 || spy.downloads != 0 || spy.lists != 1 || !strings.Contains(w.Body.String(), `"size_bytes":20971520`) {
		t.Fatal("timeline did not use metadata-only batch", spy.downloads, spy.lists, w.Body.String())
	}
}
