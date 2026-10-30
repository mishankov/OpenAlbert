package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
)

func TestTelegramRepliesReachModelAndHistory(t *testing.T) {
	var photo bytes.Buffer
	if err := png.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 16, 8))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, fields    string
		group           bool
		wantReply       *domain.ReplyContext
		wantQuote       *domain.MessageQuote
		wantAttachments []domain.Attachment
		unavailable     bool
	}{
		{name: "ordinary message"},
		{
			name:      "reply to bot from an earlier session",
			fields:    `,"reply_to_message":{"message_id":42,"from":{"id":99,"first_name":"Альберт","is_bot":true},"text":"Играбельно, но где-нибудь скачать тут не прокатит","reply_to_message":{"message_id":1,"text":"DO NOT INCLUDE NESTED REPLY"}}`,
			wantReply: &domain.ReplyContext{MessageID: 42, AuthorID: 99, AuthorName: "Альберт", AuthorIsBot: true, Text: "Играбельно, но где-нибудь скачать тут не прокатит"},
		},
		{
			name:      "selected passage with emoji before it",
			fields:    `,"reply_to_message":{"message_id":43,"from":{"id":8,"first_name":"Саша","last_name":"Иванов","username":"sasha"},"text":"😀 Я люблю чай. Я люблю кофе."},"quote":{"text":"Я люблю кофе.","position":17,"is_manual":true}`,
			group:     true,
			wantReply: &domain.ReplyContext{MessageID: 43, AuthorID: 8, AuthorName: "Саша Иванов", AuthorUsername: "sasha", Text: "😀 Я люблю чай. Я люблю кофе."},
			wantQuote: &domain.MessageQuote{Text: "Я люблю кофе.", Position: 17, IsManual: true},
		},
		{
			name:            "caption of referenced photo",
			fields:          `,"reply_to_message":{"message_id":44,"caption":"Карта памяти","photo":[{"file_id":"reply-small"},{"file_id":"reply-big"}]}`,
			wantReply:       &domain.ReplyContext{MessageID: 44, Text: "Карта памяти", MediaKinds: []string{"image"}},
			wantAttachments: []domain.Attachment{{ReplyToMessageID: 44, FileID: "reply-big", Name: "photo.jpg", MIME: "image/jpeg", Kind: "image"}},
		},
		{
			name:            "group reply with current and referenced photos",
			group:           true,
			fields:          `,"photo":[{"file_id":"own"}],"reply_to_message":{"message_id":44,"from":{"id":8,"first_name":"Саша"},"photo":[{"file_id":"reply-big"}],"reply_to_message":{"message_id":1,"photo":[{"file_id":"nested-do-not-download"}]}}`,
			wantReply:       &domain.ReplyContext{MessageID: 44, AuthorID: 8, AuthorName: "Саша", MediaKinds: []string{"image"}},
			wantAttachments: []domain.Attachment{{FileID: "own", Name: "photo.jpg", MIME: "image/jpeg", Kind: "image"}, {ReplyToMessageID: 44, FileID: "reply-big", Name: "photo.jpg", MIME: "image/jpeg", Kind: "image"}},
		},
		{
			name:            "referenced image document",
			fields:          `,"reply_to_message":{"message_id":47,"document":{"file_id":"reply-doc","file_name":"../map.png","mime_type":"image/png"}}`,
			wantReply:       &domain.ReplyContext{MessageID: 47, MediaKinds: []string{"document"}},
			wantAttachments: []domain.Attachment{{ReplyToMessageID: 47, FileID: "reply-doc", Name: "map.png", MIME: "image/png", Kind: "image"}},
		},
		{
			name:            "unavailable referenced photo",
			fields:          `,"reply_to_message":{"message_id":48,"photo":[{"file_id":"missing"}]}`,
			wantReply:       &domain.ReplyContext{MessageID: 48, MediaKinds: []string{"image"}},
			wantAttachments: []domain.Attachment{{ReplyToMessageID: 48, FileID: "missing", Name: "photo.jpg", MIME: "image/jpeg", Kind: "image"}},
			unavailable:     true,
		},
		{
			name:      "referenced non-image document",
			fields:    `,"reply_to_message":{"message_id":49,"document":{"file_id":"do-not-download","mime_type":"application/pdf"}}`,
			wantReply: &domain.ReplyContext{MessageID: 49, MediaKinds: []string{"document"}},
		},
		{
			name:      "referenced voice without caption",
			fields:    `,"reply_to_message":{"message_id":45,"voice":{"file_id":"do-not-transcribe"}}`,
			wantReply: &domain.ReplyContext{MessageID: 45, MediaKinds: []string{"voice"}},
		},
		{
			name:      "rich bot response",
			fields:    `,"reply_to_message":{"message_id":46,"from":{"id":99,"is_bot":true},"rich_message":{"blocks":[{"type":"paragraph","text":{"type":"plain","text":"Полный ответ бота"}}]}}`,
			wantReply: &domain.ReplyContext{MessageID: 46, AuthorID: 99, AuthorIsBot: true, Text: "[Rich message blocks]\n" + `{"blocks":[{"type":"paragraph","text":{"type":"plain","text":"Полный ответ бота"}}]}`},
		},
		{
			name:      "quote without accessible original",
			fields:    `,"quote":{"text":"Только доступная цитата","position":0}`,
			wantQuote: &domain.MessageQuote{Text: "Только доступная цитата"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			a, repo, _, _ := fixture(t)
			chat := `{"id":7,"type":"private"}`
			if tc.group {
				chat = `{"id":-20,"type":"supergroup"}`
			}
			downloads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/bottest/getFile":
					var request struct {
						FileID string `json:"file_id"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					found := false
					for _, at := range tc.wantAttachments {
						found = found || request.FileID == at.FileID
					}
					if !found {
						t.Errorf("unexpected attachment download: %s", request.FileID)
					}
					downloads++
					if tc.unavailable {
						_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"file unavailable"}`)
					} else {
						_, _ = io.WriteString(w, `{"ok":true,"result":{"file_path":"photo.png"}}`)
					}
					return
				case "/file/bottest/photo.png":
					_, _ = w.Write(photo.Bytes())
					return
				case "/bottest/getUpdates":
				default:
					t.Errorf("unexpected Telegram request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":100,"message":{"message_id":101,"from":{"id":7,"first_name":"Денис"},"chat":`+chat+`,"text":"😏🤭"`+tc.fields+`}}]}`)
			}))
			defer server.Close()
			tg, err := integrations.NewTelegram(integrations.TelegramConfig{Token: "test", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
			if err != nil {
				t.Fatal(err)
			}
			a.Messenger = tg
			updates, err := tg.Poll(ctx, 0)
			if err != nil || len(updates) != 1 || updates[0].Incoming == nil {
				t.Fatalf("poll: %+v %v", updates, err)
			}
			incoming := *updates[0].Incoming
			if downloads != 0 || !reflect.DeepEqual(incoming.Attachments, tc.wantAttachments) {
				t.Fatalf("poll must preserve attachment references without downloading: %+v downloads=%d", incoming.Attachments, downloads)
			}
			if incoming.Text != "😏🤭" || !reflect.DeepEqual(incoming.ReplyTo, tc.wantReply) || !reflect.DeepEqual(incoming.Quote, tc.wantQuote) {
				t.Fatalf("wrong reply target or selected passage: %+v reply=%+v quote=%+v", incoming, incoming.ReplyTo, incoming.Quote)
			}
			// Exercise durable inbox JSON serialization, also used by PostgreSQL.
			raw, err := json.Marshal(incoming)
			if err != nil {
				t.Fatal(err)
			}
			var restored domain.Incoming
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.AcceptIncoming(ctx, restored, updates[0].Offset); err != nil {
				t.Fatal(err)
			}
			input := RunInput{Key: "reply", SessionID: "fresh-session", ConversationID: incoming.Conversation.ID, IncomingIDs: []string{incoming.ID}}
			turn, err := a.Prepare(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := repo.Messages(ctx, input.SessionID)
			if err != nil || len(saved) != 1 || saved[0].AuthorID != 7 || saved[0].SourceMessageID != incoming.ID {
				t.Fatalf("history lost message attribution: %+v %v", saved, err)
			}
			content := saved[0].Content
			if !strings.HasPrefix(content, "😏🤭") || strings.Contains(content, "DO NOT INCLUDE NESTED REPLY") {
				t.Fatalf("current text lost or nested reply included: %s", content)
			}
			if tc.wantReply == nil && tc.wantQuote == nil {
				if content != incoming.Text {
					t.Fatalf("ordinary message changed: %s", content)
				}
			} else {
				var metadata struct {
					ReplyTo *domain.ReplyContext `json:"reply_to"`
					Quote   *domain.MessageQuote `json:"quote"`
				}
				_, data, _ := strings.Cut(content, "]\n")
				data, _, _ = strings.Cut(data, "\n")
				if err := json.Unmarshal([]byte(data), &metadata); err != nil {
					t.Fatalf("reply context is not delimited JSON: %s %v", content, err)
				}
				if !reflect.DeepEqual(metadata.ReplyTo, tc.wantReply) || !reflect.DeepEqual(metadata.Quote, tc.wantQuote) {
					t.Fatalf("history lost reply context: %s", content)
				}
			}
			wantImages := len(tc.wantAttachments)
			if tc.unavailable {
				wantImages = 0
			}
			if len(saved[0].Images) != wantImages || strings.Contains(content, "reply-big") || strings.Contains(content, "reply-doc") {
				t.Fatalf("missing images or transport file IDs leaked into model context: %+v", saved[0])
			}
			if tc.unavailable && !strings.Contains(content, "[Attachment unavailable]") {
				t.Fatal("download failure lost from reply context")
			}
			for i, at := range tc.wantAttachments {
				if at.ReplyToMessageID != 0 && !strings.Contains(content, "Following attachment belongs to Telegram reply_to message_id="+strconv.FormatInt(at.ReplyToMessageID, 10)) {
					t.Fatal("referenced image lost its attribution")
				}
				if tc.unavailable {
					continue
				}
				art, ok, err := repo.Artifact(ctx, incoming.Conversation.ID, ID(incoming.ID, strconv.Itoa(i)))
				if err != nil || !ok || !bytes.Equal(art.Data, photo.Bytes()) {
					t.Fatalf("original image was not saved: %+v %v", art, err)
				}
				decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(saved[0].Images[i], "data:image/jpeg;base64,"))
				if err != nil {
					t.Fatal(err)
				}
				cfg, _, err := image.DecodeConfig(bytes.NewReader(decoded))
				if err != nil || cfg.Width != 16 || cfg.Height != 8 {
					t.Fatalf("invalid model image: %+v %v", cfg, err)
				}
			}
			a.Model = &testModel{respond: func(messages []domain.Message) (domain.Message, error) {
				last := messages[len(messages)-1]
				if last.Role != "user" || !strings.HasSuffix(last.Content, content) || len(last.Images) != wantImages {
					t.Fatalf("model did not receive persisted reply context: %+v", last)
				}
				return domain.Message{Role: "assistant", Content: "ok"}, nil
			}}
			if _, err := a.ModelCall(ctx, ModelInput{Turn: turn}); err != nil {
				t.Fatal(err)
			}
			history, err := repo.ConversationHistory(ctx, incoming.Conversation.ID, "", 0, 10)
			if err != nil || len(history) != 1 || history[0].Content != content {
				t.Fatalf("retrievable history lost reply context: %+v %v", history, err)
			}
			if _, err := a.Prepare(ctx, input); err != nil {
				t.Fatal(err)
			}
			if !tc.unavailable && downloads != len(tc.wantAttachments) {
				t.Fatalf("retry downloaded images again: %d", downloads)
			}
			if tc.group && tc.wantQuote != nil && a.participantEvidence(ctx, incoming.Conversation.ID, 7, incoming.ID, tc.wantQuote.Text) == nil {
				t.Fatal("quoted participant's words accepted as current sender's self-statement")
			}
		})
	}
}

func TestReplyContextIsBoundedWithoutLosingSelectedPassage(t *testing.T) {
	msg := domain.Incoming{
		Text:    strings.Repeat("😏", 17000),
		ReplyTo: &domain.ReplyContext{MessageID: 42, Text: strings.Repeat("я", 20000)},
		Quote:   &domain.MessageQuote{Text: "Нужный фрагмент в конце", Position: 19000, IsManual: true},
	}
	content := incomingText(msg)
	if !utf8.ValidString(content) || !strings.Contains(content, msg.Quote.Text) || strings.Contains(content, strings.Repeat("я", 16001)) || strings.Contains(content, strings.Repeat("😏", 16001)) {
		t.Fatal("large reply is unbounded, invalid UTF-8 or lost the selected passage")
	}
	if len([]rune(msg.ReplyTo.Text)) != 20000 {
		t.Fatal("formatting mutated the durable source snapshot")
	}
}
