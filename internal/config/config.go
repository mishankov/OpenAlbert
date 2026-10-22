// Package config loads immutable launch settings. Secret values are never included in errors.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TelegramOwnerUserID                                     int64
	DatabaseURL                                             string
	TemporalAddress, TemporalNamespace, TemporalTaskQueue   string
	TelegramBotToken, TelegramBaseURL                       string
	TelegramAllowedUserIDs, TelegramAllowedGroupIDs         []int64
	LLMBaseURL, LLMAPIKey, LLMModel                         string
	STTBaseURL, STTAPIKey, STTModel, STTRevision            string
	YandexAPIKey, YandexFolderID, YandexEndpoint            string
	KnowledgeDir, SoulFile, HTTPAddr                        string
	SessionIdleTimeout, TaskMinInterval, InitiativeInterval time.Duration
	MaxAgentSteps                                           int
	S3Endpoint, S3AccessKey, S3SecretKey, S3Bucket          string
	S3Secure                                                bool
	BrowserEnabled, BrowserNoSandbox                        bool
	BrowserExecutable                                       string
	InspectorPublicURL                                      string
	VictoriaLogsURL                                         string
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
		STTBaseURL: value("STT_BASE_URL", ""), STTAPIKey: value("STT_API_KEY", ""), STTModel: value("STT_MODEL", ""), STTRevision: value("STT_REVISION", ""),
		DatabaseURL: value("DATABASE_URL", ""), TemporalAddress: value("TEMPORAL_ADDRESS", "localhost:7233"), TemporalNamespace: value("TEMPORAL_NAMESPACE", "default"), TemporalTaskQueue: value("TEMPORAL_TASK_QUEUE", "jarvis"),
		TelegramBotToken: value("TELEGRAM_BOT_TOKEN", ""), TelegramBaseURL: value("TELEGRAM_BASE_URL", ""), LLMBaseURL: value("LLM_BASE_URL", "https://api.openai.com/v1"), LLMAPIKey: value("LLM_API_KEY", ""), LLMModel: value("LLM_MODEL", ""),
		YandexAPIKey: value("YANDEX_API_KEY", ""), YandexEndpoint: value("YANDEX_SEARCH_URL", ""), YandexFolderID: value("YANDEX_FOLDER_ID", ""), KnowledgeDir: value("KNOWLEDGE_DIR", "./container_data/knowledge"), SoulFile: value("SOUL_FILE", ""), HTTPAddr: value("HTTP_ADDR", ":8080"),
		S3Endpoint: value("S3_ENDPOINT", "localhost:9000"), S3AccessKey: value("S3_ACCESS_KEY", ""), S3SecretKey: value("S3_SECRET_KEY", ""), S3Bucket: value("S3_BUCKET", "jarvis"),
		InspectorPublicURL: value("INSPECTOR_PUBLIC_URL", "http://localhost:8080/inspect/"),
		VictoriaLogsURL:    strings.TrimSpace(get("VICTORIALOGS_URL")),
	}
	var problems []error
	if c.STTBaseURL != "" {
		if u, e := url.Parse(c.STTBaseURL); e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			problems = append(problems, errors.New("STT_BASE_URL must be an HTTP(S) URL without credentials, query or fragment"))
		}
		if c.STTModel == "" || c.STTRevision == "" {
			problems = append(problems, errors.New("STT_MODEL and STT_REVISION are required when STT_BASE_URL is set"))
		}
	}
	required := []struct{ name, value string }{{"DATABASE_URL", c.DatabaseURL}, {"TELEGRAM_BOT_TOKEN", c.TelegramBotToken}, {"LLM_API_KEY", c.LLMAPIKey}, {"LLM_MODEL", c.LLMModel}, {"S3_ACCESS_KEY", c.S3AccessKey}, {"S3_SECRET_KEY", c.S3SecretKey}}
	for _, r := range required {
		if r.value == "" {
			problems = append(problems, fmt.Errorf("%s is required", r.name))
		}
	}
	if (c.YandexAPIKey == "") != (c.YandexFolderID == "") {
		problems = append(problems, errors.New("set both YANDEX_API_KEY and YANDEX_FOLDER_ID to enable search, or leave both empty"))
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
	c.SessionIdleTimeout = duration("SESSION_IDLE_TIMEOUT", "30m", false)
	c.TaskMinInterval = duration("TASK_MIN_INTERVAL", "5m", false)
	c.InitiativeInterval = duration("INITIATIVE_INTERVAL", "30m", true)
	c.MaxAgentSteps, err = strconv.Atoi(value("MAX_AGENT_STEPS", "100"))
	if err != nil || c.MaxAgentSteps < 1 || c.MaxAgentSteps > 200 {
		problems = append(problems, errors.New("MAX_AGENT_STEPS must be between 1 and 200"))
	}
	c.S3Secure, err = strconv.ParseBool(value("S3_SECURE", "false"))
	if err != nil {
		problems = append(problems, errors.New("S3_SECURE must be a boolean"))
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
	if u, e := url.Parse(c.LLMBaseURL); e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		problems = append(problems, errors.New("LLM_BASE_URL must be an HTTP(S) URL without credentials, query or fragment"))
	}
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
