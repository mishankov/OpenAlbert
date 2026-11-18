package desktopapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/integrations/siwc"
	"openalbert/apps/backend/internal/storage"
)

type planControls struct {
	*siwc.Client
	starts, finishes int
	chosen           string
}

func (p *planControls) Status() siwc.Status {
	return siwc.Status{Connected: true, PlanEnabled: true, Model: "account-model"}
}
func (p *planControls) Begin() (string, error) {
	p.starts++
	return "https://auth.openai.com/api/accounts/authorize?state=synthetic", nil
}
func (p *planControls) Finish(context.Context, string) error { p.finishes++; return nil }
func (p *planControls) Models(context.Context) ([]siwc.Model, error) {
	return []siwc.Model{{Slug: "account-model", DisplayName: "Account model"}}, nil
}
func (p *planControls) SetModel(_ context.Context, s string) error { p.chosen = s; return nil }
func TestChatGPTControlsAreAuthenticatedAndDoNotPersistCallbacks(t *testing.T) {
	repo := storage.NewMemory()
	a := newAPI(t, repo)
	plan := &planControls{}
	a.Billing = &integrations.BillingModel{Mode: "auto", Plan: plan}
	h := a.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/chatgpt/sign-in", nil))
	if w.Code != 401 || plan.starts != 0 {
		t.Fatal("anonymous sign-in")
	}
	if w = call(t, h, "POST", "/api/v1/chatgpt/sign-in", ""); w.Code != 200 || plan.starts != 1 {
		t.Fatal(w.Body.String())
	}
	if w = call(t, h, "POST", "/api/v1/chatgpt/callback", `{"url":"http://127.0.0.1:1455/auth/callback?code=secret-code&state=synthetic"}`); w.Code != 200 || plan.finishes != 1 || strings.Contains(w.Body.String(), "secret-code") {
		t.Fatal(w.Body.String())
	}
	pending, err := repo.PendingIncoming(context.Background(), 100)
	if err != nil || len(pending) != 0 {
		t.Fatal("callback entered model input")
	}
	if w = call(t, h, "GET", "/api/v1/chatgpt/models", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Account model") {
		t.Fatal(w.Body.String())
	}
	if w = call(t, h, "PUT", "/api/v1/chatgpt/model", `{"slug":"account-model"}`); w.Code != http.StatusOK || plan.chosen != "account-model" {
		t.Fatal(w.Body.String())
	}
	a.Billing = &integrations.BillingModel{Mode: "api_key"}
	if w = call(t, h, "POST", "/api/v1/chatgpt/sign-in", ""); w.Code != 409 {
		t.Fatal("plan configured unexpectedly")
	}
}
