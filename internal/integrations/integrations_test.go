package integrations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jarvis/internal/domain"
)

func TestOpenAIChatContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("incorrect request endpoint or authorization")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["model"]) != `"chosen-model"` {
			t.Error("launch model not used")
		}
		var messages []map[string]json.RawMessage
		_ = json.Unmarshal(body["messages"], &messages)
		if !strings.Contains(string(messages[0]["content"]), "image_url") {
			t.Error("missing image parts")
		}
		if !strings.Contains(string(body["tools"]), `"parameters":{"type":"object"}`) {
			t.Error("invalid tool schema")
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":null,"reasoning_content":"Returned provider reasoning","tool_calls":[{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":123,"completion_tokens":9}}`)
	}))
	defer server.Close()
	model, err := NewOpenAI(OpenAIConfig{BaseURL: server.URL + "/v1", APIKey: "secret", Model: "chosen-model"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := model.Complete(context.Background(), []domain.Message{{Role: "user", Content: "what", Images: []string{"data:image/png;base64,aGVsbG8="}}}, []domain.Tool{{Name: "web_search", Parameters: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Message.ToolCalls) != 1 || out.Message.ToolCalls[0].Name != "web_search" || out.InputTokens != 123 || out.Message.Reasoning != "Returned provider reasoning" {
		t.Fatalf("unexpected completion %+v", out)
	}
}
func TestOpenAIRejectsTruncatedOrInvalidToolCall(t *testing.T) {
	for _, body := range []string{`{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`, `{"choices":[{"message":{"tool_calls":[{"id":"c","type":"function","function":{"name":"x","arguments":"broken"}}]}}]}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer s.Close()
			m, _ := NewOpenAI(OpenAIConfig{BaseURL: s.URL, Model: "x"})
			if _, err := m.Complete(context.Background(), nil, nil); err == nil {
				t.Fatal("accepted incomplete response")
			}
		})
	}
}
func TestProviderErrorsNeverExposeBodyOrURL(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, "SECRET_FROM_BODY")
	}))
	defer s.Close()
	m, _ := NewOpenAI(OpenAIConfig{BaseURL: s.URL, Model: "x", APIKey: "SECRET_FROM_KEY"})
	_, err := m.Complete(context.Background(), nil, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), s.URL) {
		t.Fatalf("unsafe error %v", err)
	}
}
func TestYandexContractAndXML(t *testing.T) {
	xml := `<yandexsearch><response><results><grouping><group><doc><url>https://example.com/a</url><title>Go <hl>docs</hl></title><passages><passage>A &amp; B</passage></passages></doc></group></grouping></results></response></yandexsearch>`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Api-Key secret" {
			t.Error("missing auth")
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["folderId"] != "folder" || b["responseFormat"] != "FORMAT_XML" {
			t.Errorf("request %v", b)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"rawData": base64.StdEncoding.EncodeToString([]byte(xml))})
	}))
	defer s.Close()
	y, err := NewYandex(YandexConfig{APIKey: "secret", FolderID: "folder", Endpoint: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	out, err := y.Search(context.Background(), "golang")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Title != "Go docs" || out[0].Snippet != "A & B" {
		t.Fatalf("bad parsed search: %+v", out)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type resolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(c context.Context, s string) ([]net.IPAddr, error) { return f(c, s) }
func publicResolver() IPResolver {
	return resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	})
}
func TestWebReadExtractionAndLimits(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(`<html><head><title>Title</title><script>secret_script</script></head><body><nav>menu</nav><main>Hello <b>world</b> &amp; friends</main><p hidden>hidden</p></body></html>`)), Request: r}, nil
	})}
	reader := NewWebReader(WebConfig{HTTPClient: client, Resolver: publicResolver()})
	out, err := reader.Read(context.Background(), "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Title Hello world & friends" {
		t.Fatalf("bad readable text %q", out)
	}
	reader = NewWebReader(WebConfig{HTTPClient: client, Resolver: publicResolver(), MaxBytes: 8})
	if _, err = reader.Read(context.Background(), "https://example.com"); err == nil {
		t.Fatal("body limit not enforced")
	}
}
func TestWebReadBlocksSSRF(t *testing.T) {
	called := false
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("should not reach transport")
	})}
	r := NewWebReader(WebConfig{HTTPClient: client, Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.0.0.1")}}, nil
	})})
	for _, u := range []string{"http://127.0.0.1", "http://[::1]", "http://169.254.169.254/latest/meta-data", "http://[::ffff:127.0.0.1]", "http://10.1.1.1", "ftp://example.com", "http://user:pass@example.com", "http://example.com:8080", "https://mixed-dns.example"} {
		if _, err := r.Read(context.Background(), u); err == nil {
			t.Errorf("allowed %s", u)
		}
	}
	if called {
		t.Fatal("blocked URL reached transport")
	}
}
func TestWebRedirectCannotReachPrivateHost(t *testing.T) {
	calls := 0
	r := NewWebReader(WebConfig{Resolver: publicResolver(), HTTPClient: &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://127.0.0.1/private"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}})
	if _, err := r.Read(context.Background(), "https://example.com"); err == nil {
		t.Fatal("redirect accepted")
	}
	if calls != 1 {
		t.Fatalf("redirect reached private server: %d", calls)
	}
}
func TestTelegramPollAuthorizationScopesAndAttachments(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botsecret/getUpdates" {
			t.Error("wrong method")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["offset"] != float64(10) {
			t.Error("offset not preserved")
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":10,"message":{"message_id":1,"date":100,"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"hello","photo":[{"file_id":"small"},{"file_id":"big"}]}},{"update_id":11,"message":{"date":100,"from":{"id":99},"chat":{"id":99,"type":"private"},"text":"intruder"}},{"update_id":12,"message":{"date":100,"from":{"id":7},"chat":{"id":-20,"type":"supergroup"},"message_thread_id":3,"caption":"caption","document":{"file_id":"doc","file_name":"report.txt","mime_type":"text/plain"}}},{"update_id":13,"message":{"date":100,"from":{"id":7},"chat":{"id":-21,"type":"group"},"text":"unapproved group"}},{"update_id":14,"callback_query":{}}]}`)
	}))
	defer s.Close()
	tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: s.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tg.Poll(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 5 || out[4].Offset != 15 {
		t.Fatalf("lost ignored offsets: %+v", out)
	}
	if out[0].Incoming.Conversation.ID != "personal:7" || out[0].Incoming.Attachments[0].FileID != "big" {
		t.Error("bad personal scope/image")
	}
	if out[1].Incoming != nil || out[3].Incoming != nil || out[4].Incoming != nil {
		t.Error("accepted unauthorized update")
	}
	if out[2].Incoming.Conversation.ID != "telegram:group:-20:topic:3" || out[2].Incoming.Attachments[0].MIME != "text/plain" {
		t.Error("bad group or attachment")
	}
}
func TestTelegramSendChunkFilesAndRateLimit(t *testing.T) {
	texts := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "sendRichMessage"):
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if len([]rune(b["rich_message"].(map[string]any)["markdown"].(string))) > 32768 {
				t.Error("oversized chunk")
			}
			texts++
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		case strings.HasSuffix(r.URL.Path, "sendDocument"):
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			f, h, err := r.FormFile("document")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			data, _ := io.ReadAll(f)
			if h.Filename != "answer.txt" || string(data) != "answer" {
				t.Error("bad uploaded file")
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":2}}`)
		case strings.HasSuffix(r.URL.Path, "getFile"):
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"SECRET","parameters":{"retry_after":12}}`)
		}
	}))
	defer s.Close()
	tg, _ := NewTelegram(TelegramConfig{Token: "secret", BaseURL: s.URL, AllowedUserIDs: []int64{7}})
	c := domain.Conversation{OwnerID: 7, ChatID: 7, Private: true}
	if _, err := tg.Send(context.Background(), c, strings.Repeat("😀", 2000)); err != nil {
		t.Fatal(err)
	}
	if texts != 1 {
		t.Fatalf("expected one persistent outbox message, got %d", texts)
	}
	if _, err := tg.Send(context.Background(), c, strings.Repeat("😀", 32769)); err == nil {
		t.Fatal("oversized rich message accepted")
	}
	if _, err := tg.SendFile(context.Background(), c, "answer.txt", []byte("answer")); err != nil {
		t.Fatal(err)
	}
	_, err := tg.Download(context.Background(), "f")
	var apiErr *TelegramError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != 12*time.Second || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("bad rate-limit handling: %v", err)
	}
}
func TestTelegramDownloadSizeLimit(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "getFile") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"file_path":"documents/a.txt"}}`)
		} else {
			_, _ = io.WriteString(w, "too much content")
		}
	}))
	defer s.Close()
	tg, _ := NewTelegram(TelegramConfig{Token: "secret", BaseURL: s.URL, AllowedUserIDs: []int64{7}, MaxFileBytes: 4})
	if _, err := tg.Download(context.Background(), "f"); err == nil {
		t.Fatal("file limit bypassed")
	}
}

func TestOpenAIReplaysToolHistory(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role             string        `json:"role"`
				Content          string        `json:"content"`
				ReasoningContent *string       `json:"reasoning_content"`
				ToolCalls        []apiToolCall `json:"tool_calls"`
				ToolCallID       string        `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) != 2 || len(request.Messages[0].ToolCalls) != 1 || request.Messages[0].ToolCalls[0].Function.Arguments != `{"q":"go"}` || request.Messages[1].ToolCallID != "call" {
			t.Errorf("invalid replay %+v", request)
		}
		for _, message := range request.Messages {
			if message.ReasoningContent != nil {
				t.Error("DeepSeek-specific field sent to another model")
			}
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Done"},"finish_reason":"stop"}]}`)
	}))
	defer s.Close()
	model, _ := NewOpenAI(OpenAIConfig{BaseURL: s.URL, Model: "chosen"})
	_, err := model.Complete(context.Background(), []domain.Message{{Role: "assistant", Reasoning: "Internal reasoning from earlier history", ToolCalls: []domain.ToolCall{{ID: "call", Name: "search", Arguments: json.RawMessage(`{"q":"go"}`)}}}, {Role: "tool", ToolCallID: "call", Content: "result"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
}
func TestTranscriptionUsesConfiguredModelAndMultipart(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("model") != "speech-model" {
			t.Error("wrong transcription model")
		}
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		data, _ := io.ReadAll(f)
		if string(data) != "audio" || h.Filename != "voice.ogg" {
			t.Error("wrong transcription data")
		}
		_, _ = io.WriteString(w, `{"text":"Привет"}`)
	}))
	defer s.Close()
	m, _ := NewOpenAI(OpenAIConfig{BaseURL: s.URL, Model: "chat", TranscriptionModel: "speech-model"})
	text, err := m.Transcribe(context.Background(), "voice.ogg", []byte("audio"))
	if err != nil || text != "Привет" {
		t.Fatalf("transcription %q, %v", text, err)
	}
}
func TestTelegramRejectsOutgoingUnknownRecipient(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer s.Close()
	tg, _ := NewTelegram(TelegramConfig{Token: "secret", BaseURL: s.URL, AllowedUserIDs: []int64{7}})
	for _, c := range []domain.Conversation{{OwnerID: 8, ChatID: 8, Private: true}, {OwnerID: 7, ChatID: 8, Private: true}, {OwnerID: 7, ChatID: -1}} {
		if _, err := tg.Send(context.Background(), c, "text"); err == nil {
			t.Fatal("unknown outgoing recipient accepted")
		}
		if _, err := tg.SendFile(context.Background(), c, "file", nil); err == nil {
			t.Fatal("unknown file recipient accepted")
		}
	}
	if calls != 0 {
		t.Fatal("unauthorized message reached Telegram")
	}
}
func TestTelegramRejectsUnsafeDownloadPaths(t *testing.T) {
	for _, p := range []string{"../secret", "/absolute", "a/../../b", "a\\b", ""} {
		t.Run(p, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]string{"file_path": p}})
			}))
			defer s.Close()
			tg, _ := NewTelegram(TelegramConfig{Token: "secret", BaseURL: s.URL, AllowedUserIDs: []int64{7}})
			if _, err := tg.Download(context.Background(), "file"); err == nil {
				t.Fatal("unsafe path accepted")
			}
			if calls != 1 {
				t.Fatal("unsafe download path requested")
			}
		})
	}
}

func TestTelegramDeliveryOutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		unknown    bool
	}{
		{"truncated acknowledgement", `{"ok":true,`, 200, true},
		{"missing envelope", `{}`, 200, true},
		{"missing message ID", `{"ok":true,"result":{}}`, 200, true},
		{"server error", `{"ok":false,"error_code":500}`, 500, true},
		{"rate limit", `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`, 429, false},
		{"bad recipient", `{"ok":false,"error_code":403}`, 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer s.Close()
			tg, _ := NewTelegram(TelegramConfig{Token: "secret", BaseURL: s.URL, AllowedUserIDs: []int64{7}})
			_, err := tg.Send(context.Background(), domain.Conversation{OwnerID: 7, ChatID: 7, Private: true}, "hello")
			if err == nil {
				t.Fatal("expected send failure")
			}
			var unknown *TelegramDeliveryError
			got := errors.As(err, &unknown) && unknown.Unknown
			if got != tc.unknown {
				t.Fatalf("unknown=%v, want%v, err%v", got, tc.unknown, err)
			}
		})
	}
}
func TestTelegramSendTimeoutIsUnknown(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })}
	tg, _ := NewTelegram(TelegramConfig{Token: "secret", AllowedUserIDs: []int64{7}, HTTPClient: client})
	_, err := tg.Send(context.Background(), domain.Conversation{OwnerID: 7, ChatID: 7, Private: true}, "hello")
	var unknown *TelegramDeliveryError
	if !errors.As(err, &unknown) || !unknown.Unknown {
		t.Fatalf("expected unknown timeout, got%v", err)
	}
}
func TestOpenAIAllowsExplicitEmptyFinalStop(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`)
	}))
	defer s.Close()
	model, _ := NewOpenAI(OpenAIConfig{BaseURL: s.URL, Model: "chosen"})
	out, err := model.Complete(context.Background(), nil, nil)
	if err != nil || out.Message.Content != "" {
		t.Fatalf("empty final stop rejected: %+v %v", out, err)
	}
}

func TestWebReaderRemainsAvailableWithoutYandex(t *testing.T) {
	requests := 0
	search, err := NewSearch(YandexConfig{Web: WebConfig{Resolver: publicResolver(), HTTPClient: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: io.NopCloser(strings.NewReader("<html><body>Public linked page</body></html>")), Request: r}, nil
	})}}})
	if err != nil {
		t.Fatal(err)
	}
	if search.(interface{ SearchEnabled() bool }).SearchEnabled() {
		t.Fatal("search was enabled without credentials")
	}
	if _, err = search.Search(context.Background(), "anything"); !errors.Is(err, ErrSearchDisabled) {
		t.Fatalf("unexpected disabled result: %v", err)
	}
	if requests != 0 {
		t.Fatal("disabled search attempted an HTTP request")
	}
	text, err := search.Read(context.Background(), "https://example.com/page")
	if err != nil || !strings.Contains(text, "Public linked page") || requests != 1 {
		t.Fatalf("URL reading unavailable: %q %v requests=%d", text, err, requests)
	}
	if _, err = NewSearch(YandexConfig{APIKey: "key-only"}); err == nil {
		t.Fatal("partial provider credentials accepted")
	}
}

func TestOpenCodeGoUsesStableConversationHeaderAndOwnUserAgent(t *testing.T) {
	for _, base := range []string{"https://opencode.ai/inference/go/openai/v1", "https://opencode.ai/zen/go/v1", "https://api.openai.com/v1"} {
		t.Run(base, func(t *testing.T) {
			requests := 0
			model, err := NewOpenAI(OpenAIConfig{BaseURL: base, APIKey: "secret", Model: "test", HTTPClient: &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Header.Get("User-Agent") != "Jarvis/0.1" {
					t.Error("client identity missing")
				}
				want := ""
				if strings.Contains(base, "opencode.ai") {
					want = "opaque-conversation"
				}
				if req.Header.Get("x-opencode-session") != want {
					t.Error("conversation header missing or leaked to another provider")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)), Request: req}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := domain.WithModelSession(context.Background(), "opaque-conversation")
			for range 2 {
				if _, err = model.Complete(ctx, []domain.Message{{Role: "user", Content: "Hello"}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if requests != 2 {
				t.Fatal("unexpected requests")
			}
			if strings.Contains(base, "opencode.ai") {
				if _, err = model.Complete(context.Background(), nil, nil); err == nil {
					t.Fatal("OpenCode call without session accepted")
				}
				if requests != 2 {
					t.Fatal("unidentified request reached network")
				}
			}
		})
	}
}

func TestTelegramMarkdownAndTypingInGroupTopic(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			ChatID      int64               `json:"chat_id"`
			ThreadID    int                 `json:"message_thread_id"`
			Text        string              `json:"text"`
			Entities    []domain.TextEntity `json:"entities"`
			Action      string              `json:"action"`
			ParseMode   string              `json:"parse_mode"`
			RichMessage map[string]string   `json:"rich_message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if p.ChatID != -20 || p.ThreadID != 3 {
			t.Errorf("wrong topic %+v", p)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "sendRichMessage"):
			if p.RichMessage["markdown"] != "😀 **Привет**\n\n```go\nx < y\n```" || p.Text != "" || len(p.Entities) != 0 || p.ParseMode != "" {
				t.Errorf("original rich Markdown was not preserved: %+v", p)
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":7}}`)
		case strings.HasSuffix(r.URL.Path, "sendChatAction"):
			if p.Action != "typing" {
				t.Error("wrong chat action")
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected method %s", r.URL.Path)
		}
		calls++
	}))
	defer server.Close()
	tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
	if err != nil {
		t.Fatal(err)
	}
	c := domain.Conversation{ChatID: -20, OwnerID: 7, ThreadID: 3}
	if _, err = tg.Send(context.Background(), c, "😀 **Привет**\n\n```go\nx < y\n```"); err != nil {
		t.Fatal(err)
	}
	if err = tg.Typing(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err = tg.Typing(context.Background(), domain.Conversation{ChatID: 999, OwnerID: 7, Private: true}); err == nil {
		t.Fatal("unauthorized typing accepted")
	}
	if calls != 2 {
		t.Fatalf("unexpected requests %d", calls)
	}
}

func TestAllowedGroupAcceptsParticipantsWithoutOpeningPrivateAccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "getUpdates") {
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"message":{"date":100,"from":{"id":99,"first_name":"Саша","last_name":"Петров","username":"sasha99"},"chat":{"id":-20,"type":"supergroup","title":"Команда"},"text":"Я люблю пончики"}},{"update_id":2,"message":{"date":101,"from":{"id":99},"chat":{"id":99,"type":"private"},"text":"private unauthorized"}},{"update_id":3,"message":{"date":102,"from":{"id":99},"chat":{"id":-21,"type":"group"},"text":"unapproved group"}},{"update_id":4,"message":{"date":103,"from":{"id":99,"is_bot":true},"chat":{"id":-20,"type":"supergroup"},"text":"other bot"}}]}`)
		} else {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":123}}`)
		}
	}))
	defer server.Close()
	tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
	if err != nil {
		t.Fatal(err)
	}
	updates, err := tg.Poll(context.Background(), 0)
	if err != nil || len(updates) != 4 {
		t.Fatalf("poll: %v", err)
	}
	got := updates[0].Incoming
	if got == nil || got.Conversation.OwnerID != 99 || got.AuthorName != "Саша Петров" || got.AuthorUsername != "sasha99" || got.Conversation.Title != "Команда" {
		t.Fatalf("participant metadata lost: %+v", got)
	}
	for _, update := range updates[1:] {
		if update.Incoming != nil {
			t.Fatal("private/unknown group/bot accepted")
		}
	}
	if _, err = tg.Send(context.Background(), got.Conversation, "Групповой ответ"); err != nil {
		t.Fatalf("group reply to unlisted participant rejected: %v", err)
	}
}
