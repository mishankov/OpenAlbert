package integrations

import (
	"context"
	"errors"

	"jarvis/internal/domain"
)

var ErrSearchDisabled = errors.New("web search is disabled; ask for a public URL to read instead")

// NewSearch enables Yandex only when both credentials are configured. Public URL
// reading uses its own HTTP client and remains available without a search provider.
func NewSearch(c YandexConfig) (domain.Search, error) {
	if c.APIKey == "" && c.FolderID == "" {
		return &readOnlyWeb{WebReader: NewWebReader(c.Web)}, nil
	}
	return NewYandex(c)
}

type readOnlyWeb struct{ *WebReader }

func (w *readOnlyWeb) Search(context.Context, string) ([]domain.SearchResult, error) {
	return nil, ErrSearchDisabled
}
func (w *readOnlyWeb) SearchEnabled() bool { return false }
func (y *Yandex) SearchEnabled() bool      { return true }
