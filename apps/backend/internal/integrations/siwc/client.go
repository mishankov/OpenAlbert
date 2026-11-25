// Package siwc implements the public Sign in with ChatGPT plan-usage route.
// Credentials and diagnostic response bodies must never be logged or returned to clients.
package siwc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"openalbert/apps/backend/internal/domain"
)

const resource = "https://api.openai.com/v1"
const issuer = "https://auth.openai.com"
const redirectURI = "http://127.0.0.1:1455/auth/callback"
const directScope = "chatgpt.tokens.use.direct"

var ErrDisconnected = errors.New("Sign in with ChatGPT is required")
var ErrPermission = errors.New("ChatGPT plan usage is disabled; sign in to enable it or use an API key")

type credentials struct {
	HostID       string    `json:"ext_agent_host_id"`
	ClientID     string    `json:"client_id,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes,omitempty"`
	Model        string    `json:"model,omitempty"`
}
type attempt struct {
	state, nonce, verifier, clientID string
	expires                          time.Time
}
type Model struct {
	Slug                     string   `json:"slug"`
	DisplayName              string   `json:"display_name"`
	Visibility               string   `json:"visibility,omitempty"`
	SupportedReasoningLevels []string `json:"supported_reasoning_efforts,omitempty"`
}

func parseReasoningLevels(raw []json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, entry := range raw {
		var effort string
		if json.Unmarshal(entry, &effort) != nil {
			var obj struct {
				Effort string `json:"effort"`
				Level  string `json:"level"`
			}
			if json.Unmarshal(entry, &obj) == nil {
				effort = obj.Effort
				if effort == "" {
					effort = obj.Level
				}
			}
		}
		effort = strings.TrimSpace(effort)
		switch effort {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
			if !seen[effort] {
				seen[effort] = true
				out = append(out, effort)
			}
		}
	}
	return out
}

type Status struct {
	Connected   bool   `json:"connected"`
	PlanEnabled bool   `json:"plan_enabled"`
	Model       string `json:"model"`
}
type Client struct {
	mu                        credentialLock // Covers authorization, refresh rotation, persistence and catalog invalidation.
	path                      string
	lock                      *os.File // One process per store; prevents refresh reuse across backend processes.
	creds                     credentials
	pending                   *attempt
	http                      *http.Client
	authURL, tokenURL, apiURL string
	catalogEndpoint           apiEndpoint
	keys                      oidc.KeySet
	catalog                   []Model
	catalogUntil              time.Time
}

type apiEndpoint struct {
	baseURL string
	headers map[string]string
}

// credentialLock keeps authorization/refresh serialized while allowing a model
// request's deadline to expire while another request is refreshing credentials.
type credentialLock struct {
	once sync.Once
	held chan struct{}
}

func (m *credentialLock) LockContext(ctx context.Context) error {
	m.once.Do(func() { m.held = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.held <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *credentialLock) Lock()   { _ = m.LockContext(context.Background()) }
func (m *credentialLock) Unlock() { <-m.held }

func New(path, model string) (*Client, error) {
	return NewWithCatalogProfile(path, model, domain.ModelProfile{})
}

// NewWithCatalogProfile uses the selected profile for the account's model catalog.
func NewWithCatalogProfile(path, model string, profile domain.ModelProfile) (*Client, error) {
	if path == "" {
		return nil, errors.New("OPENAI_TOKEN_STORE is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, errors.New("cannot create ChatGPT credential directory")
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("cannot open ChatGPT credential lock")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("ChatGPT credential store is already in use")
	}
	c := &Client{path: path, lock: f, http: &http.Client{Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, authURL: issuer + "/api/accounts/authorize", tokenURL: issuer + "/api/accounts/oauth/token", apiURL: resource}
	c.catalogEndpoint = apiEndpoint{baseURL: profile.BaseURL, headers: maps.Clone(profile.Headers)}
	c.keys = oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), c.http), issuer+"/.well-known/jwks.json")
	data, err := os.ReadFile(path)
	if err == nil {
		info, e := os.Stat(path)
		if e != nil || info.Mode().Perm()&0077 != 0 || json.Unmarshal(data, &c.creds) != nil || c.creds.HostID == "" {
			c.Close()
			return nil, errors.New("invalid or unprotected ChatGPT credential store (requires 0600)")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		c.Close()
		return nil, errors.New("cannot read ChatGPT credential store")
	}
	// OpenAI requires a host identifier in a supported format. Replace IDs
	// generated by earlier releases while keeping the rest of the connection.
	if c.creds.HostID == "" || strings.HasPrefix(c.creds.HostID, "openalbert-") {
		updated := c.creds
		updated.HostID = uuid.New().URN()
		if err := c.save(updated); err != nil {
			c.Close()
			return nil, err
		}
	}
	if c.creds.Model == "" {
		c.creds.Model = model
	}
	return c, nil
}
func (c *Client) Close() error { return c.lock.Close() }
func (c *Client) save(v credentials) error {
	data, err := json.Marshal(v)
	if err != nil {
		return errors.New("cannot encode ChatGPT credentials")
	}
	f, err := os.CreateTemp(filepath.Dir(c.path), ".chatgpt-*")
	if err != nil {
		return errors.New("cannot write ChatGPT credentials")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(name, c.path)
	}
	if err != nil {
		return errors.New("cannot persist ChatGPT credentials")
	}
	c.creds = v
	return nil
}
func hasScope(scopes []string, name string) bool {
	for _, s := range scopes {
		if s == name {
			return true
		}
	}
	return false
}
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{Connected: c.creds.Subject != "" && c.creds.IDToken != "", PlanEnabled: c.creds.AccessToken != "" && hasScope(c.creds.Scopes, directScope), Model: c.creds.Model}
}

// APIError retains provider diagnostics in memory. Error deliberately omits the
// body/message, which can contain user content or credentials. Do not serialize it.
type APIError struct {
	Status                 int
	Code, Param, RequestID string
	Body                   json.RawMessage
}

func (e *APIError) Error() string {
	return "ChatGPT request failed; inspect billing status and request ID"
}
func (e *APIError) HTTPStatus() int { return e.Status }
func (e *APIError) NonRetryable() bool {
	return e.Code == "subscription_sharing_unsupported_capability"
}
func responseError(resp *http.Response) *APIError {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	e := &APIError{Status: resp.StatusCode, RequestID: resp.Header.Get("x-request-id"), Body: data}
	var envelope struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	if json.Unmarshal(data, &envelope) == nil {
		if json.Unmarshal(envelope.Error, &e.Code) != nil {
			var detail struct {
				Code  string `json:"code"`
				Param string `json:"param"`
			}
			_ = json.Unmarshal(envelope.Error, &detail)
			e.Code = detail.Code
			e.Param = detail.Param
		}
		if e.Code == "" {
			e.Code = envelope.Code
		}
	}
	return e
}
func (c *Client) do(req *http.Request) (*http.Response, error) {
	return do(c.http, req)
}

func do(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, errors.New("ChatGPT connection failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, responseError(resp)
	}
	return resp, nil
}
func (c *Client) apiRequest(ctx context.Context, endpoint apiEndpoint, method, path, token string, body io.Reader) (*http.Request, error) {
	baseURL := endpoint.baseURL
	if baseURL == "" {
		baseURL = c.apiURL
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return nil, errors.New("invalid ChatGPT request")
	}
	for name, value := range endpoint.headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func (endpoint apiEndpoint) safeDiagnostic(value, token string) string {
	if token != "" && strings.Contains(value, token) {
		return ""
	}
	for _, header := range endpoint.headers {
		if secret := strings.TrimSpace(header); secret != "" && strings.Contains(value, secret) {
			return ""
		}
	}
	return value
}

func (endpoint apiEndpoint) sanitizeError(err error, token string) error {
	var api *APIError
	if errors.As(err, &api) {
		api.Code = endpoint.safeDiagnostic(api.Code, token)
		api.Param = endpoint.safeDiagnostic(api.Param, token)
		api.RequestID = endpoint.safeDiagnostic(api.RequestID, token)
		if endpoint.baseURL != "" || len(endpoint.headers) != 0 {
			// An opaque response body may echo credentials in an encoded form.
			api.Body = nil
		}
	}
	return err
}

func (c *Client) get(ctx context.Context, endpoint apiEndpoint, path, token string, out any) error {
	req, err := c.apiRequest(ctx, endpoint, http.MethodGet, path, token, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return endpoint.sanitizeError(err, token)
	}
	defer resp.Body.Close()
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return errors.New("invalid ChatGPT response")
	}
	return nil
}
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	return c.ModelsFresh(ctx, false)
}

func (c *Client) ModelsFresh(ctx context.Context, refresh bool) ([]Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	if !refresh && time.Now().Before(c.catalogUntil) {
		return append([]Model{}, c.catalog...), nil
	}
	models, err := c.fetchModels(ctx, c.catalogEndpoint, token)
	if err != nil {
		return nil, err
	}
	c.catalog = models
	c.catalogUntil = time.Now().Add(5 * time.Minute)
	return append([]Model{}, models...), nil
}

// ModelsForProfile fetches the ChatGPT model catalog through a specific provider's
// endpoint and headers while sharing the connection's serialized token refresh.
func (c *Client) ModelsForProfile(ctx context.Context, profile domain.ModelProfile, refresh bool) ([]Model, error) {
	endpoint := apiEndpoint{baseURL: profile.BaseURL, headers: maps.Clone(profile.Headers)}
	if endpoint.baseURL == c.catalogEndpoint.baseURL && maps.Equal(endpoint.headers, c.catalogEndpoint.headers) {
		return c.ModelsFresh(ctx, refresh)
	}
	if err := c.mu.LockContext(ctx); err != nil {
		return nil, err
	}
	token, err := c.accessToken(ctx)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.fetchModels(ctx, endpoint, token)
}

func (c *Client) fetchModels(ctx context.Context, endpoint apiEndpoint, token string) ([]Model, error) {
	var response struct {
		Models []struct {
			Slug                      string            `json:"slug"`
			ID                        string            `json:"id"`
			DisplayName               string            `json:"display_name"`
			Name                      string            `json:"name"`
			Visibility                string            `json:"visibility"`
			SupportedReasoningLevels  []json.RawMessage `json:"supported_reasoning_levels"`
			SupportedReasoningEfforts []json.RawMessage `json:"supported_reasoning_efforts"`
		} `json:"models"`
	}
	if err := c.get(ctx, endpoint, "/models", token, &response); err != nil {
		return nil, err
	}
	models := []Model{}
	for _, raw := range response.Models {
		if raw.Visibility != "list" {
			continue
		}
		slug := strings.TrimSpace(raw.Slug)
		if slug == "" {
			slug = strings.TrimSpace(raw.ID)
		}
		if slug == "" || endpoint.safeDiagnostic(slug, token) == "" {
			continue
		}
		display := strings.TrimSpace(raw.DisplayName)
		if display == "" {
			display = strings.TrimSpace(raw.Name)
		}
		if display != "" && endpoint.safeDiagnostic(display, token) == "" {
			continue
		}
		if display == "" {
			display = slug
		}
		efforts := parseReasoningLevels(raw.SupportedReasoningLevels)
		if len(efforts) == 0 {
			efforts = parseReasoningLevels(raw.SupportedReasoningEfforts)
		}
		models = append(models, Model{
			Slug:                     slug,
			DisplayName:              display,
			Visibility:               raw.Visibility,
			SupportedReasoningLevels: efforts,
		})
	}
	return models, nil
}
func (c *Client) SetModel(ctx context.Context, slug string) error {
	models, err := c.Models(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range models {
		if m.Slug == slug {
			v := c.creds
			v.Model = slug
			return c.save(v)
		}
	}
	return errors.New("choose a model from the ChatGPT catalog")
}
func terminalRefresh(code string) bool {
	switch code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
		return true
	}
	return false
}
func scopes(value string) []string { return strings.Fields(value) }
