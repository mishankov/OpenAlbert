package integrations

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations/siwc"
)

type BillingStatus struct {
	Profiles       map[domain.ModelRole]ProfileRouteStatus `json:"profiles,omitempty"`
	PlanIneligible bool                                    `json:"plan_ineligible"`
	Mode           string                                  `json:"mode"`
	ActiveLane     string                                  `json:"active_lane"`
	SIWC           siwc.Status                             `json:"siwc"`
	SIWCRequests   uint64                                  `json:"siwc_requests"`
	APIRequests    uint64                                  `json:"api_key_requests"`
	RetryAt        *time.Time                              `json:"retry_at,omitempty"` // Local cooldown, never a claimed plan reset time.
	Last429        *time.Time                              `json:"last_429,omitempty"`
	LastErrorCode  string                                  `json:"last_error_code,omitempty"`
	LastRequestID  string                                  `json:"last_request_id,omitempty"`
	LastHTTPStatus int                                     `json:"last_http_status,omitempty"`
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
	StreamTimeout             time.Duration
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
	s := BillingStatus{PlanIneligible: b.ineligible, Mode: b.Mode, ActiveLane: "api_key", SIWCRequests: b.siwcRequests, APIRequests: b.apiRequests, Last429: b.last429, LastErrorCode: b.lastCode, LastRequestID: b.lastRequestID, LastHTTPStatus: b.lastStatus}
	planReady := !b.ineligible && !time.Now().Before(b.retryAt)
	if !b.ineligible && time.Now().Before(b.retryAt) {
		t := b.retryAt
		s.RetryAt = &t
	}
	b.mu.Unlock()
	// Reading account status may wait for token refresh. Do not hold the billing
	// lock: API profiles must keep running independently of that connection.
	if b.Plan != nil {
		s.SIWC = b.Plan.Status()
	}
	if s.Mode == "profiles" {
		s.ActiveLane = "profiles"
		if router, ok := b.API.(interface {
			ProfileRoutes() map[domain.ModelRole]ProfileRouteStatus
		}); ok {
			s.Profiles = router.ProfileRoutes()
		}
	}
	if s.Mode == "siwc" || (s.Mode == "auto" && s.SIWC.PlanEnabled && planReady) {
		s.ActiveLane = "siwc"
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
	if b.Mode == "profiles" {
		lane := ""
		if router, ok := b.API.(interface{ Lane(context.Context) string }); ok {
			lane = router.Lane(ctx)
		}
		b.mu.Lock()
		switch lane {
		case "siwc":
			b.siwcRequests++
		case "api_key":
			b.apiRequests++
		}
		b.mu.Unlock()
		result, err := b.API.Complete(ctx, messages, tools)
		if err != nil && lane == "siwc" {
			b.mu.Lock()
			b.recordPlanError(ctx, err)
			b.mu.Unlock()
			result.Message = domain.Message{}
		}
		return result, err
	}
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
		api := b.recordPlanError(ctx, err)
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

// Catalog exposes selectable models from the configured billing mode/router for coding clients.
func (b *BillingModel) Catalog(ctx context.Context, refresh bool) (string, []domain.CatalogModel, error) {
	if b.Mode == "siwc" && b.Plan != nil {
		defSlug := b.Plan.Status().Model
		models, err := b.Plan.Models(ctx)
		if err != nil && defSlug == "" {
			return "", nil, err
		}
		var out []domain.CatalogModel
		seen := map[string]bool{}
		for _, m := range models {
			if m.Slug == "" || (m.Visibility != "" && m.Visibility != "list") {
				continue
			}
			id := "chatgpt/" + m.Slug
			if seen[id] {
				continue
			}
			seen[id] = true
			name := m.DisplayName
			if name == "" {
				name = m.Slug
			}
			out = append(out, domain.CatalogModel{
				ID:                  id,
				Provider:            "chatgpt",
				Model:               m.Slug,
				DisplayName:         name,
				Capabilities:        []string{"tools", "vision", "structured_output"},
				ContextWindowTokens: 128000,
				MaxOutputTokens:     8192,
				Default:             m.Slug == defSlug,
			})
		}
		defaultID := ""
		if defSlug != "" {
			defaultID = "chatgpt/" + defSlug
			if !seen[defaultID] {
				out = append([]domain.CatalogModel{{
					ID:                  defaultID,
					Provider:            "chatgpt",
					Model:               defSlug,
					DisplayName:         defSlug,
					Capabilities:        []string{"tools", "vision", "structured_output"},
					ContextWindowTokens: 128000,
					MaxOutputTokens:     8192,
					Default:             true,
				}}, out...)
			}
		} else if len(out) > 0 {
			out[0].Default = true
			defaultID = out[0].ID
		}
		return defaultID, out, nil
	}
	if router, ok := b.API.(interface {
		Catalog(context.Context, bool) (string, []domain.CatalogModel, error)
	}); ok {
		def, models, err := router.Catalog(ctx, refresh)
		if err != nil {
			return def, models, err
		}
		// In auto mode while the ChatGPT plan lane is active, strip reasoning_effort
		// options because the plan lane cannot honor per-request reasoning overrides.
		if b.Mode == "auto" && b.Plan != nil {
			b.mu.Lock()
			planActive := !b.ineligible && !time.Now().Before(b.retryAt)
			b.mu.Unlock()
			if planActive {
				for i := range models {
					models[i].SupportedReasoningEfforts = nil
				}
			}
		}
		return def, models, nil
	}
	return "", nil, errors.New("model catalog is unavailable")
}

// CompleteModelStream streams a completion for a specifically selected model ID
// while respecting the configured billing mode (profiles, siwc, auto, api_key).
func (b *BillingModel) CompleteModelStream(ctx context.Context, modelID string, opts domain.ModelOptions, messages []domain.Message, tools []domain.Tool, onEvent domain.StreamFunc) (domain.Completion, error) {
	if b.Mode == "profiles" {
		router, ok := b.API.(interface {
			CompleteModelStream(context.Context, string, domain.ModelOptions, []domain.Message, []domain.Tool, domain.StreamFunc) (domain.Completion, error)
		})
		if !ok {
			return domain.Completion{}, errors.New("streaming model gateway is unavailable")
		}
		lane := ""
		if laner, ok := b.API.(interface{ ModelLane(string) string }); ok {
			lane = laner.ModelLane(modelID)
		}
		b.mu.Lock()
		switch lane {
		case "siwc":
			b.siwcRequests++
		case "api_key":
			b.apiRequests++
		}
		b.mu.Unlock()
		res, err := router.CompleteModelStream(ctx, modelID, opts, messages, tools, onEvent)
		if err != nil && lane == "siwc" {
			b.mu.Lock()
			b.recordPlanError(ctx, err)
			b.mu.Unlock()
			res.Message = domain.Message{}
		}
		return res, err
	}

	// Validate model selection and reasoning_effort overrides before attempting
	// either lane in auto mode.
	if b.Mode == "auto" {
		if validator, ok := b.API.(interface {
			ValidateModelOptions(string, domain.ModelOptions) error
		}); ok {
			if err := validator.ValidateModelOptions(modelID, opts); err != nil {
				return domain.Completion{}, err
			}
		}
	}

	b.mu.Lock()
	if b.Mode == "siwc" && (b.ineligible || time.Now().Before(b.retryAt)) {
		err := b.lastErr
		b.mu.Unlock()
		if err == nil {
			err = siwc.ErrDisconnected
		}
		return domain.Completion{}, err
	}
	usePlan := b.Mode == "siwc" || (b.Mode == "auto" && !b.ineligible && !time.Now().Before(b.retryAt))
	if usePlan && opts.ReasoningEffort != "" {
		b.mu.Unlock()
		return domain.Completion{}, errors.New("reasoning_effort override is not supported while the ChatGPT plan lane is active")
	}
	if usePlan {
		b.siwcRequests++
	}
	b.mu.Unlock()

	if usePlan {
		if b.Plan == nil {
			err := siwc.ErrDisconnected
			if b.Mode == "siwc" {
				return domain.Completion{}, err
			}
		} else {
			emitted := false
			wrappedOnEvent := func(ev domain.StreamEvent) error {
				emitted = true
				if onEvent != nil {
					return onEvent(ev)
				}
				return nil
			}
			result, err := b.streamViaPlan(ctx, modelID, messages, tools, wrappedOnEvent)
			if err == nil {
				return result, nil
			}
			b.mu.Lock()
			api := b.recordPlanError(ctx, err)
			canFallback := b.Mode == "auto" && fallback(err) && ctx.Err() == nil && !emitted
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
				return domain.Completion{}, err
			}
		}
	}

	router, ok := b.API.(interface {
		CompleteModelStream(context.Context, string, domain.ModelOptions, []domain.Message, []domain.Tool, domain.StreamFunc) (domain.Completion, error)
	})
	if !ok {
		return domain.Completion{}, errors.New("streaming model gateway is unavailable")
	}
	b.mu.Lock()
	b.apiRequests++
	b.mu.Unlock()
	return router.CompleteModelStream(ctx, modelID, opts, messages, tools, onEvent)
}

func (b *BillingModel) streamViaPlan(ctx context.Context, modelID string, messages []domain.Message, tools []domain.Tool, onEvent domain.StreamFunc) (domain.Completion, error) {
	timeout := b.StreamTimeout
	if timeout <= 0 {
		timeout = domain.MaxModelTimeout
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	slug := b.Plan.Status().Model
	if prefix, s, ok := strings.Cut(modelID, "/"); ok && prefix == "chatgpt" && s != "" {
		slug = s
	} else if slug == "" {
		if _, s, ok := strings.Cut(modelID, "/"); ok && s != "" {
			slug = s
		} else {
			slug = modelID
		}
	}
	if streamer, ok := b.Plan.(interface {
		CompleteForProfileStream(context.Context, domain.ModelProfile, []domain.Message, []domain.Tool, domain.StreamFunc) (domain.Completion, error)
	}); ok {
		return streamer.CompleteForProfileStream(ctx, domain.ModelProfile{Model: slug}, messages, tools, onEvent)
	}
	res, err := b.Plan.Complete(ctx, messages, tools)
	if err != nil {
		return domain.Completion{}, err
	}
	if onEvent != nil && res.Message.Content != "" {
		if err := onEvent(domain.StreamEvent{Type: "text_delta", Delta: res.Message.Content}); err != nil {
			return domain.Completion{}, err
		}
	}
	return res, nil
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

// recordPlanError records shared account diagnostics without changing routing.
// The caller holds b.mu. Only legacy modes apply cooldown or eligibility gates.
func (b *BillingModel) recordPlanError(ctx context.Context, err error) *siwc.APIError {
	var api *siwc.APIError
	if !errors.As(err, &api) {
		return nil
	}
	b.lastCode = api.Code
	b.lastRequestID = api.RequestID
	b.lastStatus = api.Status
	if api.Status == 429 || api.Code == "subscription_sharing_usage_limit_exceeded" {
		t := time.Now().UTC()
		b.last429 = &t
	}
	slog.WarnContext(ctx, "ChatGPT plan request failed", "http_status", api.Status, "error_code", api.Code, "error_param", api.Param, "request_id", api.RequestID)
	return api
}
