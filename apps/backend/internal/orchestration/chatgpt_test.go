package orchestration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"openalbert/apps/backend/internal/domain"
	"strconv"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/integrations/siwc"
)

type settingsPlan struct {
	*siwc.Client
	finishes int
}

func (p *settingsPlan) Finish(context.Context, string) error { p.finishes++; return nil }
func (p *settingsPlan) Status() siwc.Status {
	return siwc.Status{Connected: true, PlanEnabled: true, Model: "test"}
}
func TestChatGPTDeliveryFailureDoesNotReplayCallbackOrBlockPollOffset(t *testing.T) {
	for _, status := range []int{403, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var bodies []string
			r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				if status == 504 {
					bodies = append(bodies, "ambiguous proxy response")
					w.WriteHeader(504)
					_, _ = io.WriteString(w, "<html>Gateway timeout</html>")
					return
				}
				var payload struct {
					Text string `json:"text"`
				}
				_ = json.NewDecoder(req.Body).Decode(&payload)
				bodies = append(bodies, payload.Text)
				if len(bodies) == 1 || status == 403 {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"ok":false,"error_code":`+strconv.Itoa(status)+`}`)
					return
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
			})
			plan := &settingsPlan{}
			r.Telegram.ChatGPT = &integrations.ChatGPTControls{OwnerID: 7, Billing: &integrations.BillingModel{Mode: "auto", Plan: plan}}
			in := &domain.Incoming{Conversation: conv, Command: "chatgpt", Text: "/chatgpt callback http://127.0.0.1:1455/auth/callback?code=secret"}
			if err := r.handleChatGPTUpdate(context.Background(), integrations.Update{Offset: 9, Incoming: in}); err != nil {
				t.Fatal(err)
			}
			offset, _ := repo.PollOffset(context.Background())
			if offset != 9 || plan.finishes != 1 || len(bodies) == 0 || (status != 504 && !strings.Contains(bodies[0], "sign-in saved")) {
				t.Fatal("callback was replayed or offset blocked")
			}
			if status == 504 && len(bodies) != 1 {
				t.Fatal("retried an unknown Telegram acknowledgement")
			}
			if status == 503 && (len(bodies) != 2 || bodies[0] != bodies[1]) {
				t.Fatal("delivery retry reran the command instead of reusing its reply")
			}
		})
	}
}
