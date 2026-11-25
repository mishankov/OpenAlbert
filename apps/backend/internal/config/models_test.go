package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

const validModels = `[routes]
chat = "primary/configured-model"
background = "primary/configured-model"
analysis = "primary/configured-model"

[providers.primary]
adapter = "openai_chat_completions"
base_url = "https://example.invalid/v1/"
api_key_env = "MAIN_MODEL_KEY"
discover_models = true
default_capabilities = ["tools", "vision", "structured_output"]
default_context_window_tokens = 128000
default_max_output_tokens = 8192
default_timeout = "120s"
`

func modelFile(t testing.TB, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.toml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func modelEnv(key string) string {
	if key == "MAIN_MODEL_KEY" {
		return "private-model-token"
	}
	return ""
}

func TestModelProvidersLoadDefaultsOverridesOptionsAndPricing(t *testing.T) {
	content := validModels + `
[providers.primary.default_options]
reasoning_effort = "medium"
output_token_field = "max_completion_tokens"

[models."primary/configured-model"]
display_name = "Configured Model"
context_window_tokens = 200000
max_output_tokens = 16384
supported_reasoning_efforts = ["low", "medium", "high"]

[models."primary/configured-model".options]
reasoning_effort = "high"
thinking = "enabled"
include_reasoning_content = true
session_header = "x-opencode-session"
output_token_field = "max_completion_tokens"

[models."primary/configured-model".pricing]
input_per_million = 3.0
cached_input_per_million = 0.3
output_per_million = 12.0
`
	models, err := loadModelProfiles(modelFile(t, content), modelEnv)
	if err != nil {
		t.Fatal(err)
	}
	if models.Routes[domain.RoleCoding] != "" {
		t.Fatalf("unset coding route should not be synthesized into routes map: %+v", models.Routes)
	}
	p := models.Profiles[models.Routes[domain.RoleAnalysis]]
	if p.Provider != "primary" || p.Model != "configured-model" || p.DisplayName != "Configured Model" || p.BaseURL != "https://example.invalid/v1" || p.APIKey != "private-model-token" || p.Timeout != 2*time.Minute || p.ContextWindowTokens != 200000 || p.MaxOutputTokens != 16384 {
		t.Fatalf("profile settings were not retained: %+v", p)
	}
	if p.Options.ReasoningEffort != "high" || p.Options.Thinking != "enabled" || !p.Options.IncludeReasoningContent || p.Options.SessionHeader != "x-opencode-session" || p.Options.OutputTokenField != "max_completion_tokens" || p.Pricing == nil || p.Pricing.CachedInputPerMillion != 0.3 || len(p.SupportedReasoningEfforts) != 3 {
		t.Fatalf("profile options or pricing were not retained: %+v", p)
	}
	raw, err := json.Marshal(models)
	if err != nil || strings.Contains(string(raw), "private-model-token") {
		t.Fatal("model credentials must not serialize")
	}
}

func TestModelRolesCanSelectDifferentModelsAndCodingDefault(t *testing.T) {
	content := strings.Replace(validModels, `analysis = "primary/configured-model"`, "analysis = \"primary/other-model\"\ncoding = \"primary/coding-model\"", 1)
	models, err := loadModelProfiles(modelFile(t, content), modelEnv)
	if err != nil {
		t.Fatal(err)
	}
	if models.Routes[domain.RoleChat] != "primary/configured-model" ||
		models.Profiles[models.Routes[domain.RoleAnalysis]].Model != "other-model" ||
		models.Profiles[models.Routes[domain.RoleCoding]].Model != "coding-model" {
		t.Fatalf("role-specific model selection lost: %+v", models.Routes)
	}
}

func TestModelProvidersRejectInvalidConfigurationWithoutEchoingValues(t *testing.T) {
	cases := map[string]string{
		"uppercase table":          strings.Replace(validModels, "[providers.", "[PROVIDERS.", 1),
		"mixed option case":        validModels + "[providers.primary.default_options]\nReasoning_Effort = \"high\"\n",
		"legacy profiles table":    validModels + "[profiles.legacy]\nmodel = \"x\"\n",
		"syntax":                   validModels + "private-model-token = [\n",
		"unknown field":            validModels + "private-model-token = true\n",
		"unknown option":           validModels + "[providers.primary.default_options]\nprivate-model-token = true\n",
		"unknown role":             strings.Replace(validModels, "chat =", "private-model-token =", 1),
		"default role":             strings.Replace(validModels, "chat =", "default =", 1),
		"missing role":             strings.Replace(validModels, "background = \"primary/configured-model\"\n", "", 1),
		"undefined provider":       strings.Replace(validModels, "chat = \"primary/configured-model\"", "chat = \"private-model-token/m\"", 1),
		"missing slash in route":   strings.Replace(validModels, "chat = \"primary/configured-model\"", "chat = \"private-model-token\"", 1),
		"fallback list":            strings.Replace(validModels, "chat = \"primary/configured-model\"", "chat = [\"primary/configured-model\"]", 1),
		"adapter":                  strings.Replace(validModels, "openai_chat_completions", "private-model-token", 1),
		"url scheme":               strings.Replace(validModels, "https://example.invalid/v1/", "file:///private-model-token", 1),
		"url credentials":          strings.Replace(validModels, "https://example.invalid/v1/", "https://user:private-model-token@example.invalid/v1", 1),
		"url query":                strings.Replace(validModels, "https://example.invalid/v1/", "https://example.invalid/v1?private-model-token", 1),
		"empty model in route":     strings.Replace(validModels, "primary/configured-model", "primary/", 1),
		"invalid env name":         strings.Replace(validModels, "MAIN_MODEL_KEY", "private-model-token", 1),
		"missing key":              strings.Replace(validModels, "MAIN_MODEL_KEY", "UNSET_KEY", 1),
		"unknown capability":       strings.Replace(validModels, `"vision"`, `"private-model-token"`, 1),
		"duplicate capability":     strings.Replace(validModels, `"vision"`, `"tools"`, 1),
		"no tools":                 strings.Replace(validModels, `"tools", `, "", 1),
		"no structured output":     strings.Replace(validModels, `, "structured_output"`, "", 1),
		"zero context":             strings.Replace(validModels, "128000", "0", 1),
		"negative output":          strings.Replace(validModels, "8192", "-1", 1),
		"output exceeds context":   strings.Replace(validModels, "8192", "128000", 1),
		"timeout":                  strings.Replace(validModels, "120s", "private-model-token", 1),
		"negative timeout":         strings.Replace(validModels, "120s", "-1s", 1),
		"timeout exceeds activity": strings.Replace(validModels, "120s", "150.001s", 1),
		"reasoning":                validModels + "[providers.primary.default_options]\nreasoning_effort = \"private-model-token\"\n",
		"thinking":                 validModels + "[providers.primary.default_options]\nthinking = \"private-model-token\"\n",
		"session header":           validModels + "[providers.primary.default_options]\nsession_header = \"Authorization\"\n",
		"output field":             validModels + "[providers.primary.default_options]\noutput_token_field = \"private-model-token\"\n",
		"invalid include glob":     validModels + "include_models = [\"[private-model-token\"]\n",
		"missing pricing field":    validModels + "[providers.primary.default_pricing]\ninput_per_million = 1.0\noutput_per_million = 2.0\n",
		"negative price":           validModels + "[providers.primary.default_pricing]\ninput_per_million = -1.0\ncached_input_per_million = 0.0\noutput_per_million = 2.0\n",
		"infinite price":           validModels + "[providers.primary.default_pricing]\ninput_per_million = inf\ncached_input_per_million = 0.0\noutput_per_million = 2.0\n",
		"nan price":                validModels + "[providers.primary.default_pricing]\ninput_per_million = nan\ncached_input_per_million = 0.0\noutput_per_million = 2.0\n",
		"bad model override key":   validModels + "[models.private-model-token]\ncontext_window_tokens = 10000\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadModelProfiles(modelFile(t, content), modelEnv)
			if err == nil {
				t.Fatal("invalid model configuration accepted")
			}
			if strings.Contains(err.Error(), "private-model-token") {
				t.Fatal("model error echoed source content")
			}
		})
	}
}

func TestModelFileBoundsAndEnvironmentAPI(t *testing.T) {
	for _, p := range []string{"/nonexistent/private-model-token", t.TempDir(), modelFile(t, strings.Repeat("#", maxModelConfigBytes+1))} {
		_, err := loadModelProfiles(p, modelEnv)
		if err == nil || strings.Contains(err.Error(), "private-model-token") {
			t.Fatal("unreadable or oversized config accepted, or filename echoed")
		}
	}
	t.Setenv("MAIN_MODEL_KEY", "private-model-token")
	models, err := LoadModelProfiles(modelFile(t, validModels))
	if err != nil || models.Profiles["primary/configured-model"].APIKey != "private-model-token" || models.Profiles["primary/configured-model"].Options.OutputTokenField != "max_tokens" {
		t.Fatal("exported environment loader failed", err)
	}
}

func TestLegacyLLMSettingsDoNotReplaceTOML(t *testing.T) {
	env := validEnv(t)
	delete(env, "LLM_CONFIG_FILE")
	env["LLM_BASE_URL"] = "https://example.invalid/v1"
	env["LLM_MODEL"] = "legacy-model"
	env["LLM_API_KEY"] = "private-model-token"
	_, err := load(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "LLM_CONFIG_FILE") || strings.Contains(err.Error(), "private-model-token") {
		t.Fatal("legacy LLM settings must not provide routing")
	}
}

func TestModelTimeoutFitsEnclosingActivity(t *testing.T) {
	models, err := loadModelProfiles(modelFile(t, strings.Replace(validModels, "120s", "150s", 1)), modelEnv)
	if err != nil || models.Profiles["primary/configured-model"].Timeout != domain.MaxModelTimeout {
		t.Fatal("maximum model timeout should retain checkpoint headroom", err)
	}
}

func TestModelOverrideOptionsMergeWithProviderDefaults(t *testing.T) {
	cfg := validModels + `
[providers.primary.default_options]
output_token_field = "max_completion_tokens"
include_reasoning_content = true
reasoning_effort = "medium"

[models."primary/configured-model".options]
reasoning_effort = "high"
`
	models, err := loadModelProfiles(modelFile(t, cfg), modelEnv)
	if err != nil {
		t.Fatal(err)
	}
	opts := models.Profiles["primary/configured-model"].Options
	if opts.ReasoningEffort != "high" || opts.OutputTokenField != "max_completion_tokens" || !opts.IncludeReasoningContent {
		t.Fatalf("model options override clobbered inherited provider defaults: %+v", opts)
	}
}

const subscriptionProvider = `
[providers.chatgpt]
adapter = "openai_chatgpt"
discover_models = true
default_capabilities = ["tools", "vision", "structured_output"]
default_context_window_tokens = 128000
default_max_output_tokens = 8192
default_timeout = "90s"
`

func mixedSubscriptionModels() string {
	content := strings.Replace(validModels, `chat = "primary/configured-model"`, `chat = "chatgpt/chatgpt-chat-model"`, 1)
	content = strings.Replace(content, `analysis = "primary/configured-model"`, `analysis = "chatgpt/chatgpt-analysis-model"`, 1)
	return content + subscriptionProvider
}

func TestMixedSubscriptionAndAPIProviders(t *testing.T) {
	var lookups []string
	get := func(key string) string { lookups = append(lookups, key); return modelEnv(key) }
	models, err := loadModelProfiles(modelFile(t, mixedSubscriptionModels()), get)
	if err != nil {
		t.Fatal(err)
	}
	if !models.HasSubscription() || models.Routes[domain.RoleChat] != "chatgpt/chatgpt-chat-model" || models.Routes[domain.RoleBackground] != "primary/configured-model" || models.Routes[domain.RoleAnalysis] != "chatgpt/chatgpt-analysis-model" {
		t.Fatal("mixed role configuration lost")
	}
	for _, name := range []string{"chatgpt/chatgpt-chat-model", "chatgpt/chatgpt-analysis-model"} {
		p := models.Profiles[name]
		if p.APIKey != "" || p.APIKeyEnv != "" || p.BaseURL != "" || p.Options != (domain.ModelOptions{}) || p.Timeout != 90*time.Second || p.Pricing != nil {
			t.Fatal("subscription profile acquired API-only settings")
		}
	}
	if len(lookups) != 1 || lookups[0] != "MAIN_MODEL_KEY" {
		t.Fatalf("subscription adapter read credentials from API environment: %v", lookups)
	}
	if models.Profiles["chatgpt/chatgpt-chat-model"].Model == models.Profiles["chatgpt/chatgpt-analysis-model"].Model {
		t.Fatal("independent subscription models lost")
	}
}

func TestModelProvidersResolveHeadersForBothAdapters(t *testing.T) {
	content := mixedSubscriptionModels()
	content = strings.Replace(content, "[providers.chatgpt]\nadapter", `[providers.primary.headers_env]
"x-proxy-key" = "API_PROXY_KEY"

[providers.chatgpt]
base_url = "https://proxy.example.invalid/v1/"
adapter`, 1)
	content += `
[providers.chatgpt.headers_env]
"X-Proxy-Key" = "PLAN_PROXY_KEY"
`
	get := func(key string) string {
		switch key {
		case "API_PROXY_KEY":
			return "private-api-proxy-key"
		case "PLAN_PROXY_KEY":
			return "private-plan-proxy-key"
		default:
			return modelEnv(key)
		}
	}
	models, err := loadModelProfiles(modelFile(t, content), get)
	if err != nil {
		t.Fatal(err)
	}
	plan := models.SubscriptionCatalogProfile()
	if plan.Model != "chatgpt-chat-model" || plan.BaseURL != "https://proxy.example.invalid/v1" || plan.Headers["X-Proxy-Key"] != "private-plan-proxy-key" {
		t.Fatalf("wrong subscription catalog transport: %+v", plan)
	}
	if models.Profiles["primary/configured-model"].Headers["X-Proxy-Key"] != "private-api-proxy-key" {
		t.Fatal("headers not isolated to their providers")
	}
	encoded, err := json.Marshal(models)
	if err != nil || strings.Contains(string(encoded), "private-api-proxy-key") || strings.Contains(string(encoded), "private-plan-proxy-key") {
		t.Fatal("custom header values must not serialize")
	}
}

func TestModelProvidersRejectInvalidHeadersWithoutEchoingValues(t *testing.T) {
	const apiHeaders = "\n[providers.primary.headers_env]\n"
	cases := map[string]string{
		"invalid name":    validModels + apiHeaders + `"private model token" = "MAIN_MODEL_KEY"`,
		"literal value":   validModels + apiHeaders + `"X-Proxy-Key" = "private-model-token"`,
		"missing env":     validModels + apiHeaders + `"X-Proxy-Key" = "UNSET_KEY"`,
		"invalid value":   validModels + apiHeaders + `"X-Proxy-Key" = "INVALID_HEADER"`,
		"duplicate name":  validModels + apiHeaders + "\"X-Proxy-Key\" = \"MAIN_MODEL_KEY\"\n\"x-proxy-key\" = \"MAIN_MODEL_KEY\"",
		"authentication":  validModels + apiHeaders + `"authorization" = "MAIN_MODEL_KEY"`,
		"request framing": validModels + apiHeaders + `"transfer-encoding" = "MAIN_MODEL_KEY"`,
		"session header":  validModels + "\n[providers.primary.default_options]\nsession_header = \"x-opencode-session\"\n" + apiHeaders + `"X-OpenCode-Session" = "MAIN_MODEL_KEY"`,
		"implicit URL":    validModels + subscriptionProvider + "\n[providers.chatgpt.headers_env]\n" + `"X-Proxy-Key" = "MAIN_MODEL_KEY"`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadModelProfiles(modelFile(t, content), func(ref string) string {
				if ref == "INVALID_HEADER" {
					return "private-model-token\r\n"
				}
				return modelEnv(ref)
			})
			if err == nil || strings.Contains(err.Error(), "private-model-token") {
				t.Fatal("invalid header configuration accepted or echoed")
			}
		})
	}
}

func TestDiscoverModelsDefaultsTrueAndCanBeDisabled(t *testing.T) {
	omitted := strings.Replace(validModels, "discover_models = true\n", "", 1)
	models, err := loadModelProfiles(modelFile(t, omitted), modelEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !models.Providers["primary"].DiscoverModels {
		t.Fatal("discover_models must default to true when omitted")
	}

	disabled := strings.Replace(validModels, "discover_models = true", "discover_models = false", 1)
	models, err = loadModelProfiles(modelFile(t, disabled), modelEnv)
	if err != nil {
		t.Fatal(err)
	}
	if models.Providers["primary"].DiscoverModels {
		t.Fatal("discover_models = false must disable discovery")
	}
}

func TestSubscriptionProvidersAllowReasoningEffortAndRejectIncompatibleSettings(t *testing.T) {
	allowed := validModels + subscriptionProvider + `
[providers.chatgpt.default_options]
reasoning_effort = "medium"

[models."chatgpt/codex-5".options]
reasoning_effort = "xhigh"
`
	models, err := loadModelProfiles(modelFile(t, allowed), modelEnv)
	if err != nil {
		t.Fatalf("expected ChatGPT provider/model reasoning_effort to succeed: %v", err)
	}
	if models.Providers["chatgpt"].DefaultOptions.ReasoningEffort != "medium" || models.Models["chatgpt/codex-5"].Options.ReasoningEffort != "xhigh" {
		t.Fatalf("ChatGPT reasoning_effort not preserved: prov=%+v model=%+v", models.Providers["chatgpt"].DefaultOptions, models.Models["chatgpt/codex-5"].Options)
	}

	for name, extra := range map[string]string{
		"invalid endpoint":     `base_url = "https://private-sentinel.invalid/v1?token=PRIVATE_SENTINEL"`,
		"empty fragment":       `base_url = "https://private-sentinel.invalid/v1#"`,
		"API credential":       `api_key_env = "PRIVATE_SENTINEL"`,
		"empty API credential": `api_key_env = ""`,
		"empty API options":    "[providers.chatgpt.default_options]",
		"false API option":     "[providers.chatgpt.default_options]\ninclude_reasoning_content = false",
		"thinking option":      "[providers.chatgpt.default_options]\nreasoning_effort = \"high\"\nthinking = \"enabled\"",
		"output field option":  "[providers.chatgpt.default_options]\nreasoning_effort = \"high\"\noutput_token_field = \"max_completion_tokens\"",
		"model thinking opt":   "[models.\"chatgpt/codex-5\".options]\nreasoning_effort = \"high\"\nthinking = \"enabled\"",
		"empty API pricing":    "[providers.chatgpt.default_pricing]",
		"API pricing":          "[providers.chatgpt.default_pricing]\ninput_per_million = 1\ncached_input_per_million = 0\noutput_per_million = 2",
	} {
		t.Run(name, func(t *testing.T) {
			content := validModels + subscriptionProvider + "\n" + extra + "\n"
			_, err := loadModelProfiles(modelFile(t, content), modelEnv)
			if err == nil || strings.Contains(err.Error(), "private-sentinel") || strings.Contains(err.Error(), "PRIVATE_SENTINEL") {
				t.Fatalf("invalid subscription settings accepted or echoed: %v", err)
			}
		})
	}
	for _, mode := range []string{"api_key", "siwc", "auto"} {
		_, err := loadModelProfilesForAuth(modelFile(t, mixedSubscriptionModels()), modelEnv, mode)
		if err == nil || !strings.Contains(err.Error(), "OPENAI_AUTH_MODE=profiles") {
			t.Fatalf("legacy global %s silently overrides subscription routes: %v", mode, err)
		}
	}
}

func TestSubscriptionOnlyRequiresNoAPISecretButMixedAPIStillDoes(t *testing.T) {
	allPlan := `[routes]
chat = "chatgpt/chatgpt-chat-model"
background = "chatgpt/chatgpt-chat-model"
analysis = "chatgpt/chatgpt-analysis-model"
` + subscriptionProvider
	if _, err := loadModelProfiles(modelFile(t, allPlan), func(string) string { t.Fatal("plan-only config read API credentials"); return "" }); err != nil {
		t.Fatal(err)
	}
	if _, err := loadModelProfiles(modelFile(t, mixedSubscriptionModels()), func(string) string { return "" }); err == nil {
		t.Fatal("mixed config accepted missing API credential")
	}
}
