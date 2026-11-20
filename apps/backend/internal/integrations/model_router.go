package integrations

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations/siwc"
)

// SubscriptionModel shares account authentication while keeping model selection request-local.
type SubscriptionModel interface {
	CompleteForModel(context.Context, string, []domain.Message, []domain.Tool) (domain.Completion, error)
}

type ProfileRouteStatus struct {
	Profile  string `json:"profile"`
	Model    string `json:"model"`
	AuthMode string `json:"auth_mode"`
}

type routedModel struct {
	profile domain.ModelProfile
	client  *OpenAI
}

// ModelRouter binds application roles to immutable configured profiles. It never
// switches models or repeats a failed request on another profile.
type ModelRouter struct {
	routes map[domain.ModelRole]string
	models map[string]routedModel
	plan   SubscriptionModel
}

func NewModelRouter(config domain.ModelProfiles) (*ModelRouter, error) {
	return NewModelRouterWithPlan(config, nil)
}

func NewModelRouterWithPlan(config domain.ModelProfiles, plan SubscriptionModel) (*ModelRouter, error) {
	r := &ModelRouter{routes: make(map[domain.ModelRole]string), models: make(map[string]routedModel), plan: plan}
	for name, p := range config.Profiles {
		if name == "" || (p.Adapter != "openai_chat_completions" && p.Adapter != "openai_chatgpt") || strings.TrimSpace(p.Model) == "" || p.ContextWindowTokens <= 0 || p.MaxOutputTokens <= 0 || p.MaxOutputTokens >= p.ContextWindowTokens || p.Timeout <= 0 || p.Timeout > domain.MaxModelTimeout {
			return nil, errors.New("invalid model profile")
		}
		if p.Pricing != nil {
			prices := *p.Pricing
			for _, v := range []float64{prices.InputPerMillion, prices.CachedInputPerMillion, prices.OutputPerMillion} {
				if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
					return nil, errors.New("invalid model pricing")
				}
			}
			p.Pricing = &prices
		}
		p.Capabilities = append([]string(nil), p.Capabilities...)
		var client *OpenAI
		if p.Adapter == "openai_chatgpt" {
			if plan == nil {
				return nil, errors.New("ChatGPT model profiles require a subscription client")
			}
			if p.BaseURL != "" || p.APIKey != "" || p.APIKeyEnv != "" || p.Options != (domain.ModelOptions{}) || p.Pricing != nil {
				return nil, errors.New("ChatGPT model profiles do not support API fields, options or pricing")
			}
		} else {
			var err error
			client, err = NewOpenAI(OpenAIConfig{BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model, Timeout: p.Timeout, MaxOutputTokens: p.MaxOutputTokens, Options: p.Options})
			if err != nil {
				return nil, err
			}
		}
		r.models[name] = routedModel{profile: p, client: client}
	}
	for role, name := range config.Routes {
		if role != domain.RoleChat && role != domain.RoleBackground && role != domain.RoleAnalysis {
			return nil, errors.New("unknown model role")
		}
		model, ok := r.models[name]
		if !ok {
			return nil, errors.New("model route references an undefined profile")
		}
		capability := "tools"
		if role == domain.RoleAnalysis {
			capability = "structured_output"
		}
		if !hasCapability(model.profile, capability) {
			return nil, fmt.Errorf("model route %s requires %s capability", role, capability)
		}
		r.routes[role] = name
	}
	for _, role := range []domain.ModelRole{domain.RoleChat, domain.RoleBackground, domain.RoleAnalysis} {
		if r.routes[role] == "" {
			return nil, fmt.Errorf("missing model route %s", role)
		}
	}
	return r, nil
}

func hasCapability(profile domain.ModelProfile, capability string) bool {
	for _, c := range profile.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

func (r *ModelRouter) Complete(ctx context.Context, messages []domain.Message, tools []domain.Tool) (out domain.Completion, err error) {
	role := domain.ModelRoleFromContext(ctx)
	name, ok := r.routes[role]
	if !ok {
		return out, errors.New("model call requires an explicitly configured role")
	}
	model := r.models[name]
	out.Usage = domain.ModelUsage{Role: role, Profile: name, Model: model.profile.Model}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if len(tools) > 0 && !hasCapability(model.profile, "tools") {
		return out, errors.New("model profile does not support tools")
	}
	for _, message := range messages {
		if message.ContextOverflow {
			return out, errors.New("required current-turn input exceeds the Temporal payload budget")
		}
		if (len(message.ToolCalls) > 0 || message.Role == "tool") && !hasCapability(model.profile, "tools") {
			return out, errors.New("model profile does not support tool history")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, model.profile.Timeout)
	defer cancel()
	estimate, complete := r.request(model, role)
	prepared := append([]domain.Message(nil), messages...)
	protected := domain.ModelTurnStart(prepared, domain.ModelTurn(ctx))
	if !hasCapability(model.profile, "vision") {
		for {
			image := -1
			for i, m := range prepared {
				if len(m.Images) > 0 {
					image = i
					break
				}
			}
			if image < 0 {
				break
			}
			if image >= protected {
				return out, errors.New("model profile does not support images")
			}
			offset := 0
			for {
				start, end := domain.OldestModelTurn(prepared[offset:], protected-offset)
				if start == end || image < offset+start {
					return out, errors.New("model profile does not support images")
				}
				start, end = offset+start, offset+end
				if image < end {
					prepared = append(prepared[:start], prepared[end:]...)
					protected -= end - start
					break
				}
				offset = end
			}
		}
	}
	var fitted []domain.Message
	fitted, out.Usage.EstimatedInputTokens, err = fitModelContextWithEstimator(ctx, estimate, prepared, tools, model.profile.ContextWindowTokens-model.profile.MaxOutputTokens)
	if err != nil {
		return out, err
	}
	out.Usage.DroppedMessages = len(messages) - len(fitted)
	completion, err := complete(ctx, fitted, tools)
	if err != nil {
		completion.Message = domain.Message{}
	}
	out.Model, out.Message, out.InputTokens, out.OutputTokens = completion.Model, completion.Message, completion.InputTokens, completion.OutputTokens
	if completion.Usage.Model != "" {
		out.Usage.Model = completion.Usage.Model
	}
	out.Usage.CachedInputTokens = completion.Usage.CachedInputTokens
	out.Usage.TokenUsageKnown = completion.Usage.TokenUsageKnown
	if price := model.profile.Pricing; model.profile.Adapter == "openai_chat_completions" && price != nil && out.Usage.TokenUsageKnown {
		cost := (float64(out.InputTokens-out.Usage.CachedInputTokens)*price.InputPerMillion + float64(out.Usage.CachedInputTokens)*price.CachedInputPerMillion + float64(out.OutputTokens)*price.OutputPerMillion) / 1_000_000
		if !math.IsInf(cost, 0) && !math.IsNaN(cost) {
			out.Usage.EstimatedCostUSD = &cost
		}
	}
	return out, err
}

// fitModelContext uses serialized UTF-8 bytes as a conservative token estimate.
// This deliberately overestimates ordinary text and inline images; it is not a
// provider tokenizer. Framing overhead and the output reservation remain bounded
// separately. Never shorten instructions, current-turn evidence or image data.
func fitModelContext(ctx context.Context, client *OpenAI, messages []domain.Message, tools []domain.Tool, budget int) ([]domain.Message, int, error) {
	return fitModelContextWithEstimator(ctx, func(messages []domain.Message, tools []domain.Tool) (int, error) {
		return estimateModelInput(client, messages, tools)
	}, messages, tools, budget)
}

type inputEstimator func([]domain.Message, []domain.Tool) (int, error)

func fitModelContextWithEstimator(ctx context.Context, estimateInput inputEstimator, messages []domain.Message, tools []domain.Tool, budget int) ([]domain.Message, int, error) {
	out := append([]domain.Message(nil), messages...)
	protected := domain.ModelTurnStart(out, domain.ModelTurn(ctx))
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		estimate, err := estimateInput(out, tools)
		if err != nil {
			return nil, 0, err
		}
		if estimate <= budget {
			return out, estimate, nil
		}
		start, end := domain.OldestModelTurn(out, protected)
		if start == end {
			return nil, estimate, errors.New("required model input exceeds the profile context budget")
		}
		out = append(out[:start], out[end:]...)
		protected -= end - start
	}
}

var _ domain.Model = (*ModelRouter)(nil)

func estimateModelInput(client *OpenAI, messages []domain.Message, tools []domain.Tool) (int, error) {
	data, err := client.requestData(messages, tools)
	return len(data) + len(messages)*64 + len(tools)*128 + 256, err
}

// FitsContext lets structured callers pack complete records before serializing
// a protected request. It uses the same estimate and output reserve as Complete.
func (r *ModelRouter) FitsContext(ctx context.Context, messages []domain.Message, tools []domain.Tool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	role := domain.ModelRoleFromContext(ctx)
	name, ok := r.routes[role]
	if !ok {
		return false, errors.New("model call requires an explicitly configured role")
	}
	model := r.models[name]
	estimateInput, _ := r.request(model, role)
	estimate, err := estimateInput(messages, tools)
	return estimate <= model.profile.ContextWindowTokens-model.profile.MaxOutputTokens, err
}

// request creates per-call behavior without mutating either shared transport.
func (r *ModelRouter) request(model routedModel, role domain.ModelRole) (inputEstimator, func(context.Context, []domain.Message, []domain.Tool) (domain.Completion, error)) {
	if model.profile.Adapter == "openai_chatgpt" {
		return func(messages []domain.Message, tools []domain.Tool) (int, error) {
				return siwc.EstimateInput(model.profile.Model, messages, tools)
			}, func(ctx context.Context, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
				return r.plan.CompleteForModel(ctx, model.profile.Model, messages, tools)
			}
	}
	client := *model.client
	client.config.JSONOutput = role == domain.RoleAnalysis
	return func(messages []domain.Message, tools []domain.Tool) (int, error) {
		return estimateModelInput(&client, messages, tools)
	}, client.Complete
}

// Lane identifies the configured authentication route, never an account-wide override.
func (r *ModelRouter) Lane(ctx context.Context) string {
	name, ok := r.routes[domain.ModelRoleFromContext(ctx)]
	if !ok {
		return ""
	}
	if r.models[name].profile.Adapter == "openai_chatgpt" {
		return "siwc"
	}
	return "api_key"
}

func (r *ModelRouter) ProfileRoutes() map[domain.ModelRole]ProfileRouteStatus {
	out := make(map[domain.ModelRole]ProfileRouteStatus, len(r.routes))
	for role, name := range r.routes {
		out[role] = ProfileRouteStatus{Profile: name, Model: r.models[name].profile.Model, AuthMode: r.Lane(domain.WithModelRole(context.Background(), role))}
	}
	return out
}
