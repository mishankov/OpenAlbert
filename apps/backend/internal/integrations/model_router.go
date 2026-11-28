package integrations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations/siwc"
)

// SubscriptionModel shares account authentication while keeping model selection request-local.
type SubscriptionModel interface {
	CompleteForProfile(context.Context, domain.ModelProfile, []domain.Message, []domain.Tool) (domain.Completion, error)
}

type streamingSubscriptionModel interface {
	CompleteForProfileStream(context.Context, domain.ModelProfile, []domain.Message, []domain.Tool, domain.StreamFunc) (domain.Completion, error)
}

type catalogSubscriptionModel interface {
	Models(context.Context) ([]siwc.Model, error)
}

type catalogRefreshSubscriptionModel interface {
	ModelsFresh(context.Context, bool) ([]siwc.Model, error)
}

type catalogProfileSubscriptionModel interface {
	ModelsForProfile(context.Context, domain.ModelProfile, bool) ([]siwc.Model, error)
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

type cachedProviderModels struct {
	models  []DiscoveredModel
	expires time.Time
}

// ModelRouter binds application roles to immutable configured profiles and
// resolves selectable models across configured providers for coding sessions.
type ModelRouter struct {
	routes    map[domain.ModelRole]string
	providers map[string]domain.ModelProvider
	overrides map[string]domain.ModelProfile
	models    map[string]routedModel
	plan      SubscriptionModel

	mu           sync.Mutex
	discoveryTTL time.Duration
	discovered   map[string]cachedProviderModels
}

func NewModelRouter(config domain.ModelProfiles) (*ModelRouter, error) {
	return NewModelRouterWithPlan(config, nil)
}

func NewModelRouterWithPlan(config domain.ModelProfiles, plan SubscriptionModel) (*ModelRouter, error) {
	r := &ModelRouter{
		routes:       make(map[domain.ModelRole]string),
		providers:    make(map[string]domain.ModelProvider),
		overrides:    make(map[string]domain.ModelProfile),
		models:       make(map[string]routedModel),
		plan:         plan,
		discoveryTTL: 5 * time.Minute,
		discovered:   make(map[string]cachedProviderModels),
	}
	for name, prov := range config.Providers {
		if name == "" || (prov.Adapter != "openai_chat_completions" && prov.Adapter != "openai_chatgpt") || prov.DefaultContextWindowTokens <= 0 || prov.DefaultMaxOutputTokens <= 0 || prov.DefaultMaxOutputTokens >= prov.DefaultContextWindowTokens || prov.DefaultTimeout <= 0 || prov.DefaultTimeout > domain.MaxModelTimeout {
			return nil, errors.New("invalid model provider")
		}
		if prov.Adapter == "openai_chatgpt" && plan == nil {
			return nil, errors.New("ChatGPT model providers require a subscription client")
		}
		prov.Name = name
		prov.DefaultCapabilities = append([]string(nil), prov.DefaultCapabilities...)
		prov.IncludeModels = append([]string(nil), prov.IncludeModels...)
		prov.ExcludeModels = append([]string(nil), prov.ExcludeModels...)
		prov.Headers = maps.Clone(prov.Headers)
		if prov.DefaultPricing != nil {
			p := *prov.DefaultPricing
			prov.DefaultPricing = &p
		}
		r.providers[name] = prov
	}
	for key, m := range config.Models {
		r.overrides[key] = cloneProfile(m)
	}
	for name, p := range config.Profiles {
		rm, err := buildRoutedModel(p, plan)
		if err != nil {
			return nil, err
		}
		r.models[name] = rm
	}
	for role, name := range config.Routes {
		if role != domain.RoleChat && role != domain.RoleBackground && role != domain.RoleAnalysis && role != domain.RoleCoding {
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

func cloneProfile(p domain.ModelProfile) domain.ModelProfile {
	p.Capabilities = append([]string(nil), p.Capabilities...)
	p.SupportedReasoningEfforts = append([]string(nil), p.SupportedReasoningEfforts...)
	p.Headers = maps.Clone(p.Headers)
	if p.Pricing != nil {
		cp := *p.Pricing
		p.Pricing = &cp
	}
	return p
}

func buildRoutedModel(p domain.ModelProfile, plan SubscriptionModel) (routedModel, error) {
	if (p.Adapter != "openai_chat_completions" && p.Adapter != "openai_chatgpt") || strings.TrimSpace(p.Model) == "" || p.ContextWindowTokens <= 0 || p.MaxOutputTokens <= 0 || p.MaxOutputTokens >= p.ContextWindowTokens || p.Timeout <= 0 || p.Timeout > domain.MaxModelTimeout {
		return routedModel{}, errors.New("invalid model profile")
	}
	if p.Pricing != nil {
		prices := *p.Pricing
		for _, v := range []float64{prices.InputPerMillion, prices.CachedInputPerMillion, prices.OutputPerMillion} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return routedModel{}, errors.New("invalid model pricing")
			}
		}
		p.Pricing = &prices
	}
	p = cloneProfile(p)
	var client *OpenAI
	if p.Adapter == "openai_chatgpt" {
		if plan == nil {
			return routedModel{}, errors.New("ChatGPT model profiles require a subscription client")
		}
		if p.APIKey != "" || p.APIKeyEnv != "" || p.Pricing != nil ||
			p.Options.Thinking != "" || p.Options.IncludeReasoningContent || p.Options.SessionHeader != "" || p.Options.OutputTokenField != "" {
			return routedModel{}, errors.New("ChatGPT model profiles do not support API fields, Chat Completions options or pricing")
		}
	} else {
		var err error
		client, err = NewOpenAI(OpenAIConfig{BaseURL: p.BaseURL, Headers: p.Headers, APIKey: p.APIKey, Model: p.Model, Timeout: p.Timeout, MaxOutputTokens: p.MaxOutputTokens, Options: p.Options})
		if err != nil {
			return routedModel{}, err
		}
	}
	return routedModel{profile: p, client: client}, nil
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
	return r.completeRouted(ctx, role, name, model, messages, tools, nil)
}

// CompleteModelStream invokes a specific selectable model ("<provider>/<model>"
// or static profile ID) with optional per-request options and streams events.
func (r *ModelRouter) CompleteModelStream(ctx context.Context, modelID string, optOverrides domain.ModelOptions, messages []domain.Message, tools []domain.Tool, onEvent domain.StreamFunc) (domain.Completion, error) {
	if strings.TrimSpace(modelID) == "" {
		modelID = r.routes[domain.RoleCoding]
		if modelID == "" {
			modelID = r.routes[domain.RoleChat]
		}
	}
	model, err := r.resolveDynamicModel(modelID, optOverrides)
	if err != nil {
		return domain.Completion{}, err
	}
	role := domain.ModelRoleFromContext(ctx)
	if role == "" {
		role = domain.RoleCoding
	}
	return r.completeRouted(ctx, role, modelID, model, messages, tools, onEvent)
}

var defaultAPIReasoningEfforts = []string{"low", "medium", "high", "xhigh", "max"}

func supportedReasoningEffortsForProfile(p domain.ModelProfile) []string {
	if len(p.SupportedReasoningEfforts) > 0 {
		return append([]string(nil), p.SupportedReasoningEfforts...)
	}
	efforts := append([]string(nil), defaultAPIReasoningEfforts...)
	if p.Options.ReasoningEffort != "" {
		found := false
		for _, e := range efforts {
			if e == p.Options.ReasoningEffort {
				found = true
				break
			}
		}
		if !found {
			efforts = append(efforts, p.Options.ReasoningEffort)
		}
	}
	return efforts
}

// ModelLane reports whether a selected model ID routes through "siwc" or "api_key".
func (r *ModelRouter) ModelLane(modelID string) string {
	if modelID == "" {
		modelID = r.routes[domain.RoleCoding]
		if modelID == "" {
			modelID = r.routes[domain.RoleChat]
		}
	}
	if rm, ok := r.models[modelID]; ok {
		if rm.profile.Adapter == "openai_chatgpt" {
			return "siwc"
		}
		return "api_key"
	}
	if providerName, _, ok := strings.Cut(modelID, "/"); ok {
		if prov, exists := r.providers[providerName]; exists {
			if prov.Adapter == "openai_chatgpt" {
				return "siwc"
			}
			return "api_key"
		}
	}
	return ""
}

// ValidateModelOptions checks whether modelID and optOverrides are permitted by
// the router configuration before dispatching to any billing lane.
func (r *ModelRouter) ValidateModelOptions(modelID string, optOverrides domain.ModelOptions) error {
	if modelID == "" {
		modelID = r.routes[domain.RoleCoding]
		if modelID == "" {
			modelID = r.routes[domain.RoleChat]
		}
	}
	_, err := r.resolveDynamicModel(modelID, optOverrides)
	return err
}

func (r *ModelRouter) resolveDynamicModel(modelID string, optOverrides domain.ModelOptions) (routedModel, error) {
	var profile domain.ModelProfile
	var found bool
	if rm, ok := r.models[modelID]; ok {
		profile = cloneProfile(rm.profile)
		found = true
	} else if ov, ok := r.overrides[modelID]; ok {
		profile = cloneProfile(ov)
		found = true
	} else if providerName, slug, ok := strings.Cut(modelID, "/"); ok && providerName != "" && slug != "" {
		if prov, exists := r.providers[providerName]; exists && prov.DiscoverModels && matchesProviderFilters(prov, slug) {
			var pricing *domain.ModelPricing
			if prov.DefaultPricing != nil {
				p := *prov.DefaultPricing
				pricing = &p
			}
			profile = domain.ModelProfile{
				Provider:            prov.Name,
				Adapter:             prov.Adapter,
				BaseURL:             prov.BaseURL,
				Headers:             maps.Clone(prov.Headers),
				Model:               slug,
				DisplayName:         slug,
				APIKeyEnv:           prov.APIKeyEnv,
				APIKey:              prov.APIKey,
				Capabilities:        append([]string(nil), prov.DefaultCapabilities...),
				ContextWindowTokens: prov.DefaultContextWindowTokens,
				MaxOutputTokens:     prov.DefaultMaxOutputTokens,
				Timeout:             prov.DefaultTimeout,
				Options:             prov.DefaultOptions,
				Pricing:             pricing,
			}
			if discovered := r.findCachedDiscovered(providerName, slug); discovered != nil {
				if discovered.ContextWindowTokens > profile.MaxOutputTokens {
					profile.ContextWindowTokens = discovered.ContextWindowTokens
				}
				if discovered.MaxOutputTokens > 0 && discovered.MaxOutputTokens < profile.ContextWindowTokens {
					profile.MaxOutputTokens = discovered.MaxOutputTokens
				}
				if len(discovered.SupportedReasoningEfforts) > 0 {
					profile.SupportedReasoningEfforts = append([]string(nil), discovered.SupportedReasoningEfforts...)
				}
			}
			found = true
		}
	}
	if !found {
		return routedModel{}, errors.New("requested model is not configured or allowed")
	}
	if len(profile.SupportedReasoningEfforts) == 0 {
		if providerName, slug, ok := strings.Cut(modelID, "/"); ok && providerName != "" && slug != "" {
			if discovered := r.findCachedDiscovered(providerName, slug); discovered != nil && len(discovered.SupportedReasoningEfforts) > 0 {
				profile.SupportedReasoningEfforts = append([]string(nil), discovered.SupportedReasoningEfforts...)
			}
		}
	}
	if optOverrides.ReasoningEffort != "" {
		allowed := supportedReasoningEffortsForProfile(profile)
		if len(allowed) == 0 {
			return routedModel{}, errors.New("reasoning_effort override is not supported on this model")
		}
		permitted := false
		for _, effort := range allowed {
			if effort == optOverrides.ReasoningEffort {
				permitted = true
				break
			}
		}
		if !permitted {
			return routedModel{}, errors.New("unsupported reasoning_effort override for selected model")
		}
		profile.Options.ReasoningEffort = optOverrides.ReasoningEffort
	}
	return buildRoutedModel(profile, r.plan)
}

func (r *ModelRouter) findCachedDiscovered(providerName, slug string) *DiscoveredModel {
	r.mu.Lock()
	defer r.mu.Unlock()
	cached, ok := r.discovered[providerName]
	if !ok {
		return nil
	}
	for i := range cached.models {
		if cached.models[i].ID == slug {
			cp := cached.models[i]
			return &cp
		}
	}
	return nil
}

func matchesProviderFilters(prov domain.ModelProvider, modelID string) bool {
	if len(prov.IncludeModels) > 0 {
		matched := false
		for _, pattern := range prov.IncludeModels {
			if ok, _ := path.Match(pattern, modelID); ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, pattern := range prov.ExcludeModels {
		if ok, _ := path.Match(pattern, modelID); ok {
			return false
		}
	}
	return true
}

// Catalog returns the selectable tools-capable models across all configured
// providers (combining cached /models discovery with static overrides/routes).
func (r *ModelRouter) Catalog(ctx context.Context, refresh bool) (string, []domain.CatalogModel, error) {
	defaultID := r.routes[domain.RoleCoding]
	if defaultID == "" {
		defaultID = r.routes[domain.RoleChat]
	}
	byID := map[string]domain.CatalogModel{}
	configuredIDs := map[string]bool{}
	explicitEfforts := map[string]bool{}
	profileOptions := map[string]domain.ModelOptions{}
	addProfile := func(id string, p domain.ModelProfile) {
		configuredIDs[id] = true
		if len(p.SupportedReasoningEfforts) > 0 {
			explicitEfforts[id] = true
		}
		profileOptions[id] = p.Options
		if !hasCapability(p, "tools") {
			delete(byID, id)
			return
		}
		display := p.DisplayName
		if display == "" {
			display = p.Model
		}
		provider := p.Provider
		if provider == "" {
			if prefix, _, ok := strings.Cut(id, "/"); ok {
				provider = prefix
			}
		}
		byID[id] = domain.CatalogModel{
			ID:                        id,
			Provider:                  provider,
			Model:                     p.Model,
			DisplayName:               display,
			Capabilities:              append([]string(nil), p.Capabilities...),
			ContextWindowTokens:       p.ContextWindowTokens,
			MaxOutputTokens:           p.MaxOutputTokens,
			SupportedReasoningEfforts: supportedReasoningEffortsForProfile(p),
			Default:                   id == defaultID,
		}
	}
	for id, rm := range r.models {
		addProfile(id, rm.profile)
	}
	for id, ov := range r.overrides {
		addProfile(id, ov)
	}

	providerNames := make([]string, 0, len(r.providers))
	for name := range r.providers {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)

	var firstErr error
	for _, name := range providerNames {
		prov := r.providers[name]
		if !prov.DiscoverModels {
			continue
		}
		discovered, err := r.discoverProviderModels(ctx, prov, refresh)
		if err != nil {
			slog.WarnContext(ctx, "model discovery failed for provider", "provider", name, "error", err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, item := range discovered {
			if !matchesProviderFilters(prov, item.ID) {
				continue
			}
			fullID := name + "/" + item.ID
			if existing, ok := byID[fullID]; ok {
				if existing.DisplayName == existing.Model && item.DisplayName != "" {
					existing.DisplayName = item.DisplayName
				}
				if !explicitEfforts[fullID] && len(item.SupportedReasoningEfforts) > 0 {
					existing.SupportedReasoningEfforts = supportedReasoningEffortsForProfile(domain.ModelProfile{
						Adapter:                   prov.Adapter,
						Options:                   profileOptions[fullID],
						SupportedReasoningEfforts: item.SupportedReasoningEfforts,
					})
				}
				byID[fullID] = existing
				continue
			}
			// Never re-advertise a model whose explicit profile/override disabled tools.
			if configuredIDs[fullID] {
				continue
			}
			if !hasCapability(domain.ModelProfile{Capabilities: prov.DefaultCapabilities}, "tools") {
				continue
			}
			ctxWin := prov.DefaultContextWindowTokens
			if item.ContextWindowTokens > prov.DefaultMaxOutputTokens {
				ctxWin = item.ContextWindowTokens
			}
			maxOut := prov.DefaultMaxOutputTokens
			if item.MaxOutputTokens > 0 && item.MaxOutputTokens < ctxWin {
				maxOut = item.MaxOutputTokens
			}
			display := item.DisplayName
			if display == "" {
				display = item.ID
			}
			efforts := supportedReasoningEffortsForProfile(domain.ModelProfile{
				Adapter:                   prov.Adapter,
				Options:                   prov.DefaultOptions,
				SupportedReasoningEfforts: item.SupportedReasoningEfforts,
			})
			byID[fullID] = domain.CatalogModel{
				ID:                        fullID,
				Provider:                  name,
				Model:                     item.ID,
				DisplayName:               display,
				Capabilities:              append([]string(nil), prov.DefaultCapabilities...),
				ContextWindowTokens:       ctxWin,
				MaxOutputTokens:           maxOut,
				SupportedReasoningEfforts: efforts,
				Default:                   fullID == defaultID,
			}
		}
	}
	out := make([]domain.CatalogModel, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].ID < out[j].ID
	})
	if len(out) == 0 && firstErr != nil {
		return defaultID, nil, firstErr
	}
	return defaultID, out, nil
}

func (r *ModelRouter) discoverProviderModels(ctx context.Context, prov domain.ModelProvider, refresh bool) ([]DiscoveredModel, error) {
	if !refresh {
		r.mu.Lock()
		cached, ok := r.discovered[prov.Name]
		r.mu.Unlock()
		if ok && time.Now().Before(cached.expires) {
			return append([]DiscoveredModel(nil), cached.models...), nil
		}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, min(prov.DefaultTimeout, 15*time.Second))
	defer cancel()
	var list []DiscoveredModel
	if prov.Adapter == "openai_chatgpt" {
		if r.plan == nil {
			return nil, errors.New("ChatGPT catalog client unavailable")
		}
		var (
			models []siwc.Model
			err    error
		)
		if perProfile, ok := r.plan.(catalogProfileSubscriptionModel); ok {
			models, err = perProfile.ModelsForProfile(fetchCtx, domain.ModelProfile{
				Provider: prov.Name,
				Adapter:  prov.Adapter,
				BaseURL:  prov.BaseURL,
				Headers:  maps.Clone(prov.Headers),
			}, refresh)
		} else if refresher, ok := r.plan.(catalogRefreshSubscriptionModel); ok {
			models, err = refresher.ModelsFresh(fetchCtx, refresh)
		} else if cataloger, ok := r.plan.(catalogSubscriptionModel); ok {
			models, err = cataloger.Models(fetchCtx)
		} else {
			return nil, errors.New("ChatGPT catalog client unavailable")
		}
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			list = append(list, DiscoveredModel{
				ID:                        m.Slug,
				DisplayName:               m.DisplayName,
				SupportedReasoningEfforts: append([]string(nil), m.SupportedReasoningLevels...),
			})
		}
	} else {
		client, err := NewOpenAI(OpenAIConfig{
			BaseURL: prov.BaseURL, Headers: prov.Headers, APIKey: prov.APIKey,
			Model: "catalog", Timeout: min(prov.DefaultTimeout, 15*time.Second), Options: prov.DefaultOptions,
		})
		if err != nil {
			return nil, err
		}
		list, err = client.ListModels(fetchCtx)
		if err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	r.discovered[prov.Name] = cachedProviderModels{models: append([]DiscoveredModel(nil), list...), expires: time.Now().Add(r.discoveryTTL)}
	r.mu.Unlock()
	return list, nil
}

func (r *ModelRouter) completeRouted(ctx context.Context, role domain.ModelRole, name string, model routedModel, messages []domain.Message, tools []domain.Tool, onEvent domain.StreamFunc) (out domain.Completion, err error) {
	out.Usage = domain.ModelUsage{Role: role, Profile: name, Model: model.profile.Model}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if len(tools) > 0 && !hasCapability(model.profile, "tools") {
		return out, errors.New("model profile does not support tools")
	}
	for _, message := range messages {
		if message.ContextOverflow {
			return out, errors.New("required current-turn input exceeds the context payload budget")
		}
		if (len(message.ToolCalls) > 0 || message.Role == "tool") && !hasCapability(model.profile, "tools") {
			return out, errors.New("model profile does not support tool history")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, model.profile.Timeout)
	defer cancel()
	estimate, complete := r.request(model, role, onEvent)
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
	estimateInput, _ := r.request(model, role, nil)
	estimate, err := estimateInput(messages, tools)
	return estimate <= model.profile.ContextWindowTokens-model.profile.MaxOutputTokens, err
}

// request creates per-call behavior without mutating either shared transport.
func (r *ModelRouter) request(model routedModel, role domain.ModelRole, onEvent domain.StreamFunc) (inputEstimator, func(context.Context, []domain.Message, []domain.Tool) (domain.Completion, error)) {
	if model.profile.Adapter == "openai_chatgpt" {
		return func(messages []domain.Message, tools []domain.Tool) (int, error) {
				return siwc.EstimateInputForProfile(model.profile, messages, tools)
			}, func(ctx context.Context, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
				if onEvent != nil {
					if streamer, ok := r.plan.(streamingSubscriptionModel); ok {
						return streamer.CompleteForProfileStream(ctx, model.profile, messages, tools, onEvent)
					}
				}
				return r.plan.CompleteForProfile(ctx, model.profile, messages, tools)
			}
	}
	client := *model.client
	client.config.JSONOutput = role == domain.RoleAnalysis
	return func(messages []domain.Message, tools []domain.Tool) (int, error) {
			return estimateModelInput(&client, messages, tools)
		}, func(ctx context.Context, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
			if onEvent != nil {
				return client.CompleteStream(ctx, messages, tools, onEvent)
			}
			return client.Complete(ctx, messages, tools)
		}
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
