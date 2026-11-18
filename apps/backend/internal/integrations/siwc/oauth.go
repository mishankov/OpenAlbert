package siwc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Begin returns a short-lived browser URL. Hints containing tokens are never put
// into URLs because the VPS copy/paste flow also sends these links via Telegram.
func (c *Client) Begin() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil && time.Now().Before(c.pending.expires) {
		return "", errors.New("ChatGPT sign-in is already pending; finish it or wait ten minutes")
	}
	a := &attempt{state: rand.Text(), nonce: rand.Text(), verifier: rand.Text() + rand.Text(), clientID: c.creds.ClientID, expires: time.Now().Add(10 * time.Minute)}
	clientID := a.clientID
	if clientID == "" {
		clientID = "dynamic_agent_client"
	}
	digest := sha256.Sum256([]byte(a.verifier))
	q := url.Values{"client_id": {clientID}, "ext_agent_host_id": {c.creds.HostID}, "response_type": {"code"}, "redirect_uri": {redirectURI}, "scope": {"openid profile email offline_access resource.invoke " + directScope}, "resource": {resource}, "state": {a.state}, "nonce": {a.nonce}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}}
	if a.clientID == "" {
		q.Set("agent_name_hint", "OpenAlbert")
	} else if c.creds.Subject != "" && c.creds.IDToken != "" && !hasScope(c.creds.Scopes, directScope) {
		// Only an explicit retry of a validated identity with a disabled plan grant
		// needs consent. force_reconsent requires OpenAI rollout confirmation.
		q.Set("prompt", "consent")
	}
	c.pending = a
	return c.authURL + "?" + q.Encode(), nil
}

func (c *Client) Finish(ctx context.Context, callback string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.pending
	if a == nil || !time.Now().Before(a.expires) {
		c.pending = nil
		return errors.New("ChatGPT sign-in expired; start again")
	}
	u, err := url.Parse(strings.TrimSpace(callback))
	if err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:1455" || u.Path != "/auth/callback" || u.User != nil || u.Fragment != "" {
		return errors.New("paste the complete loopback callback URL")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(a.state)) != 1 {
		return errors.New("ChatGPT sign-in state mismatch")
	}
	c.pending = nil // Consume a validated callback once, including denied or failed exchanges.
	if q.Get("error") != "" {
		return errors.New("ChatGPT sign-in was declined; start again or use an API key")
	}
	if len(q["code"]) != 1 || q.Get("code") == "" || len(q["client_id"]) > 1 {
		return errors.New("incomplete ChatGPT callback")
	}
	clientID := a.clientID
	if clientID == "" {
		clientID = q.Get("client_id")
		if clientID == "" || clientID == "dynamic_agent_client" {
			return errors.New("ChatGPT registration did not issue a client ID")
		}
	} else if issued := q.Get("client_id"); issued != "" && issued != clientID {
		return errors.New("ChatGPT client identity changed")
	}
	// Retain an issued registration even if the one-use code exchange fails.
	if c.creds.ClientID == "" {
		v := c.creds
		v.ClientID = clientID
		if err = c.save(v); err != nil {
			return err
		}
	}
	token, err := c.exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {q.Get("code")}, "code_verifier": {a.verifier}, "redirect_uri": {redirectURI}, "resource": {resource}})
	if err != nil {
		return err
	}
	verifier := oidc.NewVerifier(issuer, c.keys, &oidc.Config{ClientID: clientID})
	identity, err := verifier.Verify(ctx, token.IDToken)
	if err != nil || identity.Subject == "" || subtle.ConstantTimeCompare([]byte(identity.Nonce), []byte(a.nonce)) != 1 {
		return errors.New("ChatGPT ID token validation failed")
	}
	if c.creds.Subject != "" && c.creds.Subject != identity.Subject {
		return errors.New("ChatGPT account changed; use the previously connected account")
	}
	v := c.creds
	v.Subject = identity.Subject
	v.IDToken = token.IDToken
	v.AccessToken = token.AccessToken
	v.RefreshToken = token.RefreshToken
	v.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	v.Scopes = scopes(token.Scope)
	if err = c.save(v); err != nil {
		return err
	}
	c.catalog = nil
	c.catalogUntil = time.Time{}
	return nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (c *Client) exchange(ctx context.Context, form url.Values) (tokenResponse, error) {
	var token tokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return token, errors.New("invalid ChatGPT token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.do(req)
	if err != nil {
		return token, err
	}
	defer resp.Body.Close()
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token) != nil || (hasScope(scopes(token.Scope), directScope) && token.AccessToken == "") || (token.AccessToken != "" && (token.ExpiresIn <= 0 || !strings.EqualFold(token.TokenType, "Bearer"))) {
		return token, errors.New("invalid ChatGPT token response")
	}
	return token, nil
}

// accessToken requires mu. Rotation and saving complete before another caller
// can use the refreshed credentials. Transient failures leave the store intact.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	if c.creds.AccessToken == "" {
		return "", ErrDisconnected
	}
	if !hasScope(c.creds.Scopes, directScope) {
		return "", ErrPermission
	}
	if time.Until(c.creds.ExpiresAt) > time.Minute {
		return c.creds.AccessToken, nil
	}
	if c.creds.RefreshToken == "" {
		return "", ErrDisconnected
	}
	token, err := c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {c.creds.ClientID}, "refresh_token": {c.creds.RefreshToken}, "resource": {resource}})
	if err != nil {
		var api *APIError
		if errors.As(err, &api) && terminalRefresh(api.Code) {
			v := c.creds
			v.AccessToken = ""
			v.RefreshToken = ""
			v.IDToken = ""
			v.Scopes = nil
			if saveErr := c.save(v); saveErr != nil {
				return "", saveErr
			}
			return "", errors.Join(ErrDisconnected, err)
		}
		return "", err
	}
	v := c.creds
	v.AccessToken = token.AccessToken
	v.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	if token.RefreshToken != "" {
		v.RefreshToken = token.RefreshToken
	}
	if token.Scope != "" {
		v.Scopes = scopes(token.Scope)
	}
	if err = c.save(v); err != nil {
		// The old refresh token may already be spent. Keep the new set in memory
		// rather than retrying the spent token, but report the persistence failure.
		c.creds = v
		return "", err
	}
	if !hasScope(v.Scopes, directScope) {
		return "", ErrPermission
	}
	return v.AccessToken, nil
}
