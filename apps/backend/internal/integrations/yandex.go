package integrations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
)

type YandexConfig struct {
	APIKey     string
	FolderID   string
	Endpoint   string
	HTTPClient *http.Client
	Web        WebConfig
}
type Yandex struct {
	config YandexConfig
	client *http.Client
	reader *WebReader
}

func NewYandex(c YandexConfig) (*Yandex, error) {
	if c.APIKey == "" || c.FolderID == "" {
		return nil, errors.New("Yandex API key and folder ID are required")
	}
	if c.Endpoint == "" {
		c.Endpoint = "https://searchapi.api.cloud.yandex.net/v2/web/search"
	}
	endpoint, err := providerURL(c.Endpoint)
	if err != nil {
		return nil, err
	}
	c.Endpoint = endpoint
	return &Yandex{config: c, client: httpClient(c.HTTPClient, 30*time.Second), reader: NewWebReader(c.Web)}, nil
}
func (y *Yandex) Read(ctx context.Context, u string) (string, error) { return y.reader.Read(ctx, u) }
func (y *Yandex) Search(ctx context.Context, query string) ([]domain.SearchResult, error) {
	if strings.TrimSpace(query) == "" || utf8.RuneCountInString(query) > 400 {
		return nil, errors.New("search query must contain 1 to 400 characters")
	}
	body, _ := json.Marshal(map[string]any{"query": map[string]string{"searchType": "SEARCH_TYPE_RU", "queryText": query, "familyMode": "FAMILY_MODE_MODERATE"}, "folderId": y.config.FolderID, "responseFormat": "FORMAT_XML", "groupSpec": map[string]string{"groupMode": "GROUP_MODE_FLAT", "groupsOnPage": "10", "docsInGroup": "1"}, "maxPassages": "3"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, y.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid search request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Api-Key "+y.config.APIKey)
	var out struct {
		RawData string `json:"rawData"`
	}
	if err := request(ctx, y.client, req, &out, 4<<20); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(out.RawData)
	if err != nil {
		return nil, errors.New("invalid Yandex result encoding")
	}
	var result struct {
		Response struct {
			Error *struct {
				Code string `xml:"code,attr"`
			} `xml:"error"`
			Results struct {
				Groups []struct {
					Docs []struct {
						URL   string `xml:"url"`
						Title struct {
							Inner string `xml:",innerxml"`
						} `xml:"title"`
						Passages []struct {
							Inner string `xml:",innerxml"`
						} `xml:"passages>passage"`
					} `xml:"doc"`
				} `xml:"grouping>group"`
			} `xml:"results"`
		} `xml:"response"`
	}
	if xml.Unmarshal(raw, &result) != nil {
		return nil, errors.New("invalid Yandex XML response")
	}
	if result.Response.Error != nil {
		return nil, errors.New("Yandex search returned an error")
	}
	results := []domain.SearchResult{}
	for _, g := range result.Response.Results.Groups {
		for _, d := range g.Docs {
			var snippets []string
			for _, p := range d.Passages {
				snippets = append(snippets, readableHTML(p.Inner))
			}
			if d.URL != "" {
				results = append(results, domain.SearchResult{Title: readableHTML(d.Title.Inner), URL: d.URL, Snippet: strings.Join(snippets, " ")})
			}
		}
	}
	return results, nil
}

var _ domain.Search = (*Yandex)(nil)
