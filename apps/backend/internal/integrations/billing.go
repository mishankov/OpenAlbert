package integrations

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations/siwc"
)

type BillingStatus struct {
	PlanIneligible bool        `json:"plan_ineligible"`
	Mode           string      `json:"mode"`
	ActiveLane     string      `json:"active_lane"`
	SIWC           siwc.Status `json:"siwc"`
	SIWCRequests   uint64      `json:"siwc_requests"`
	APIRequests    uint64      `json:"api_key_requests"`
	RetryAt        *time.Time  `json:"retry_at,omitempty"` // Local cooldown, never a claimed plan reset time.
	Last429        *time.Time  `json:"last_429,omitempty"`
	LastErrorCode  string      `json:"last_error_code,omitempty"`
	LastRequestID  string      `json:"last_request_id,omitempty"`
	LastHTTPStatus int         `json:"last_http_status,omitempty"`
}
type PlanClient interface {
	domain.Model
	Status() siwc.Status
	Begin() (string, error)
	Finish(context.Context, string) error
	Models(context.Context) ([]siwc.Model, error)
	SetModel(context.Context, string) error
	Close() error
}
type BillingModel struct {
	Mode                      string
	Plan                      PlanClient
	API                       domain.Model
	Cooldown                  time.Duration
	mu                        sync.Mutex
	retryAt                   time.Time
	lastErr                   error
	ineligible                bool
	last429                   *time.Time
	siwcRequests, apiRequests uint64
	lastCode, lastRequestID   string
	lastStatus                int
}

func (b *BillingModel) Status() BillingStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := BillingStatus{PlanIneligible: b.ineligible, Mode: b.Mode, ActiveLane: "api_key", SIWCRequests: b.siwcRequests, APIRequests: b.apiRequests, Last429: b.last429, LastErrorCode: b.lastCode, LastRequestID: b.lastRequestID, LastHTTPStatus: b.lastStatus}
	if b.Plan != nil {
		s.SIWC = b.Plan.Status()
	}
	if b.Mode == "siwc" || (b.Mode == "auto" && s.SIWC.PlanEnabled && !b.ineligible && !time.Now().Before(b.retryAt)) {
		s.ActiveLane = "siwc"
	}
	if !b.ineligible && time.Now().Before(b.retryAt) {
		t := b.retryAt
		s.RetryAt = &t
	}
	return s
}
func fallback(err error) bool {
	if errors.Is(err, siwc.ErrDisconnected) || errors.Is(err, siwc.ErrPermission) {
		return true
	}
	var api *siwc.APIError
	if !errors.As(err, &api) {
		return false
	}
	switch api.Code {
	case "subscription_sharing_usage_limit_exceeded", "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable", "subscription_sharing_user_not_eligible":
		return true
	case "":
		return api.Status == 429 || api.Status == 403 || api.Status == 503
	}
	return false // Configuration/unsupported capability errors require a fix, not a billing switch.
}
func (b *BillingModel) Complete(ctx context.Context, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	b.mu.Lock()
	if b.Mode == "siwc" && (b.ineligible || time.Now().Before(b.retryAt)) {
		err := b.lastErr
		b.mu.Unlock()
		return domain.Completion{}, err
	}
	usePlan := b.Mode == "siwc" || (b.Mode == "auto" && !b.ineligible && !time.Now().Before(b.retryAt))
	if usePlan {
		b.siwcRequests++
	}
	b.mu.Unlock()
	if usePlan {
		result, err := b.Plan.Complete(ctx, messages, tools)
		if err == nil {
			return result, nil
		}
		b.mu.Lock()
		var api *siwc.APIError
		if errors.As(err, &api) {
			b.lastCode = api.Code
			b.lastRequestID = api.RequestID
			b.lastStatus = api.Status
			if api.Status == 429 || api.Code == "subscription_sharing_usage_limit_exceeded" {
				t := time.Now().UTC()
				b.last429 = &t
			}
			slog.WarnContext(ctx, "ChatGPT plan request failed", "http_status", api.Status, "error_code", api.Code, "error_param", api.Param, "request_id", api.RequestID)
		}
		canFallback := b.Mode == "auto" && fallback(err) && ctx.Err() == nil
		if fallback(err) && ctx.Err() == nil {
			b.lastErr = err
			cooldown := b.Cooldown
			if cooldown <= 0 {
				cooldown = 15 * time.Minute
			}
			b.retryAt = time.Now().Add(cooldown)
			if api != nil && api.Code == "subscription_sharing_user_not_eligible" {
				b.ineligible = true
			}
		}
		b.mu.Unlock()
		if !canFallback {
			result.Message = domain.Message{}
			return result, err
		}
	}
	b.mu.Lock()
	b.apiRequests++
	b.mu.Unlock()
	return b.API.Complete(ctx, messages, tools)
}

var _ domain.Model = (*BillingModel)(nil)

// A completed, explicit sign-in is the point at which a previous grant restriction may be reevaluated.
func (b *BillingModel) Reconnected() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.retryAt = time.Time{}
	b.ineligible = false
	b.lastErr = nil
}

// Keep auto-lane requests within the API fallback budget. Pure plan mode uses
// an independent local payload bound because it never calls the API profile.
func (b *BillingModel) FitsContext(ctx context.Context, messages []domain.Message, tools []domain.Tool) (bool, error) {
	if b.Mode == "siwc" {
		return siwc.FitsContext(ctx, b.Plan.Status().Model, messages, tools)
	}
	if sizer, ok := b.API.(domain.ModelContext); ok {
		return sizer.FitsContext(ctx, messages, tools)
	}
	return true, ctx.Err()
}
