package config

import (
	"strings"
	"testing"
	"time"
)

func validEnv() map[string]string {
	return map[string]string{"DATABASE_URL": "postgres://u:secret@localhost/db", "TELEGRAM_BOT_TOKEN": "secret-token", "TELEGRAM_ALLOWED_USER_IDS": "123, 123,456", "LLM_API_KEY": "secret-key", "LLM_MODEL": "test-model", "YANDEX_API_KEY": "secret-yandex", "YANDEX_FOLDER_ID": "folder", "S3_ACCESS_KEY": "test", "S3_SECRET_KEY": "secret-s3"}
}
func TestLaunchSettings(t *testing.T) {
	env := validEnv()
	env["TELEGRAM_ALLOWED_GROUP_IDS"] = "-100123"
	env["INITIATIVE_INTERVAL"] = "0"
	cfg, err := load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionIdleTimeout != 30*time.Minute || cfg.InitiativeInterval != 0 || len(cfg.TelegramAllowedUserIDs) != 2 || cfg.TelegramAllowedGroupIDs[0] != -100123 {
		t.Fatalf("unexpected parsed launch settings")
	}
}

func TestSTTConfigurationIsIndependent(t *testing.T) {
	for _, invalid := range []string{"", "STT_MODEL", "STT_REVISION", "STT_BASE_URL"} {
		env := validEnv()
		env["STT_BASE_URL"] = "http://stt:8080/v1"
		env["STT_MODEL"] = "nemotron"
		env["STT_REVISION"] = "weights-v1"
		if invalid != "" {
			env[invalid] = ""
			if invalid == "STT_BASE_URL" {
				env[invalid] = "https://user:PRIVATE@host/v1"
			}
		}
		c, e := load(func(k string) string { return env[k] })
		if invalid == "" {
			if e != nil || c.STTAPIKey != "" || c.STTModel != "nemotron" {
				t.Fatal("optional independent key rejected")
			}
		} else if e == nil || strings.Contains(e.Error(), "PRIVATE") {
			t.Fatal("invalid STT settings accepted or leaked")
		}
	}
}
func TestRequiredOwnerAndSecrets(t *testing.T) {
	env := validEnv()
	env["TELEGRAM_ALLOWED_USER_IDS"] = ""
	env["LLM_API_KEY"] = ""
	_, err := load(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_ALLOWED_USER_IDS") || !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Fatal("expected missing configuration errors")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("error leaked configured secret")
	}
}
func TestInvalidSettingsNeverEchoInput(t *testing.T) {
	for _, key := range []string{"TELEGRAM_ALLOWED_USER_IDS", "TELEGRAM_ALLOWED_GROUP_IDS", "SESSION_IDLE_TIMEOUT", "MAX_AGENT_STEPS", "LLM_BASE_URL", "INSPECTOR_PUBLIC_URL", "VICTORIALOGS_URL", "DATABASE_URL", "S3_SECURE", "S3_ENDPOINT", "BROWSER_ENABLED", "BROWSER_NO_SANDBOX"} {
		t.Run(key, func(t *testing.T) {
			env := validEnv()
			env[key] = "secret-bad-value"
			if key == "S3_ENDPOINT" {
				env[key] = "https://secret-bad-value"
			}
			_, err := load(func(k string) string { return env[k] })
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), "secret-bad-value") {
				t.Fatal("invalid input leaked")
			}
		})
	}
}

func TestSearchCredentialsAreOptionalButMustBeComplete(t *testing.T) {
	for _, key := range []string{"", "YANDEX_API_KEY", "YANDEX_FOLDER_ID"} {
		t.Run("missing_"+key, func(t *testing.T) {
			env := validEnv()
			delete(env, "YANDEX_API_KEY")
			delete(env, "YANDEX_FOLDER_ID")
			if key != "" {
				env[key] = "private-value"
			}
			cfg, err := load(func(k string) string { return env[k] })
			if key == "" {
				if err != nil || cfg.YandexAPIKey != "" || cfg.YandexFolderID != "" {
					t.Fatalf("no-search startup rejected: %v", err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private-value") {
				t.Fatal("partial configuration must fail without leaking input")
			}
		})
	}
}

func TestOwnerUsesFirstAllowedIDOrExplicitAllowedID(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    int64
		invalid bool
	}{
		{want: 123}, {raw: "456", want: 456}, {raw: "999", invalid: true}, {raw: "secret-invalid", invalid: true}, {raw: "0", invalid: true},
	} {
		env := validEnv()
		env["TELEGRAM_OWNER_USER_ID"] = tc.raw
		cfg, err := load(func(k string) string { return env[k] })
		if tc.invalid {
			if err == nil || strings.Contains(err.Error(), "secret-invalid") {
				t.Fatal("invalid owner accepted or echoed")
			}
		} else if err != nil || cfg.TelegramOwnerUserID != tc.want {
			t.Fatalf("owner=%d err=%v", cfg.TelegramOwnerUserID, err)
		}
	}
}
