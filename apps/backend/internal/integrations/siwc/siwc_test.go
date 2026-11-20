package siwc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"openalbert/apps/backend/internal/domain"
)

var hostIDPattern = regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type oauthFixture struct {
	c              *Client
	server         *httptest.Server
	key            *rsa.PrivateKey
	nonce          string
	tokenCalls     atomic.Int32
	refreshCalls   atomic.Int32
	scope          string
	refreshError   string
	responseStream string
	responseStatus int
}

func newFixture(t *testing.T) *oauthFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &oauthFixture{key: key, scope: "openid offline_access " + directScope}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.c, err = New(filepath.Join(t.TempDir(), "credentials.json"), "test-model")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.c.Close() })
	f.c.tokenURL = f.server.URL + "/token"
	f.c.apiURL = f.server.URL
	f.c.keys = oidc.NewRemoteKeySet(context.Background(), f.server.URL+"/keys")
	return f
}
func (f *oauthFixture) sign(nonce, subject, audience string, expires time.Time) string {
	encode := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	s := encode(map[string]any{"alg": "RS256", "kid": "test"}) + "." + encode(map[string]any{"iss": issuer, "aud": audience, "sub": subject, "exp": expires.Unix(), "iat": time.Now().Unix(), "nonce": nonce})
	hash := sha256.Sum256([]byte(s))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, hash[:])
	return s + "." + base64.RawURLEncoding.EncodeToString(sig)
}
func (f *oauthFixture) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/keys":
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes())}}})
	case "/token":
		f.tokenCalls.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("resource") != resource || r.Form.Get("client_id") != "issued-client" {
			http.Error(w, "wrong client/resource", 400)
			return
		}
		if r.Form.Get("grant_type") == "refresh_token" {
			f.refreshCalls.Add(1)
			if f.refreshError != "" {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": f.refreshError})
				return
			}
		} else if r.Form.Get("redirect_uri") != redirectURI || r.Form.Get("code_verifier") == "" {
			http.Error(w, "missing PKCE", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-secret", "refresh_token": "rotated-secret", "token_type": "Bearer", "expires_in": 3600, "scope": f.scope, "id_token": f.sign(f.nonce, "owner", "issued-client", time.Now().Add(time.Hour))})
	case "/models":
		if r.Header.Get("Authorization") != "Bearer access-secret" {
			http.Error(w, "auth", 401)
			return
		}
		io.WriteString(w, `{"models":[{"slug":"test-model","display_name":"Test model","visibility":"list"},{"slug":"other-model","display_name":"Other model","visibility":"list"},{"slug":"hidden","display_name":"Hidden","visibility":"hidden"}]}`)
	case "/responses":
		if f.responseStatus != 0 {
			w.Header().Set("x-request-id", "rejected-request")
			w.WriteHeader(f.responseStatus)
			io.WriteString(w, `{"error":{"code":"rejected","message":"synthetic rejection"}}`)
			return
		}
		var request responseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		model, _ := json.Marshal(request.Model)
		w.Header().Set("Content-Type", "text/event-stream")
		if f.responseStream != "" {
			io.WriteString(w, f.responseStream)
			return
		}
		fmt.Fprintf(w, `data: {"type":"response.completed","response":{"status":"completed","model":"served-model-version","output":[{"type":"message","content":[{"type":"output_text","text":%s}]}],"usage":{"input_tokens":7,"output_tokens":1,"input_tokens_details":{"cached_tokens":4}}}}`+"\n\n", model)
	default:
		http.NotFound(w, r)
	}
}
func (f *oauthFixture) begin(t *testing.T) string {
	t.Helper()
	link, err := f.c.Begin()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	q := u.Query()
	if hostID := q.Get("ext_agent_host_id"); !hostIDPattern.MatchString(hostID) {
		t.Fatalf("expected a UUIDv4 URN in the authorize URL, got %q", hostID)
	}
	f.nonce = q.Get("nonce")
	if q.Get("agent_name_hint") != "OpenAlbert" && f.c.creds.ClientID == "" {
		t.Fatal("missing app name")
	}
	if q.Get("force_reconsent") != "" || q.Get("id_token_hint") != "" || q.Get("code_challenge_method") != "S256" {
		t.Fatal("unsafe authorize parameters")
	}
	hash := sha256.Sum256([]byte(f.c.pending.verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(hash[:]) {
		t.Fatal("PKCE mismatch")
	}
	return redirectURI + "?" + url.Values{"code": {"code-secret"}, "state": {q.Get("state")}, "client_id": {"issued-client"}}.Encode()
}
func (f *oauthFixture) login(t *testing.T) {
	t.Helper()
	callback := f.begin(t)
	if err := f.c.Finish(context.Background(), callback); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthInferenceAndRestart(t *testing.T) {
	f := newFixture(t)
	callback := f.begin(t)
	if err := f.c.Finish(context.Background(), strings.Replace(callback, "state=", "state=wrong", 1)); err == nil {
		t.Fatal("accepted wrong state")
	}
	if f.tokenCalls.Load() != 0 {
		t.Fatal("exchanged invalid callback")
	}
	if err := f.c.Finish(context.Background(), callback); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Finish(context.Background(), callback); err == nil {
		t.Fatal("replayed callback")
	}
	if !f.c.Status().PlanEnabled {
		t.Fatal("scope not enabled")
	}
	info, _ := os.Stat(f.c.path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
	if _, err := New(f.c.path, "test-model"); err == nil {
		t.Fatal("second process can rotate same store")
	}
	result, err := f.c.Complete(context.Background(), []domain.Message{{Role: "user", Content: "Hello"}}, nil)
	if err != nil || result.Message.Content != "test-model" || result.Model != "test-model" || result.InputTokens != 7 || result.Usage.Model != "served-model-version" || result.Usage.CachedInputTokens != 4 || !result.Usage.TokenUsageKnown {
		t.Fatalf("inference: %+v %v", result, err)
	}
	models, err := f.c.Models(context.Background())
	if err != nil || len(models) != 2 || models[0].DisplayName != "Test model" {
		t.Fatalf("catalog %+v %v", models, err)
	}
	if err = f.c.SetModel(context.Background(), "hidden"); err == nil {
		t.Fatal("selected hidden model")
	}
	if err = f.c.SetModel(context.Background(), "other-model"); err != nil {
		t.Fatal(err)
	}
	result, err = f.c.Complete(context.Background(), []domain.Message{{Role: "user", Content: "Hello"}}, nil)
	if err != nil || result.Message.Content != "other-model" || result.Model != "other-model" {
		t.Fatalf("inference after model switch: %+v %v", result, err)
	}
	f.c.Close()
	restarted, err := New(f.c.path, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if !restarted.Status().PlanEnabled || restarted.creds.HostID != f.c.creds.HostID || restarted.creds.Model != "other-model" {
		t.Fatal("session did not survive restart")
	}
	link, err := restarted.Begin()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	if u.Query().Get("client_id") != "issued-client" || u.Query().Get("agent_name_hint") != "" {
		t.Fatal("did not reuse registration")
	}
}
func TestLegacyHostIDMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	previous := credentials{
		HostID:       "openalbert-LEGACYHOST",
		ClientID:     "issued-client",
		Subject:      "owner",
		IDToken:      "id-token",
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Date(2026, time.November, 19, 18, 0, 0, 0, time.UTC),
		Scopes:       []string{"openid", directScope},
		Model:        "saved-model",
	}
	data, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	c, err := New(path, "default-model")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	link, err := c.Begin()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	hostID := u.Query().Get("ext_agent_host_id")
	if !hostIDPattern.MatchString(hostID) || u.Query().Get("client_id") != previous.ClientID {
		t.Fatal("authorize URL must use the migrated host ID and the existing client ID")
	}

	previous.HostID = hostID
	if !reflect.DeepEqual(c.creds, previous) {
		t.Fatal("host ID migration changed the saved connection")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(path, "default-model")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if !reflect.DeepEqual(restarted.creds, previous) {
		t.Fatal("migrated connection did not survive restart")
	}
}

func TestIDTokenValidationAndNoScope(t *testing.T) {
	for _, kind := range []string{"signature", "issuer", "audience", "expiry", "nonce", "subject"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			callback := f.begin(t)
			nonce := f.nonce
			subject := "owner"
			aud := "issued-client"
			exp := time.Now().Add(time.Hour)
			switch kind {
			case "nonce":
				nonce = "wrong"
			case "subject":
				subject = ""
			case "audience":
				aud = "wrong"
			case "expiry":
				exp = time.Now().Add(-time.Hour)
			}
			token := f.sign(nonce, subject, aud, exp)
			if kind == "signature" {
				parts := strings.Split(token, ".")
				parts[2] = base64.RawURLEncoding.EncodeToString(make([]byte, 256))
				token = strings.Join(parts, ".")
			}
			if kind == "issuer" {
				parts := strings.Split(token, ".")
				data, _ := base64.RawURLEncoding.DecodeString(parts[1])
				parts[1] = base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(data), issuer, "https://attacker.example", 1)))
				s := strings.Join(parts[:2], ".")
				hash := sha256.Sum256([]byte(s))
				sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, hash[:])
				token = s + "." + base64.RawURLEncoding.EncodeToString(sig)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "bad-access", "token_type": "Bearer", "expires_in": 3600, "scope": directScope, "id_token": token})
			}))
			defer server.Close()
			f.c.tokenURL = server.URL
			if err := f.c.Finish(context.Background(), callback); err == nil || f.c.Status().Connected {
				t.Fatal("accepted invalid identity")
			}
		})
	}
	f := newFixture(t)
	f.scope = "openid"
	f.login(t)
	if !f.c.Status().Connected || f.c.Status().PlanEnabled {
		t.Fatal("missing direct scope treated as authorization")
	}
	if _, err := f.c.Complete(context.Background(), nil, nil); !errors.Is(err, ErrPermission) {
		t.Fatal(err)
	}
}
func TestRefreshSerializedAndFailureRecovery(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	f.c.creds.ExpiresAt = time.Now().Add(-time.Hour)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.c.Complete(context.Background(), []domain.Message{{Role: "user", Content: "hi"}}, nil)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.refreshCalls.Load() != 1 {
		t.Fatalf("refresh raced: %d", f.refreshCalls.Load())
	}
	data, _ := os.ReadFile(f.c.path)
	if !strings.Contains(string(data), "rotated-secret") {
		t.Fatal("rotation not persisted")
	}
	for _, code := range []string{"temporarily_unavailable", "invalid_client", "invalid_grant"} {
		f.c.creds.ExpiresAt = time.Now().Add(-time.Hour)
		f.refreshError = code
		_, err := f.c.Complete(context.Background(), nil, nil)
		if err == nil {
			t.Fatal("refresh succeeded")
		}
		if code == "invalid_grant" {
			if f.c.creds.AccessToken != "" || f.c.creds.ClientID != "issued-client" || f.c.creds.HostID == "" {
				t.Fatal("terminal refresh lost registration or retained tokens")
			}
		} else if f.c.creds.AccessToken == "" {
			t.Fatal("transient error erased tokens")
		}
	}
}

func TestCompleteForModelConcurrentRequestsShareOnlyCredentials(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	f.c.creds.ExpiresAt = time.Now().Add(-time.Hour)
	const requests = 20
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{}, requests)
	release := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer access-secret" {
			t.Errorf("incorrect response route or shared authorization")
			http.Error(w, "invalid route or authorization", http.StatusBadRequest)
			return
		}
		var request struct {
			Model string `json:"model"`
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Input) != 1 || request.Input[0].Content != request.Model {
			t.Errorf("model leaked across requests: %+v %v", request, err)
			http.Error(w, "incorrect model", http.StatusBadRequest)
			return
		}
		if calls.Add(1) <= requests {
			entered <- struct{}{}
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, `data: {"type":"response.completed","response":{"status":"completed","model":%q,"output":[{"type":"message","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":7,"output_tokens":1}}}`+"\n\n", "served-"+request.Model, request.Model)
	}))
	defer server.Close()
	f.c.apiURL = server.URL
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			model := fmt.Sprintf("profile-model-%d", i)
			out, err := f.c.CompleteForModel(ctx, model, []domain.Message{{Role: "user", Content: model}}, nil)
			if err != nil || out.Model != model || out.Message.Content != model || out.Usage.Model != "served-"+model || !out.Usage.TokenUsageKnown {
				t.Errorf("incorrect request-local completion: %+v %v", out, err)
			}
		}()
	}
	for range requests {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Errorf("requests did not reach the provider concurrently: %v", ctx.Err())
		}
	}
	// Observe the saved choice while every request is still in flight: a
	// temporary account mutation must not serve as per-request routing.
	if got := f.c.Status().Model; got != "test-model" {
		t.Errorf("concurrent profile calls changed the saved model: %q", got)
	}
	close(release)
	wg.Wait()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("shared refresh raced: %d", f.refreshCalls.Load())
	}
	data, err := os.ReadFile(f.c.path)
	var saved credentials
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Model != "test-model" || saved.RefreshToken != "rotated-secret" {
		t.Fatalf("profile calls changed persisted model or lost token rotation: %+v %v", saved, err)
	}
	out, err := f.c.Complete(ctx, []domain.Message{{Role: "user", Content: "test-model"}}, nil)
	if err != nil || out.Model != "test-model" || out.Message.Content != "test-model" {
		t.Fatalf("legacy completion lost the saved selection: %+v %v", out, err)
	}
}

func TestEstimateInputUsesExactRequestAndConservativeFraming(t *testing.T) {
	messages := []domain.Message{
		{Role: "system", Content: "Инструкция <&>"},
		{Role: "user", Content: "画像", Images: []string{"data:image/png;base64,AA=="}},
		{Role: "assistant", ResponsesItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque state"}`), json.RawMessage(`{"type":"function_call","namespace":"openalbert","call_id":"c1","name":"lookup","arguments":"{}"}`)}},
		{Role: "tool", ToolCallID: "c1", Content: "résultat"},
	}
	tools := []domain.Tool{{Name: "lookup", Description: "Поиск", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}}
	body, err := buildRequest("profile-model", messages, tools)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EstimateInput("profile-model", messages, tools)
	want := len(raw) + len(messages)*64 + len(tools)*128 + 256
	if err != nil || got != want {
		t.Fatalf("estimate omitted serialized request data/framing: got %d, want %d: %v", got, want, err)
	}
	for _, tc := range []struct {
		name     string
		messages []domain.Message
		tools    []domain.Tool
	}{
		{"unsupported role", []domain.Message{{Role: "invalid", Content: "text"}}, nil},
		{"invalid tool schema", nil, []domain.Tool{{Name: "broken", Parameters: json.RawMessage(`invalid`)}}},
		{"invalid opaque history", []domain.Message{{Role: "assistant", ResponsesItems: []json.RawMessage{json.RawMessage(`invalid`)}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if estimate, err := EstimateInput("profile-model", tc.messages, tc.tools); err == nil || estimate != 0 {
				t.Fatalf("invalid input estimated as usable: %d %v", estimate, err)
			}
		})
	}
}

func TestRequestAllowlistAndToolHistory(t *testing.T) {
	messages := []domain.Message{{Role: "system", Content: "instructions"}, {Role: "user", Content: "look", Images: []string{"data:image/png;base64,AA=="}}, {Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"x":1}`)}}}, {Role: "tool", ToolCallID: "c1", Content: "result"}}
	body, err := buildRequest("model", messages, []domain.Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	var v map[string]json.RawMessage
	_ = json.Unmarshal(raw, &v)
	for key := range v {
		switch key {
		case "model", "input", "store", "stream", "tools", "include":
		default:
			t.Fatalf("unapproved field %s", key)
		}
	}
	if string(v["store"]) != "false" || string(v["stream"]) != "true" || strings.Contains(string(raw), `"role":"system"`) || !strings.Contains(string(raw), `"type":"namespace"`) || !strings.Contains(string(raw), `"type":"function_call_output"`) {
		t.Fatal(string(raw))
	}
	if len(body.Include) != 1 || body.Include[0] != "reasoning.encrypted_content" {
		t.Fatal("missing stateless reasoning")
	}
	if len(body.Input) != 4 {
		t.Fatal("lost history")
	}
}
func TestStreamTerminalEventsAndDiagnostics(t *testing.T) {
	for _, event := range []string{`{"type":"response.output_text.delta","delta":"partial"}`, `{"type":"response.incomplete"}`, `{"type":"response.failed","response":{"error":{"code":"subscription_sharing_usage_limit_exceeded","param":"model"}}}`, `{"type":"error","code":"failure"}`} {
		if _, err := readStream(strings.NewReader("data: " + event + "\n\n")); err == nil {
			t.Fatal("accepted non-completion")
		}
	}
	event := `{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","namespace":"openalbert","call_id":"c1","name":"lookup","arguments":"{}"}],"usage":{"input_tokens":2,"output_tokens":3}}}`
	result, err := readStream(strings.NewReader("event: response.completed\r\ndata: " + event + "\r\n\r\n"))
	if err != nil || len(result.Message.ToolCalls) != 1 || result.OutputTokens != 3 || !result.Usage.TokenUsageKnown {
		t.Fatalf("tool completion %+v %v", result, err)
	}
	resp := &http.Response{StatusCode: 403, Header: http.Header{"X-Request-Id": []string{"req-test"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"subscription_sharing_route_not_supported","param":"model","message":"secret"}}`))}
	e := responseError(resp)
	if e.Code != "subscription_sharing_route_not_supported" || e.Param != "model" || e.RequestID != "req-test" || e.Status != 403 || !strings.Contains(string(e.Body), "secret") || strings.Contains(fmt.Sprint(e), "secret") {
		t.Fatalf("lost or leaked diagnostics: %+v", e)
	}
}

func TestFailedResponsesPreserveUsageWithoutPartialOutput(t *testing.T) {
	f := newFixture(t)
	f.login(t)
	partial := `{"type":"message","content":[{"type":"output_text","text":"partial"}]},{"type":"function_call","namespace":"openalbert","call_id":"partial-call","name":"lookup","arguments":"{}"}`
	for _, tc := range []struct {
		name, eventType, status, output string
	}{
		{"incomplete", "response.incomplete", "incomplete", partial},
		{"failed", "response.failed", "failed", partial},
		{"error", "error", "failed", partial},
		{"invalid call", "response.completed", "completed", partial + `,{"type":"function_call","call_id":"bad","name":"lookup","arguments":"invalid"}`},
		{"invalid item", "response.completed", "completed", partial + `,{"type":"message","content":true}`},
		{"invalid status", "response.completed", "incomplete", partial},
		{"empty output", "response.completed", "completed", `{"type":"reasoning","encrypted_content":"partial"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := fmt.Sprintf(`{"type":%q,"code":"failure","response":{"status":%q,"model":"served-model-version","output":[%s],"usage":{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":4}}}}`, tc.eventType, tc.status, tc.output)
			f.responseStream = "data: " + event + "\n\n"
			decoded, err := readStream(strings.NewReader(f.responseStream))
			if err == nil || decoded.InputTokens != 7 || decoded.OutputTokens != 3 || !decoded.Usage.TokenUsageKnown || decoded.Usage.CachedInputTokens != 4 || decoded.Usage.Model != "served-model-version" || !reflect.DeepEqual(decoded.Message, domain.Message{}) {
				t.Fatalf("decoder lost usage or exposed partial output: %+v %v", decoded, err)
			}
			got, err := f.c.Complete(context.Background(), []domain.Message{{Role: "user", Content: "hello"}}, nil)
			if err == nil || got.Model != "test-model" || got.InputTokens != 7 || got.OutputTokens != 3 || got.Usage != decoded.Usage || !reflect.DeepEqual(got.Message, domain.Message{}) {
				t.Fatalf("client lost usage/model or exposed partial output: %+v %v", got, err)
			}
			got, err = f.c.CompleteForModel(context.Background(), "profile-model", []domain.Message{{Role: "user", Content: "hello"}}, nil)
			if err == nil || got.Model != "profile-model" || got.InputTokens != 7 || got.OutputTokens != 3 || got.Usage != decoded.Usage || !reflect.DeepEqual(got.Message, domain.Message{}) {
				t.Fatalf("profile call lost usage/model or exposed partial output: %+v %v", got, err)
			}
		})
	}
}

func TestTerminalResponseUsageValidation(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.incomplete", "response.failed", "error"} {
		for _, tc := range []struct {
			name, usage           string
			known                 bool
			input, output, cached int
		}{
			{"negative input", `{"input_tokens":-1,"output_tokens":3}`, false, 0, 0, 0},
			{"negative output", `{"input_tokens":7,"output_tokens":-1}`, false, 0, 0, 0},
			{"cache above input", `{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":20}}`, true, 7, 3, 7},
			{"negative cache", `{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":-1}}`, true, 7, 3, 0},
		} {
			t.Run(eventType+"/"+tc.name, func(t *testing.T) {
				event := fmt.Sprintf(`{"type":%q,"response":{"status":"completed","model":"served","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":%s}}`, eventType, tc.usage)
				got, err := readStream(strings.NewReader("data: " + event + "\n\n"))
				if (eventType != "response.completed" || !tc.known) && err == nil {
					t.Fatal("accepted failed response or invalid usage")
				}
				if got.InputTokens != tc.input || got.OutputTokens != tc.output || got.Usage.TokenUsageKnown != tc.known || got.Usage.CachedInputTokens != tc.cached || got.Usage.Model != "served" {
					t.Fatalf("invalid usage: %+v %v", got, err)
				}
				if err != nil && !reflect.DeepEqual(got.Message, domain.Message{}) {
					t.Fatalf("failed response exposed output: %+v", got.Message)
				}
			})
		}
	}
}

func TestReasoningAndToolItemsSurviveStatelessContinuation(t *testing.T) {
	event := `{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"r1","summary":[],"encrypted_content":"opaque-state"},{"type":"function_call","id":"fc1","namespace":"openalbert","call_id":"c1","name":"lookup","arguments":"{}"}]}}`
	first, err := readStream(strings.NewReader("data: " + event + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := buildRequest("model", []domain.Message{{Role: "user", Content: "lookup"}, first.Message, {Role: "tool", ToolCallID: "c1", Content: "found"}}, nil)
	if err != nil || len(body.Input) != 4 {
		t.Fatalf("lost continuation: %+v %v", body, err)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), `"encrypted_content":"opaque-state"`) || strings.Count(string(raw), `"type":"function_call"`) != 1 || !strings.Contains(string(raw), `"type":"function_call_output"`) {
		t.Fatal(string(raw))
	}
}

func TestExplicitMissingPermissionRetryRequestsConsent(t *testing.T) {
	f := newFixture(t)
	f.scope = "openid"
	f.login(t)
	link, err := f.c.Begin()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	if u.Query().Get("prompt") != "consent" || u.Query().Get("force_reconsent") != "" || !strings.Contains(u.Query().Get("scope"), directScope) {
		t.Fatal("missing explicit plan-consent recovery")
	}
	g := newFixture(t)
	g.login(t)
	link, err = g.c.Begin()
	if err != nil {
		t.Fatal(err)
	}
	u, _ = url.Parse(link)
	if u.Query().Get("prompt") != "" {
		t.Fatal("ordinary sign-in forced consent")
	}
}

func TestStreamUsagePresence(t *testing.T) {
	for _, tc := range []struct {
		name, usage   string
		known         bool
		input, output int
	}{
		{"counts", `{"input_tokens":7,"output_tokens":3}`, true, 7, 3},
		{"zeroes", `{"input_tokens":0,"output_tokens":0}`, true, 0, 0},
		{"missing", `{}`, false, 0, 0},
		{"input-only", `{"input_tokens":7}`, false, 7, 0},
		{"output-only", `{"output_tokens":3}`, false, 0, 3},
		{"null", `{"input_tokens":null,"output_tokens":0}`, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":` + tc.usage + `}}`
			got, err := readStream(strings.NewReader("data: " + event + "\n\n"))
			if err != nil || got.Usage.TokenUsageKnown != tc.known || got.InputTokens != tc.input || got.OutputTokens != tc.output {
				t.Fatalf("usage presence lost: %+v %v", got, err)
			}
		})
	}
}

func TestHTTPRejectionRetainsRequestedModelWithUnknownUsage(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFixture(t)
			f.responseStatus = status
			f.login(t)
			out, err := f.c.Complete(context.Background(), []domain.Message{{Role: "user", Content: "Hello"}}, nil)
			var api *APIError
			if !errors.As(err, &api) || api.Status != status || api.RequestID != "rejected-request" || out.Model != "test-model" || out.Usage.TokenUsageKnown || out.InputTokens != 0 || out.OutputTokens != 0 || !reflect.DeepEqual(out.Message, domain.Message{}) {
				t.Fatalf("HTTP rejection lost request identity or invented usage/output: %+v %v", out, err)
			}
			out, err = f.c.CompleteForModel(context.Background(), "profile-model", []domain.Message{{Role: "user", Content: "Hello"}}, nil)
			if !errors.As(err, &api) || api.Status != status || api.RequestID != "rejected-request" || out.Model != "profile-model" || out.Usage.TokenUsageKnown || out.InputTokens != 0 || out.OutputTokens != 0 || !reflect.DeepEqual(out.Message, domain.Message{}) {
				t.Fatalf("profile HTTP rejection lost request identity or invented usage/output: %+v %v", out, err)
			}
		})
	}
}

func TestProfileDeadlineIncludesWaitingForCredentialRefresh(t *testing.T) {
	f := newFixture(t)
	f.c.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.c.CompleteForModel(ctx, "profile-model", nil, nil); done <- err }()
	select {
	case err := <-done:
		f.c.mu.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("credential wait ignored deadline: %v", err)
		}
	case <-time.After(time.Second):
		f.c.mu.Unlock()
		<-done
		t.Fatal("profile request waited beyond its deadline for another refresh")
	}
}
