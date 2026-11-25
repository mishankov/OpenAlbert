// Package config loads immutable launch settings. Secret values are never included in errors.
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"openalbert/apps/backend/internal/domain"
)

type Config struct {
	OpenAIAuthMode, OpenAITokenStore, OpenAISiWCModel       string
	OpenAILaneCooldown                                      time.Duration
	DesktopAPIToken                                         string
	TelegramOwnerUserID                                     int64
	DatabaseURL                                             string
	TemporalAddress, TemporalNamespace, TemporalTaskQueue   string
	TelegramWidgets                                         string
	TelegramBotToken, TelegramBaseURL                       string
	TelegramAllowedUserIDs, TelegramAllowedGroupIDs         []int64
	TelegramAllowBotMessages                                bool
	ModelsFile                                              string
	Models                                                  domain.ModelProfiles
	STTBaseURL, STTAPIKey, STTModel, STTRevision            string
	TTSBaseURL, TTSAPIKey, TTSModel, TTSVoice               string
	WebSearchProvider                                       string
	ExaAPIKey, ExaEndpoint                                  string
	ExaMonthlyBudgetUSD                                     float64
	YandexAPIKey, YandexFolderID, YandexEndpoint            string
	KnowledgeDir, SoulFile, HTTPAddr                        string
	SessionIdleTimeout, TaskMinInterval, InitiativeInterval time.Duration
	MemoryConsolidationInterval                             time.Duration
	MaxAgentSteps                                           int
	S3Endpoint, S3AccessKey, S3SecretKey, S3Bucket          string
	S3Secure                                                bool
	BrowserEnabled, BrowserNoSandbox                        bool
	BrowserExecutable                                       string
	InspectorPublicURL                                      string
	VictoriaLogsURL                                         string
	ComputerSSHAddress                                      string
	ComputerSSHUser                                         string
	ComputerSSHPassword                                     string
	ComputerSSHKeyFile                                      string
	BackupStateDir                                          string
}

func Load() (Config, error) { return load(os.Getenv) }
func load(get func(string) string) (Config, error) {
	value := func(k, fallback string) string {
		if v := strings.TrimSpace(get(k)); v != "" {
			return v
		}
		return fallback
	}
	c := Config{
		OpenAIAuthMode: value("OPENAI_AUTH_MODE", "profiles"), OpenAITokenStore: value("OPENAI_TOKEN_STORE", "./container_data/chatgpt/credentials.json"), OpenAISiWCModel: value("OPENAI_SIWC_MODEL", ""),
		TelegramWidgets: value("TELEGRAM_WIDGETS", "auto"),
		DesktopAPIToken: value("DESKTOP_API_TOKEN", ""),
		STTBaseURL:      value("STT_BASE_URL", ""), STTAPIKey: value("STT_API_KEY", ""), STTModel: value("STT_MODEL", ""), STTRevision: value("STT_REVISION", ""),
		TTSBaseURL: value("TTS_BASE_URL", ""), TTSAPIKey: value("TTS_API_KEY", ""), TTSModel: value("TTS_MODEL", "piper"), TTSVoice: value("TTS_VOICE", "ru_RU-dmitri-medium"),
		DatabaseURL: value("DATABASE_URL", ""), TemporalAddress: value("TEMPORAL_ADDRESS", "localhost:7233"), TemporalNamespace: value("TEMPORAL_NAMESPACE", "default"), TemporalTaskQueue: value("TEMPORAL_TASK_QUEUE", "openalbert"),
		TelegramBotToken: value("TELEGRAM_BOT_TOKEN", ""), TelegramBaseURL: value("TELEGRAM_BASE_URL", ""), ModelsFile: value("LLM_CONFIG_FILE", ""),
		WebSearchProvider: strings.ToLower(value("WEB_SEARCH_PROVIDER", "")), ExaAPIKey: value("EXA_API_KEY", ""), ExaEndpoint: value("EXA_SEARCH_URL", ""),
		YandexAPIKey: value("YANDEX_API_KEY", ""), YandexEndpoint: value("YANDEX_SEARCH_URL", ""), YandexFolderID: value("YANDEX_FOLDER_ID", ""), KnowledgeDir: value("KNOWLEDGE_DIR", "./container_data/knowledge"), SoulFile: value("SOUL_FILE", ""), HTTPAddr: value("HTTP_ADDR", ":8080"),
		S3Endpoint: value("S3_ENDPOINT", "localhost:9000"), S3AccessKey: value("S3_ACCESS_KEY", ""), S3SecretKey: value("S3_SECRET_KEY", ""), S3Bucket: value("S3_BUCKET", "openalbert"),
		InspectorPublicURL:  value("INSPECTOR_PUBLIC_URL", "http://localhost:8080/inspect/"),
		VictoriaLogsURL:     strings.TrimSpace(get("VICTORIALOGS_URL")),
		ComputerSSHAddress:  value("COMPUTER_SSH_ADDRESS", ""),
		ComputerSSHUser:     value("COMPUTER_SSH_USER", "albert"),
		ComputerSSHPassword: get("COMPUTER_SSH_PASSWORD"),
		ComputerSSHKeyFile:  value("COMPUTER_SSH_KEY_FILE", ""),
		BackupStateDir:      value("BACKUP_STATE_DIR", ""),
	}
	var problems []error
	if c.TelegramWidgets != "auto" && c.TelegramWidgets != "on" && c.TelegramWidgets != "off" {
		problems = append(problems, errors.New("TELEGRAM_WIDGETS must be auto, on or off"))
	}
	if c.ModelsFile == "" {
		problems = append(problems, errors.New("LLM_CONFIG_FILE is required"))
	} else {
		var modelErr error
		c.Models, modelErr = loadModelProfilesForAuth(c.ModelsFile, get, c.OpenAIAuthMode)
		if modelErr != nil {
			problems = append(problems, modelErr)
		}
	}
	if c.OpenAISiWCModel == "" {
		c.OpenAISiWCModel = c.Models.Profiles[c.Models.Routes[domain.RoleChat]].Model
		if c.OpenAIAuthMode == "profiles" {
			if p := c.Models.SubscriptionCatalogProfile(); p.Model != "" {
				c.OpenAISiWCModel = p.Model
			}
		}
	}
	if c.DesktopAPIToken != "" && len(c.DesktopAPIToken) < 32 {
		problems = append(problems, errors.New("DESKTOP_API_TOKEN must contain at least 32 bytes"))
	}
	if c.ComputerSSHAddress != "" {
		if (c.ComputerSSHPassword == "") == (c.ComputerSSHKeyFile == "") {
			problems = append(problems, errors.New("set exactly one of COMPUTER_SSH_PASSWORD or COMPUTER_SSH_KEY_FILE"))
		}
		host, port, e := net.SplitHostPort(c.ComputerSSHAddress)
		p, pe := strconv.Atoi(port)
		if e != nil || host == "" || strings.ContainsAny(host, "/\\@ \t\r\n") || pe != nil || p < 1 || p > 65535 {
			problems = append(problems, errors.New("COMPUTER_SSH_ADDRESS must be host:port"))
		}
		if strings.ContainsAny(c.ComputerSSHUser, "/\\@ \t\r\n") || strings.HasPrefix(c.ComputerSSHUser, "-") {
			problems = append(problems, errors.New("invalid COMPUTER_SSH_USER"))
		}
	}
	if c.STTBaseURL != "" {
		if u, e := url.Parse(c.STTBaseURL); e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			problems = append(problems, errors.New("STT_BASE_URL must be an HTTP(S) URL without credentials, query or fragment"))
		}
		if c.STTModel == "" || c.STTRevision == "" {
			problems = append(problems, errors.New("STT_MODEL and STT_REVISION are required when STT_BASE_URL is set"))
		}
	}
	if c.TTSBaseURL != "" {
		if u, e := url.Parse(c.TTSBaseURL); e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || strings.ContainsAny(c.TTSBaseURL, "?#") {
			problems = append(problems, errors.New("TTS_BASE_URL must be an HTTP(S) URL without credentials, query or fragment"))
		}
	}
	if c.OpenAIAuthMode != "profiles" && c.OpenAIAuthMode != "api_key" && c.OpenAIAuthMode != "siwc" && c.OpenAIAuthMode != "auto" {
		problems = append(problems, errors.New("OPENAI_AUTH_MODE must be profiles, api_key, siwc or auto"))
	}
	required := []struct{ name, value string }{{"DATABASE_URL", c.DatabaseURL}, {"TELEGRAM_BOT_TOKEN", c.TelegramBotToken}, {"S3_ACCESS_KEY", c.S3AccessKey}, {"S3_SECRET_KEY", c.S3SecretKey}}
	for _, r := range required {
		if r.value == "" {
			problems = append(problems, fmt.Errorf("%s is required", r.name))
		}
	}
	if c.WebSearchProvider != "" && c.WebSearchProvider != "exa" && c.WebSearchProvider != "yandex" {
		problems = append(problems, errors.New("WEB_SEARCH_PROVIDER must be empty, exa, or yandex"))
	}
	if c.ExaEndpoint != "" {
		if u, e := url.Parse(c.ExaEndpoint); e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			problems = append(problems, errors.New("EXA_SEARCH_URL must be an HTTP(S) URL without credentials, query or fragment"))
		}
	}
	if c.YandexEndpoint != "" {
		if u, e := url.Parse(c.YandexEndpoint); e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			problems = append(problems, errors.New("YANDEX_SEARCH_URL must be an HTTP(S) URL without credentials, query or fragment"))
		}
	}
	var budgetErr error
	c.ExaMonthlyBudgetUSD, budgetErr = strconv.ParseFloat(value("EXA_MONTHLY_BUDGET_USD", "10"), 64)
	if budgetErr != nil || math.IsNaN(c.ExaMonthlyBudgetUSD) || math.IsInf(c.ExaMonthlyBudgetUSD, 0) || c.ExaMonthlyBudgetUSD <= 0 || c.ExaMonthlyBudgetUSD > 100000 {
		problems = append(problems, errors.New("EXA_MONTHLY_BUDGET_USD must be a positive number up to 100000"))
	}
	hasExa := c.ExaAPIKey != ""
	hasYandex := c.YandexAPIKey != "" || c.YandexFolderID != ""
	if (c.YandexAPIKey == "") != (c.YandexFolderID == "") {
		problems = append(problems, errors.New("set both YANDEX_API_KEY and YANDEX_FOLDER_ID to enable Yandex search, or leave both empty"))
	}
	switch c.WebSearchProvider {
	case "":
		if hasExa && hasYandex {
			problems = append(problems, errors.New("configure either Exa or Yandex search credentials, or set WEB_SEARCH_PROVIDER"))
		}
	case "exa":
		if !hasExa {
			problems = append(problems, errors.New("EXA_API_KEY is required when WEB_SEARCH_PROVIDER=exa"))
		}
	case "yandex":
		if c.YandexAPIKey == "" || c.YandexFolderID == "" {
			problems = append(problems, errors.New("YANDEX_API_KEY and YANDEX_FOLDER_ID are required when WEB_SEARCH_PROVIDER=yandex"))
		}
	}
	var err error
	c.TelegramAllowedUserIDs, err = parseIDs(get("TELEGRAM_ALLOWED_USER_IDS"), false)
	if err != nil {
		problems = append(problems, fmt.Errorf("TELEGRAM_ALLOWED_USER_IDS: %w", err))
	}
	if len(c.TelegramAllowedUserIDs) == 0 {
		problems = append(problems, errors.New("TELEGRAM_ALLOWED_USER_IDS must contain at least one user ID"))
	}
	// The first allowed user is the owner unless explicitly configured.
	if len(c.TelegramAllowedUserIDs) > 0 {
		c.TelegramOwnerUserID = c.TelegramAllowedUserIDs[0]
	}
	if raw := strings.TrimSpace(get("TELEGRAM_OWNER_USER_ID")); raw != "" {
		owner, e := strconv.ParseInt(raw, 10, 64)
		allowed := false
		for _, id := range c.TelegramAllowedUserIDs {
			allowed = allowed || id == owner
		}
		if e != nil || owner <= 0 || !allowed {
			problems = append(problems, errors.New("TELEGRAM_OWNER_USER_ID must be an ID in TELEGRAM_ALLOWED_USER_IDS"))
		} else {
			c.TelegramOwnerUserID = owner
		}
	}
	c.TelegramAllowedGroupIDs, err = parseIDs(get("TELEGRAM_ALLOWED_GROUP_IDS"), true)
	if err != nil {
		problems = append(problems, fmt.Errorf("TELEGRAM_ALLOWED_GROUP_IDS: %w", err))
	}
	duration := func(key, def string, zero bool) time.Duration {
		v, e := time.ParseDuration(value(key, def))
		if e != nil || v < 0 || (!zero && v == 0) {
			problems = append(problems, fmt.Errorf("%s must be a %s duration", key, map[bool]string{true: "nonnegative", false: "positive"}[zero]))
		}
		return v
	}
	c.OpenAILaneCooldown = duration("OPENAI_LANE_COOLDOWN", "15m", false)
	c.SessionIdleTimeout = duration("SESSION_IDLE_TIMEOUT", "30m", false)
	c.TaskMinInterval = duration("TASK_MIN_INTERVAL", "5m", false)
	c.InitiativeInterval = duration("INITIATIVE_INTERVAL", "30m", true)
	c.MemoryConsolidationInterval = duration("MEMORY_CONSOLIDATION_INTERVAL", "24h", true)
	c.MaxAgentSteps, err = strconv.Atoi(value("MAX_AGENT_STEPS", "100"))
	if err != nil || c.MaxAgentSteps < 1 || c.MaxAgentSteps > 200 {
		problems = append(problems, errors.New("MAX_AGENT_STEPS must be between 1 and 200"))
	}
	c.S3Secure, err = strconv.ParseBool(value("S3_SECURE", "false"))
	if err != nil {
		problems = append(problems, errors.New("S3_SECURE must be a boolean"))
	}
	c.TelegramAllowBotMessages, err = strconv.ParseBool(value("TELEGRAM_ALLOW_BOT_MESSAGES", "false"))
	if err != nil {
		problems = append(problems, errors.New("TELEGRAM_ALLOW_BOT_MESSAGES must be a boolean"))
	}
	c.BrowserEnabled, err = strconv.ParseBool(value("BROWSER_ENABLED", "true"))
	if err != nil {
		problems = append(problems, errors.New("BROWSER_ENABLED must be a boolean"))
	}
	c.BrowserNoSandbox, err = strconv.ParseBool(value("BROWSER_NO_SANDBOX", "false"))
	if err != nil {
		problems = append(problems, errors.New("BROWSER_NO_SANDBOX must be a boolean"))
	}
	c.BrowserExecutable = value("BROWSER_EXECUTABLE", "")
	if u, e := url.Parse(c.DatabaseURL); e != nil || u.Host == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		problems = append(problems, errors.New("DATABASE_URL must be a PostgreSQL connection URL"))
	}
	if u, e := url.Parse(c.InspectorPublicURL); e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		problems = append(problems, errors.New("INSPECTOR_PUBLIC_URL must be an HTTP(S) URL without credentials, query or fragment"))
	}
	if c.VictoriaLogsURL != "" {
		if u, e := url.Parse(c.VictoriaLogsURL); e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			problems = append(problems, errors.New("VICTORIALOGS_URL must be an HTTP(S) URL without credentials, query or fragment"))
		}
	}
	if strings.Contains(c.S3Endpoint, "://") || strings.ContainsAny(c.S3Endpoint, "/?#@") {
		problems = append(problems, errors.New("S3_ENDPOINT must be a host[:port], without scheme or credentials"))
	}
	if len(problems) > 0 {
		return Config{}, errors.Join(problems...)
	}
	return c, nil
}
func parseIDs(s string, groups bool) ([]int64, error) {
	var ids []int64
	seen := map[int64]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, e := strconv.ParseInt(part, 10, 64)
		if e != nil || (!groups && v <= 0) || (groups && v >= 0) {
			return nil, errors.New("expected comma-separated Telegram numeric IDs")
		}
		if !seen[v] {
			ids = append(ids, v)
			seen[v] = true
		}
	}
	return ids, nil
}
