// Package siwc implements the public Sign in with ChatGPT plan-usage route.
// Credentials and diagnostic response bodies must never be logged or returned to clients.
package siwc

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
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
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Visibility  string `json:"visibility,omitempty"`
}
type Status struct {
	Connected   bool   `json:"connected"`
	PlanEnabled bool   `json:"plan_enabled"`
	Model       string `json:"model"`
}
type Client struct {
	mu                        sync.Mutex // Covers authorization, refresh rotation, persistence and catalog invalidation.
	path                      string
	lock                      *os.File // One process per store; prevents refresh reuse across backend processes.
	creds                     credentials
	pending                   *attempt
	http                      *http.Client
	authURL, tokenURL, apiURL string
	keys                      oidc.KeySet
	catalog                   []Model
	catalogUntil              time.Time
}

func New(path, model string) (*Client, error) {
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
	} else {
		c.creds.HostID = "openalbert-" + rand.Text()
		if err = c.save(c.creds); err != nil {
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
	resp, err := c.http.Do(req)
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
func (c *Client) get(ctx context.Context, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+path, nil)
	if err != nil {
		return errors.New("invalid ChatGPT request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return errors.New("invalid ChatGPT response")
	}
	return nil
}
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	if time.Now().Before(c.catalogUntil) {
		return append([]Model{}, c.catalog...), nil
	}
	var response struct {
		Models []Model `json:"models"`
	}
	if err = c.get(ctx, "/models", token, &response); err != nil {
		return nil, err
	}
	models := []Model{}
	for _, m := range response.Models {
		if m.Visibility == "list" && m.Slug != "" && m.DisplayName != "" {
			models = append(models, m)
		}
	}
	c.catalog = models
	c.catalogUntil = time.Now().Add(5 * time.Minute)
	return append([]Model{}, models...), nil
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
