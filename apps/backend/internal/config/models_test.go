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
chat = "chosen-name"
background = "chosen-name"
analysis = "chosen-name"

[profiles.chosen-name]
adapter = "openai_chat_completions"
base_url = "https://example.invalid/v1/"
model = "configured-model"
api_key_env = "MAIN_MODEL_KEY"
capabilities = ["tools", "vision", "structured_output"]
context_window_tokens = 128000
max_output_tokens = 8192
timeout = "120s"
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

func TestModelProfilesLoadArbitraryNamesOptionsAndPricing(t *testing.T) {
	content := validModels + `
[profiles.chosen-name.options]
reasoning_effort = "high"
thinking = "enabled"
include_reasoning_content = true
session_header = "x-opencode-session"
output_token_field = "max_completion_tokens"

[profiles.chosen-name.pricing]
input_per_million = 3.0
cached_input_per_million = 0.3
output_per_million = 12.0
`
	models, err := loadModelProfiles(modelFile(t, content), modelEnv)
	if err != nil {
		t.Fatal(err)
	}
	p := models.Profiles[models.Routes[domain.RoleAnalysis]]
	if p.Model != "configured-model" || p.BaseURL != "https://example.invalid/v1" || p.APIKey != "private-model-token" || p.Timeout != 2*time.Minute || p.ContextWindowTokens != 128000 || p.MaxOutputTokens != 8192 {
		t.Fatal("profile settings were not retained")
	}
	if p.Options.ReasoningEffort != "high" || p.Options.Thinking != "enabled" || !p.Options.IncludeReasoningContent || p.Options.SessionHeader != "x-opencode-session" || p.Options.OutputTokenField != "max_completion_tokens" || p.Pricing == nil || p.Pricing.CachedInputPerMillion != 0.3 {
		t.Fatal("profile options or pricing were not retained")
	}
	raw, err := json.Marshal(models)
	if err != nil || strings.Contains(string(raw), "private-model-token") {
		t.Fatal("model credentials must not serialize")
	}
}

func TestModelRolesCanSelectDifferentConfiguredProfiles(t *testing.T) {
	content := strings.Replace(validModels, `analysis = "chosen-name"`, `analysis = "other-name"`, 1)
	content += "\n[profiles.other-name]\n" + strings.Replace(strings.SplitN(validModels, "[profiles.chosen-name]", 2)[1], `model = "configured-model"`, `model = "other-model"`, 1)
	models, err := loadModelProfiles(modelFile(t, content), modelEnv)
	if err != nil {
		t.Fatal(err)
	}
	if models.Routes[domain.RoleChat] != "chosen-name" || models.Profiles[models.Routes[domain.RoleAnalysis]].Model != "other-model" {
		t.Fatal("role-specific profile selection lost")
	}
}

func TestModelProfilesRejectInvalidConfigurationWithoutEchoingValues(t *testing.T) {
	cases := map[string]string{
		"conflicting model case": strings.Replace(validModels, `model = "configured-model"`, "model = \"configured-model\"\nMODEL = \"different-model\"", 1),
		"uppercase table":        strings.Replace(validModels, "[profiles.", "[PROFILES.", 1),
		"mixed option case":      validModels + "[profiles.chosen-name.options]\nReasoning_Effort = \"high\"\n",

		"syntax":                   validModels + "private-model-token = [\n",
		"unknown field":            validModels + "private-model-token = true\n",
		"unknown option":           validModels + "[profiles.chosen-name.options]\nprivate-model-token = true\n",
		"unknown role":             strings.Replace(validModels, "chat =", "private-model-token =", 1),
		"default role":             strings.Replace(validModels, "chat =", "default =", 1),
		"missing role":             strings.Replace(validModels, "background = \"chosen-name\"\n", "", 1),
		"undefined profile":        strings.Replace(validModels, "chat = \"chosen-name\"", "chat = \"private-model-token\"", 1),
		"fallback list":            strings.Replace(validModels, "chat = \"chosen-name\"", "chat = [\"chosen-name\"]", 1),
		"empty profile name":       strings.ReplaceAll(validModels, "chosen-name", ""),
		"adapter":                  strings.Replace(validModels, "openai_chat_completions", "private-model-token", 1),
		"url scheme":               strings.Replace(validModels, "https://example.invalid/v1/", "file:///private-model-token", 1),
		"url credentials":          strings.Replace(validModels, "https://example.invalid/v1/", "https://user:private-model-token@example.invalid/v1", 1),
		"url query":                strings.Replace(validModels, "https://example.invalid/v1/", "https://example.invalid/v1?private-model-token", 1),
		"empty model":              strings.Replace(validModels, "configured-model", "", 1),
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
		"reasoning":                validModels + "[profiles.chosen-name.options]\nreasoning_effort = \"private-model-token\"\n",
		"thinking":                 validModels + "[profiles.chosen-name.options]\nthinking = \"private-model-token\"\n",
		"session header":           validModels + "[profiles.chosen-name.options]\nsession_header = \"Authorization\"\n",
		"output field":             validModels + "[profiles.chosen-name.options]\noutput_token_field = \"private-model-token\"\n",
		"missing pricing field":    validModels + "[profiles.chosen-name.pricing]\ninput_per_million = 1.0\noutput_per_million = 2.0\n",
		"negative price":           validModels + "[profiles.chosen-name.pricing]\ninput_per_million = -1.0\ncached_input_per_million = 0.0\noutput_per_million = 2.0\n",
		"infinite price":           validModels + "[profiles.chosen-name.pricing]\ninput_per_million = inf\ncached_input_per_million = 0.0\noutput_per_million = 2.0\n",
		"nan price":                validModels + "[profiles.chosen-name.pricing]\ninput_per_million = nan\ncached_input_per_million = 0.0\noutput_per_million = 2.0\n",
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
	for _, path := range []string{"/nonexistent/private-model-token", t.TempDir(), modelFile(t, strings.Repeat("#", maxModelConfigBytes+1))} {
		_, err := loadModelProfiles(path, modelEnv)
		if err == nil || strings.Contains(err.Error(), "private-model-token") {
			t.Fatal("unreadable or oversized config accepted, or filename echoed")
		}
	}
	t.Setenv("MAIN_MODEL_KEY", "private-model-token")
	models, err := LoadModelProfiles(modelFile(t, validModels))
	if err != nil || models.Profiles["chosen-name"].APIKey != "private-model-token" || models.Profiles["chosen-name"].Options.OutputTokenField != "max_tokens" {
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
	if err != nil || models.Profiles["chosen-name"].Timeout != domain.MaxModelTimeout {
		t.Fatal("maximum model timeout should retain checkpoint headroom", err)
	}
}

const subscriptionProfile = `
[profiles.subscription]
adapter = "openai_chatgpt"
model = "chatgpt-chat-model"
capabilities = ["tools", "vision", "structured_output"]
context_window_tokens = 128000
max_output_tokens = 8192
timeout = "90s"
`

func mixedSubscriptionModels() string {
	content := strings.Replace(validModels, `chat = "chosen-name"`, `chat = "subscription"`, 1)
	content = strings.Replace(content, `analysis = "chosen-name"`, `analysis = "analysis-plan"`, 1)
	return content + subscriptionProfile + strings.ReplaceAll(strings.Replace(subscriptionProfile, "chatgpt-chat-model", "chatgpt-analysis-model", 1), "profiles.subscription", "profiles.analysis-plan")
}

func TestMixedSubscriptionAndAPIProfiles(t *testing.T) {
	var lookups []string
	get := func(key string) string { lookups = append(lookups, key); return modelEnv(key) }
	models, err := loadModelProfiles(modelFile(t, mixedSubscriptionModels()), get)
	if err != nil {
		t.Fatal(err)
	}
	if !models.HasSubscription() || models.Routes[domain.RoleChat] != "subscription" || models.Routes[domain.RoleBackground] != "chosen-name" || models.Routes[domain.RoleAnalysis] != "analysis-plan" {
		t.Fatal("mixed role configuration lost")
	}
	for _, name := range []string{"subscription", "analysis-plan"} {
		p := models.Profiles[name]
		if p.APIKey != "" || p.APIKeyEnv != "" || p.BaseURL != "" || p.Options != (domain.ModelOptions{}) || p.Timeout != 90*time.Second || p.Pricing != nil {
			t.Fatal("subscription profile acquired API-only settings")
		}
	}
	if len(lookups) != 1 || lookups[0] != "MAIN_MODEL_KEY" {
		t.Fatalf("subscription adapter read credentials from API environment: %v", lookups)
	}
	if models.Profiles["subscription"].Model == models.Profiles["analysis-plan"].Model {
		t.Fatal("independent subscription models lost")
	}
}

func TestModelProfilesResolveHeadersForBothAdapters(t *testing.T) {
	content := mixedSubscriptionModels()
	content = strings.Replace(content, "[profiles.subscription]\nadapter", `[profiles.chosen-name.headers_env]
"x-proxy-key" = "API_PROXY_KEY"

[profiles.subscription]
base_url = "https://proxy.example.invalid/v1/"
adapter`, 1)
	content = strings.Replace(content, "[profiles.analysis-plan]", `[profiles.subscription.headers_env]
"X-Proxy-Key" = "PLAN_PROXY_KEY"

[profiles.analysis-plan]`, 1)
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
		t.Fatal("wrong subscription catalog transport")
	}
	if models.Profiles["chosen-name"].Headers["X-Proxy-Key"] != "private-api-proxy-key" || len(models.Profiles["analysis-plan"].Headers) != 0 {
		t.Fatal("headers not isolated to their profiles")
	}
	models.Routes[domain.RoleChat] = "chosen-name"
	if profile := models.SubscriptionCatalogProfile(); profile.Model != "chatgpt-analysis-model" {
		t.Fatal("catalog did not select the analysis subscription when chat uses the API")
	}
	encoded, err := json.Marshal(models)
	if err != nil || strings.Contains(string(encoded), "private-api-proxy-key") || strings.Contains(string(encoded), "private-plan-proxy-key") {
		t.Fatal("custom header values must not serialize")
	}
}

func TestModelProfilesRejectInvalidHeadersWithoutEchoingValues(t *testing.T) {
	const apiHeaders = "\n[profiles.chosen-name.headers_env]\n"
	cases := map[string]string{
		"invalid name":    validModels + apiHeaders + `"private model token" = "MAIN_MODEL_KEY"`,
		"literal value":   validModels + apiHeaders + `"X-Proxy-Key" = "private-model-token"`,
		"missing env":     validModels + apiHeaders + `"X-Proxy-Key" = "UNSET_KEY"`,
		"invalid value":   validModels + apiHeaders + `"X-Proxy-Key" = "INVALID_HEADER"`,
		"duplicate name":  validModels + apiHeaders + "\"X-Proxy-Key\" = \"MAIN_MODEL_KEY\"\n\"x-proxy-key\" = \"MAIN_MODEL_KEY\"",
		"authentication":  validModels + apiHeaders + `"authorization" = "MAIN_MODEL_KEY"`,
		"request framing": validModels + apiHeaders + `"transfer-encoding" = "MAIN_MODEL_KEY"`,
		"session header":  validModels + "\n[profiles.chosen-name.options]\nsession_header = \"x-opencode-session\"\n" + apiHeaders + `"X-OpenCode-Session" = "MAIN_MODEL_KEY"`,
		"implicit URL":    validModels + subscriptionProfile + "\n[profiles.subscription.headers_env]\n" + `"X-Proxy-Key" = "MAIN_MODEL_KEY"`,
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

func TestSubscriptionProfilesRejectIncompatibleSettings(t *testing.T) {
	for name, extra := range map[string]string{
		"invalid endpoint":     `base_url = "https://private-sentinel.invalid/v1?token=PRIVATE_SENTINEL"`,
		"empty fragment":       `base_url = "https://private-sentinel.invalid/v1#"`,
		"API credential":       `api_key_env = "PRIVATE_SENTINEL"`,
		"empty API credential": `api_key_env = ""`,
		"empty API options":    "[profiles.subscription.options]",
		"false API option":     "[profiles.subscription.options]\ninclude_reasoning_content = false",
		"empty API pricing":    "[profiles.subscription.pricing]",
		"API option":           "[profiles.subscription.options]\nreasoning_effort = \"high\"",
		"API pricing":          "[profiles.subscription.pricing]\ninput_per_million = 1\ncached_input_per_million = 0\noutput_per_million = 2",
	} {
		t.Run(name, func(t *testing.T) {
			content := validModels + subscriptionProfile + "\n" + extra + "\n"
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
	allPlan := strings.ReplaceAll(mixedSubscriptionModels(), `background = "chosen-name"`, `background = "subscription"`)
	// Unused API profiles are validated too; remove it from this all-plan file.
	allPlan = strings.SplitN(allPlan, "[profiles.chosen-name]", 2)[0] + subscriptionProfile + strings.ReplaceAll(subscriptionProfile, "profiles.subscription", "profiles.analysis-plan")
	if _, err := loadModelProfiles(modelFile(t, allPlan), func(string) string { t.Fatal("plan-only config read API credentials"); return "" }); err != nil {
		t.Fatal(err)
	}
	if _, err := loadModelProfiles(modelFile(t, mixedSubscriptionModels()), func(string) string { return "" }); err == nil {
		t.Fatal("mixed config accepted missing API credential")
	}
}
