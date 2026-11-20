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
	c            *Client
	server       *httptest.Server
	key          *rsa.PrivateKey
	nonce        string
	tokenCalls   atomic.Int32
	refreshCalls atomic.Int32
	scope        string
	refreshError string
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
		var request responseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		model, _ := json.Marshal(request.Model)
		w.Header().Set("Content-Type", "text/event-stream")
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
