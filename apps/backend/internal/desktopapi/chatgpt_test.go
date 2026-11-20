package desktopapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/domain"
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

type desktopControlRoutes struct {
	*siwc.Client
}

func (m *desktopControlRoutes) ProfileRoutes() map[domain.ModelRole]integrations.ProfileRouteStatus {
	return map[domain.ModelRole]integrations.ProfileRouteStatus{
		domain.RoleChat:       {Profile: "subscription_chat", Model: "chat-model", AuthMode: "siwc"},
		domain.RoleBackground: {Profile: "api_background", Model: "background-model", AuthMode: "api_key"},
		domain.RoleAnalysis:   {Profile: "subscription_analysis", Model: "analysis-model", AuthMode: "siwc"},
	}
}

func TestProfileChatGPTControlsRejectGlobalSelectionAndRetainAccountControls(t *testing.T) {
	a := newAPI(t, storage.NewMemory())
	plan := &planControls{}
	a.Billing = &integrations.BillingModel{Mode: "profiles", Plan: plan, API: &desktopControlRoutes{}}
	h := a.Handler()
	for _, body := range []string{`{"slug":"replacement"}`, `invalid-json`} {
		w := call(t, h, "PUT", "/api/v1/chatgpt/model", body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "config_managed") || !strings.Contains(w.Body.String(), "models.toml") || plan.chosen != "" {
			t.Fatalf("profile configuration was not authoritative: %d %s; selected=%q", w.Code, w.Body.String(), plan.chosen)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/chatgpt/model", strings.NewReader(`{"slug":"replacement"}`)))
	if w.Code != http.StatusUnauthorized || plan.chosen != "" {
		t.Fatal("profile model control lost authentication")
	}
	if w = call(t, h, "POST", "/api/v1/chatgpt/sign-in", ""); w.Code != http.StatusOK || plan.starts != 1 {
		t.Fatalf("profile account sign-in failed: %d %s", w.Code, w.Body.String())
	}
	if w = call(t, h, "POST", "/api/v1/chatgpt/callback", `{"url":"http://127.0.0.1:1455/auth/callback?code=secret-profile-code&state=synthetic"}`); w.Code != http.StatusOK || plan.finishes != 1 || strings.Contains(w.Body.String(), "secret-profile-code") {
		t.Fatalf("profile callback failed or leaked credentials: %d %s", w.Code, w.Body.String())
	}
	if w = call(t, h, "GET", "/api/v1/chatgpt/models", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Account model") {
		t.Fatalf("profile account catalog failed: %d %s", w.Code, w.Body.String())
	}
	w = call(t, h, "GET", "/api/v1/chatgpt", "")
	for _, expected := range []string{`"active_lane":"profiles"`, `"profiles":`, `"analysis":{"profile":"subscription_analysis","model":"analysis-model","auth_mode":"siwc"}`, `"background":{"profile":"api_background","model":"background-model","auth_mode":"api_key"}`, `"chat":{"profile":"subscription_chat","model":"chat-model","auth_mode":"siwc"}`} {
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), expected) {
			t.Fatalf("profile status missing %q: %d %s", expected, w.Code, w.Body.String())
		}
	}
	a.Billing.Plan = nil
	if w = call(t, h, "PUT", "/api/v1/chatgpt/model", `{"slug":"replacement"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "config_managed") {
		t.Fatalf("API-only profiles should also reject global model selection: %d %s", w.Code, w.Body.String())
	}
	if w = call(t, h, "POST", "/api/v1/chatgpt/sign-in", ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "openai_chatgpt") {
		t.Fatalf("disabled connection instructions omit profile setup: %d %s", w.Code, w.Body.String())
	}
}
