package integrations

import (
	"context"
	"errors"
	"strings"

	"openalbert/apps/backend/internal/domain"
)

var ErrSearchDisabled = errors.New("web_search API tool is disabled; use listed browser tools to find sources or web_read for a known public URL")

type SearchConfig struct {
	Provider string // "", "exa", or "yandex"
	Exa      ExaConfig
	Yandex   YandexConfig
	Web      WebConfig
}

// NewSearch selects the configured search provider (Exa or Yandex) or falls back
// to read-only public URL fetching when no provider credentials are configured.
// Configuring both providers without an explicit Provider fails closed.
func NewSearch(c SearchConfig) (domain.Search, error) {
	provider := strings.ToLower(strings.TrimSpace(c.Provider))
	hasExa := strings.TrimSpace(c.Exa.APIKey) != ""
	hasYandex := strings.TrimSpace(c.Yandex.APIKey) != "" || strings.TrimSpace(c.Yandex.FolderID) != ""

	switch provider {
	case "":
		if hasExa && hasYandex {
			return nil, errors.New("configure either Exa or Yandex search credentials, or set WEB_SEARCH_PROVIDER")
		}
		if hasExa {
			c.Exa.Web = mergeWebConfig(c.Exa.Web, c.Web)
			return NewExa(c.Exa)
		}
		if hasYandex {
			c.Yandex.Web = mergeWebConfig(c.Yandex.Web, c.Web)
			return NewYandex(c.Yandex)
		}
		return &readOnlyWeb{WebReader: NewWebReader(c.Web)}, nil
	case "exa":
		c.Exa.Web = mergeWebConfig(c.Exa.Web, c.Web)
		return NewExa(c.Exa)
	case "yandex":
		c.Yandex.Web = mergeWebConfig(c.Yandex.Web, c.Web)
		return NewYandex(c.Yandex)
	default:
		return nil, errors.New("WEB_SEARCH_PROVIDER must be empty, exa, or yandex")
	}
}

func mergeWebConfig(specific, fallback WebConfig) WebConfig {
	if specific == (WebConfig{}) {
		return fallback
	}
	return specific
}

type readOnlyWeb struct{ *WebReader }

func (w *readOnlyWeb) Search(context.Context, domain.SearchQuery) (domain.SearchResponse, error) {
	return domain.SearchResponse{}, ErrSearchDisabled
}
func (w *readOnlyWeb) SearchEnabled() bool    { return false }
func (w *readOnlyWeb) SearchProvider() string { return "" }
func (w *readOnlyWeb) SearchRuntimeStatus() domain.SearchRuntimeStatus {
	return domain.SearchRuntimeStatus{Enabled: false}
}

func (y *Yandex) SearchEnabled() bool { return true }
