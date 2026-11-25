package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTPError preserves a safe status without retaining response bodies or credentials.
type HTTPError struct {
	Status            int
	Source            string
	RetryAfter        time.Duration
	RetryAfterPresent bool
	RetryAfterValid   bool
	ObservedAt        time.Time
}

type RequestError struct{ Cause error }

func (e *RequestError) Error() string { return "upstream request failed" }
func (e *RequestError) Unwrap() error { return e.Cause }

func (e *HTTPError) Error() string                     { return fmt.Sprintf("%s HTTP status %d", e.Source, e.Status) }
func (e *HTTPError) HTTPStatus() int                   { return e.Status }
func (e *HTTPError) RetryAfterDuration() time.Duration { return e.RetryAfter }
func (e *HTTPError) SearchCompletedAt() time.Time      { return e.ObservedAt }

const maxParsedRetryAfterSeconds = 86400 * 365

func isUnsignedASCIIInt(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func parseRetryAfterHeader(values []string, now time.Time) (delay time.Duration, present bool, valid bool) {
	if len(values) == 0 {
		return 0, false, false
	}
	if len(values) > 1 {
		return 0, true, false
	}
	return parseRetryAfterValue(values[0], now)
}

func parseRetryAfterValue(raw string, now time.Time) (delay time.Duration, present bool, valid bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true, false
	}
	if isUnsignedASCIIInt(raw) {
		secs, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || secs > maxParsedRetryAfterSeconds {
			secs = maxParsedRetryAfterSeconds
		}
		return time.Duration(secs) * time.Second, true, true
	}
	if ts, err := http.ParseTime(raw); err == nil {
		d := ts.Sub(now)
		if d < 0 {
			d = 0
		}
		maxDur := time.Duration(maxParsedRetryAfterSeconds) * time.Second
		if d > maxDur {
			d = maxDur
		}
		return d, true, true
	}
	return 0, true, false
}

func providerURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("provider endpoint must be an HTTP(S) URL without credentials, query or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func httpClient(c *http.Client, timeout time.Duration) *http.Client {
	if c == nil {
		c = &http.Client{}
	}
	copy := *c
	if copy.Timeout <= 0 {
		copy.Timeout = timeout
	}
	// Provider redirects must not disclose authentication headers or bot tokens.
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}
func request(ctx context.Context, c *http.Client, req *http.Request, out any, limit int64) error {
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &RequestError{Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var retryAfter time.Duration
		var retryPresent, retryValid bool
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			retryAfter, retryPresent, retryValid = parseRetryAfterHeader(resp.Header.Values("Retry-After"), time.Now())
		}
		return &HTTPError{
			Status:            resp.StatusCode,
			Source:            "upstream",
			RetryAfter:        retryAfter,
			RetryAfterPresent: retryPresent,
			RetryAfterValid:   retryValid,
		}
	}
	b, err := limitedRead(resp.Body, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errors.New("invalid upstream JSON response")
	}
	return nil
}
func limitedRead(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, errors.New("upstream body read failed")
	}
	if int64(len(b)) > limit {
		return nil, errors.New("upstream response exceeds size limit")
	}
	return b, nil
}
