package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mishankov/OpenAlbert/shared/backup"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/integrations/siwc"
	"openalbert/apps/backend/internal/telemetry"
)

type roleModel struct {
	roles []domain.ModelRole
	turns []string
	err   error
}

func (m *roleModel) Complete(ctx context.Context, _ []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	role := domain.ModelRoleFromContext(ctx)
	m.roles = append(m.roles, role)
	m.turns = append(m.turns, domain.ModelTurn(ctx))
	content := "internal result"
	if role == domain.RoleAnalysis {
		if len(tools) != 0 {
			return domain.Completion{}, errors.New("analysis unexpectedly received tools")
		}
		content = `{"summary":"Closed conversation summary","memories":[]}`
	}
	cost := 0.001
	return domain.Completion{
		Message: domain.Message{Role: "assistant", Content: content}, InputTokens: 20, OutputTokens: 5,
		Usage: domain.ModelUsage{Role: role, Profile: "configured-" + string(role), Model: "actual-model", TokenUsageKnown: true, CachedInputTokens: 8, EstimatedCostUSD: &cost},
	}, m.err
}

func TestApplicationSelectsRoleByOperationAndReusesCompletionMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   RunInput
		role domain.ModelRole
	}{
		{name: "interactive", role: domain.RoleChat},
		{name: "scheduled analysis is background", in: RunInput{TaskID: "analyze-logs"}, role: domain.RoleBackground},
		{name: "initiative", in: RunInput{Initiative: true}, role: domain.RoleBackground},
		{name: "backup event", in: RunInput{BackupEvent: &backup.Event{}}, role: domain.RoleBackground},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			model := &roleModel{}
			a.Model = model
			capture := &tracedRepo{Repository: repo}
			a.Repo = capture
			var logs bytes.Buffer
			a.Logger = telemetry.New(&logs)
			in := tc.in
			in.Key, in.SessionID, in.ConversationID = "run", "session", conv.ID
			input := ModelInput{Turn: Turn{Input: in, Conversation: conv, Messages: []domain.Message{{Role: "user", TurnID: in.Key, Content: "Analyze this question"}}}}
			first, err := a.ModelCall(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			second, err := a.ModelCall(context.Background(), input)
			if err != nil || first.Usage.Profile != second.Usage.Profile || second.InputTokens != 20 {
				t.Fatalf("checkpoint lost result metadata: %+v %v", second, err)
			}
			if len(model.roles) != 1 || model.roles[0] != tc.role || model.turns[0] != "run" {
				t.Fatalf("wrong selection or repeated inference: %+v %+v", model.roles, model.turns)
			}
			if len(capture.traces) != 2 || capture.traces[1].Name != "actual-model" || capture.traces[1].Profile != first.Usage.Profile || capture.traces[1].ModelRole != tc.role {
				t.Fatalf("inspection lost actual model: %+v", capture.traces)
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("unexpected inference logs: %s", logs.String())
			}
			for i, line := range lines {
				var event map[string]any
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatal(err)
				}
				if event["model_role"] != string(tc.role) || event["model"] != "actual-model" || event["model_profile"] != first.Usage.Profile {
					t.Fatalf("usage labels lost: %v", event)
				}
				if i == 0 && (event["input_tokens"] != float64(20) || event["cached_input_tokens"] != float64(8) || event["estimated_cost_usd"] == nil) {
					t.Fatalf("missing provider usage: %v", event)
				}
				if i == 1 && (event["cached"] != true || event["input_tokens"] != float64(0) || event["output_tokens"] != float64(0) || event["cached_input_tokens"] != float64(0) || event["estimated_cost_usd"] != nil) {
					t.Fatalf("checkpoint reuse counted as new provider usage: %v", event)
				}
			}
		})
	}
}

func TestClosedSessionAnalysisExplicitlySelectsAnalysisRole(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	session := domain.Session{ID: "closed", ConversationID: conv.ID}
	if err := repo.StartSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendMessage(ctx, session.ID, "question", domain.Message{Role: "user", Content: "A conversation to summarize"}); err != nil {
		t.Fatal(err)
	}
	model := &roleModel{}
	a.Model = model
	for range 2 {
		if err := a.Analyze(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	if len(model.roles) != 1 || model.roles[0] != domain.RoleAnalysis {
		t.Fatalf("wrong role or duplicate analysis: %v", model.roles)
	}
}

func TestFailedInferenceRetainsSelectedProfileInInspection(t *testing.T) {
	a, repo, _, conv := fixture(t)
	a.Model = &roleModel{err: errors.New("provider unavailable")}
	capture := &tracedRepo{Repository: repo}
	a.Repo = capture
	_, err := a.ModelCall(context.Background(), ModelInput{Turn: Turn{Input: RunInput{Key: "failed", ConversationID: conv.ID}}})
	if err == nil || len(capture.traces) != 2 || capture.traces[1].Status != "failed" || capture.traces[1].Profile != "configured-chat" || capture.traces[1].Name != "actual-model" {
		t.Fatalf("failed inference lost selection: %+v %v", capture.traces, err)
	}
}

func TestPrepareMarksOriginalRequestAndSyntheticBackgroundInstructions(t *testing.T) {
	for _, background := range []bool{false, true} {
		a, repo, _, conv := fixture(t)
		ctx := context.Background()
		in := RunInput{Key: "marked", SessionID: "marked-session", ConversationID: conv.ID, IncomingIDs: []string{"seed"}, OriginalIncomingID: "seed", Initiative: background}
		turn, err := a.Prepare(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, message := range turn.Messages {
			if message.Role == "user" {
				found = true
				if message.TurnID != in.Key {
					t.Fatalf("current request lacks context protection: %+v", message)
				}
			}
		}
		if !found {
			t.Fatal("missing current request")
		}
		history, err := repo.Messages(ctx, in.SessionID)
		if err != nil || len(history) != 1 || history[0].TurnID != in.Key {
			t.Fatalf("incoming marker not durable: %+v %v", history, err)
		}
	}
}

func TestPreparePreservesCurrentTurnDuringHistoryBounding(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	in := RunInput{Key: "current", SessionID: "long-turn", ConversationID: conv.ID}
	if err := repo.StartSession(ctx, domain.Session{ID: in.SessionID, ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	request := domain.Message{Role: "user", TurnID: in.Key, Content: "Compare all four images", Images: []string{"image1", "image2", "image3", "image4"}}
	if err := repo.AppendMessage(ctx, in.SessionID, "original", request); err != nil {
		t.Fatal(err)
	}
	for i := range 55 {
		id := fmt.Sprint(i)
		if err := repo.AppendMessage(ctx, in.SessionID, "call/"+id, domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: id, Name: "lookup", Arguments: json.RawMessage(`{}`)}}}); err != nil {
			t.Fatal(err)
		}
		if err := repo.AppendMessage(ctx, in.SessionID, "result/"+id, domain.Message{Role: "tool", ToolCallID: id, Content: "Evidence"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.AppendMessage(ctx, in.SessionID, "view", domain.Message{Role: "user", Content: "Artifact view"}); err != nil {
		t.Fatal(err)
	}
	turn, err := a.Prepare(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range turn.Messages {
		if msg.TurnID == in.Key && msg.Content == request.Content {
			if len(msg.Images) != len(request.Images) {
				t.Fatalf("current images silently removed: %v", msg.Images)
			}
			return
		}
	}
	t.Fatal("current request silently removed before profile context fitting")
}

func httpRoutingModel(t *testing.T, endpoint string, window int, vision bool) *integrations.ModelRouter {
	t.Helper()
	capabilities := []string{"tools", "structured_output"}
	if vision {
		capabilities = append(capabilities, "vision")
	}
	model, err := integrations.NewModelRouter(domain.ModelProfiles{
		Routes:   map[domain.ModelRole]string{domain.RoleChat: "test", domain.RoleBackground: "test", domain.RoleAnalysis: "test"},
		Profiles: map[string]domain.ModelProfile{"test": {Adapter: "openai_chat_completions", BaseURL: endpoint, Model: "test", APIKey: "synthetic", Capabilities: capabilities, ContextWindowTokens: window, MaxOutputTokens: 1000, Timeout: time.Second}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestTemporalAndProfileCompactionRemoveWholeHistoricalTurns(t *testing.T) {
	var sent []domain.Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []domain.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		sent = body.Messages
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	messages := []domain.Message{{Role: "system", Content: "Rules"}, {Role: "user", TurnID: "old", Content: "Old batched request"}, {Role: "user", TurnID: "old", Content: "Second part"}}
	for i := range 26 {
		id := fmt.Sprint(i)
		messages = append(messages, domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: id, Name: "lookup", Arguments: json.RawMessage(`{}`)}}}, domain.Message{Role: "tool", ToolCallID: id, Content: strings.Repeat("x", 32000)})
	}
	messages = append(messages, domain.Message{Role: "user", Content: "Old artifact view"}, domain.Message{Role: "user", TurnID: "later-old", Content: "Another old turn"}, domain.Message{Role: "assistant", Content: strings.Repeat("y", 20000)})
	current := []domain.Message{{Role: "user", TurnID: "current", Content: "Current request"}, {Role: "user", TurnID: "current", Content: "Current batch part"}, {Role: "user", Content: "Current artifact view"}}
	messages = append(messages, current...)
	compact := CompactContext(messages, "current")
	if len(compact) != 6 || compact[1].TurnID != "later-old" {
		t.Fatalf("Temporal pass split a historical turn: %d messages", len(compact))
	}
	model := httpRoutingModel(t, server.URL, 6000, true)
	ctx := domain.WithModelTurn(domain.WithModelRole(context.Background(), domain.RoleChat), "current")
	out, err := model.Complete(ctx, compact, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 4 || sent[1].Content != current[0].Content || sent[3].Content != current[2].Content || out.Usage.DroppedMessages != 2 {
		t.Fatalf("profile pass lost the current turn or retained old evidence: %+v", sent)
	}
}

func TestTextOnlySessionRecoversAfterUnsupportedImage(t *testing.T) {
	a, repo, _, conv := fixture(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Messages []domain.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		priorPresent := false
		for _, msg := range body.Messages {
			priorPresent = priorPresent || msg.Content == "Earlier text context"
			if len(msg.Images) > 0 || strings.Contains(msg.Content, "Image question") {
				t.Error("unsupported historical image turn reached provider")
			}
		}
		if !priorPresent {
			t.Error("text before the unsupported image was unnecessarily evicted")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	a.Model = httpRoutingModel(t, server.URL, 128000, false)
	ctx := context.Background()
	if err := repo.StartSession(ctx, domain.Session{ID: "images", ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendMessage(ctx, "images", "prior", domain.Message{Role: "user", TurnID: "prior", Content: "Earlier text context"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendMessage(ctx, "images", "image", domain.Message{Role: "user", TurnID: "image", Content: "Image question", Images: []string{"data:image/jpeg;base64,YQ=="}}); err != nil {
		t.Fatal(err)
	}
	turn, err := a.Prepare(ctx, RunInput{Key: "image", SessionID: "images", ConversationID: conv.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ModelCall(ctx, ModelInput{Turn: turn}); err == nil || requests != 0 {
		t.Fatal("unsupported current image was not rejected")
	}
	if _, err = repo.AcceptIncoming(ctx, domain.Incoming{ID: "text", Conversation: conv, Text: "Text question"}, 2); err != nil {
		t.Fatal(err)
	}
	turn, err = a.Prepare(ctx, RunInput{Key: "text", SessionID: "images", ConversationID: conv.ID, IncomingIDs: []string{"text"}, OriginalIncomingID: "text"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.ModelCall(ctx, ModelInput{Turn: turn})
	if err != nil || requests != 1 || out.Usage.DroppedMessages != 1 {
		t.Fatalf("image poisoned following text request: %+v %v", out, err)
	}
}

func TestAnalysisFitsCompleteNewestRecordsAndKeepsFullDeletionCheck(t *testing.T) {
	a, repo, k, conv := fixture(t)
	var payload analysisContext
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Messages []domain.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Messages) != 2 {
			t.Errorf("analysis prompt missing: %d", len(body.Messages))
			return
		}
		if err := json.Unmarshal([]byte(body.Messages[1].Content), &payload); err != nil {
			t.Error("analysis truncated a JSON record:", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"Recent messages\",\"memories\":[{\"content\":\"Forgotten fact\",\"kind\":\"fact\"}]}"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	a.Model = &integrations.BillingModel{Mode: "api_key", API: httpRoutingModel(t, server.URL, 64000, false)}
	transcript := []domain.Message{{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "forget", Name: "memory_update", Arguments: json.RawMessage(`{"delete":true}`)}}}}
	for i := range 10 {
		transcript = append(transcript, domain.Message{Role: "user", Content: fmt.Sprintf("entry-%d ", i) + strings.Repeat("quoted \"record\" ", 1000)})
	}
	session := closedAnalysisSession(t, a, repo, conv.ID, transcript...)
	if err := a.Analyze(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(payload.Transcript) < 1 || len(payload.Transcript) >= len(transcript) || payload.Transcript[len(payload.Transcript)-1].Content != transcript[len(transcript)-1].Content {
		t.Fatalf("analysis did not retain complete newest records: %d", len(payload.Transcript))
	}
	if payload.Transcript[0].Role != "user" {
		t.Fatal("old deletion record was not evicted by the context limit")
	}
	memories, err := k.Search(context.Background(), conv.ID, "")
	if err != nil || len(memories) != 0 {
		t.Fatalf("deletion outside retained transcript was ignored: %+v %v", memories, err)
	}
}

type analysisPlanModel struct{ *siwc.Client }

func (*analysisPlanModel) Status() siwc.Status { return siwc.Status{Model: "plan-model"} }

func TestPlanOnlyAnalysisFitsWholeRecordsIndependentlyOfAPIProfile(t *testing.T) {
	api := httpRoutingModel(t, "http://127.0.0.1:1", 5000, false)
	newest := domain.Message{Role: "user", Content: "newest " + strings.Repeat("record ", 2000)}
	transcript := []domain.Message{}
	for range 8 {
		transcript = append(transcript, domain.Message{Role: "user", Content: strings.Repeat("older ", 2000)})
	}
	transcript = append(transcript, newest)
	for _, mode := range []string{"siwc", "api_key", "auto"} {
		t.Run(mode, func(t *testing.T) {
			a := &Activities{Model: &integrations.BillingModel{Mode: mode, API: api, Plan: &analysisPlanModel{}}}
			messages, fitted, err := a.fitAnalysisContext(context.Background(), "Analyze this session", analysisContext{Transcript: transcript})
			if mode != "siwc" {
				if err == nil {
					t.Fatal("API lane accepted a newest record larger than its budget")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(fitted.Transcript) < 1 || len(fitted.Transcript) >= len(transcript) || fitted.Transcript[len(fitted.Transcript)-1].Content != newest.Content {
				t.Fatalf("lost newest record or failed to bound plan payload: %d records", len(fitted.Transcript))
			}
			var decoded analysisContext
			if err := json.Unmarshal([]byte(messages[1].Content), &decoded); err != nil || len(decoded.Transcript) != len(fitted.Transcript) {
				t.Fatalf("partial records in plan request: %v", err)
			}
		})
	}
}

func TestTransportDropsWholeHistoricalImageTurnsBeforeTextOnlyRouting(t *testing.T) {
	priorText := strings.Repeat("earlier text ", 3500)
	var sent []domain.Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []domain.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		sent = body.Messages
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	messages := []domain.Message{{Role: "system", Content: "Rules"}, {Role: "user", TurnID: "text-A", Content: priorText}, {Role: "assistant", Content: "Earlier reply"}}
	for i := range 3 {
		messages = append(messages, domain.Message{Role: "user", TurnID: fmt.Sprintf("image-%d", i), Content: "Historical image question", Images: []string{strings.Repeat("a", 250000)}}, domain.Message{Role: "assistant", Content: "Historical image answer"})
	}
	messages = append(messages, domain.Message{Role: "user", TurnID: "current", Content: "Text C"})
	compact := CompactContext(messages, "current")
	if len(compact) != 6 || compact[1].Content != priorText || len(compact[3].Images) != 1 {
		t.Fatalf("transport clipped text or retained partial image turns: %d messages", len(compact))
	}
	model := httpRoutingModel(t, server.URL, 128000, false)
	ctx := domain.WithModelTurn(domain.WithModelRole(context.Background(), domain.RoleChat), "current")
	if _, err := model.Complete(ctx, compact, nil); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 4 || sent[1].Content != priorText || sent[2].Content != "Earlier reply" || sent[3].Content != "Text C" {
		t.Fatalf("image question/reply leaked as text, or earlier text lost: %d messages", len(sent))
	}
}

func TestAnalysisRefillsTranscriptAfterTrimmingMemorySnapshot(t *testing.T) {
	a := &Activities{Model: httpRoutingModel(t, "http://127.0.0.1:1", 6000, false)}
	latest := domain.Message{Role: "user", Content: "Remember that I prefer morning meetings."}
	_, fitted, err := a.fitAnalysisContext(context.Background(), "Summarize", analysisContext{
		Existing:   []domain.MemoryEntry{{Content: strings.Repeat("large old memory ", 1000)}},
		Transcript: []domain.Message{latest, {Role: "assistant"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fitted.Existing) != 0 || len(fitted.Transcript) != 2 || fitted.Transcript[0].Content != latest.Content {
		t.Fatalf("analysis discarded useful transcript after freeing memory budget: %+v", fitted)
	}
}

func TestUnmarkedHistoricalImageBatchLeavesNoDependentText(t *testing.T) {
	var sent []domain.Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []domain.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		sent = body.Messages
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	for _, size := range []int{1000, 450000} {
		messages := []domain.Message{
			{Role: "system", Content: "Rules"},
			// No recoverable boundaries separate the legacy batch parts.
			{Role: "user", Content: "Image from old batch", Images: []string{strings.Repeat("a", size)}},
			{Role: "user", Content: "What does the same image show?"},
			{Role: "assistant", Content: "Dependent image answer"},
			{Role: "user", TurnID: "prior", Content: "Earlier marked text"},
			{Role: "assistant", Content: "Prior answer"},
			{Role: "user", TurnID: "current", Content: "Current text"},
		}
		compact := CompactContext(messages, "current")
		model := httpRoutingModel(t, server.URL, 128000, false)
		ctx := domain.WithModelTurn(domain.WithModelRole(context.Background(), domain.RoleChat), "current")
		if _, err := model.Complete(ctx, compact, nil); err != nil {
			t.Fatal(err)
		}
		for _, m := range sent {
			if strings.Contains(m.Content, "old batch") || strings.Contains(m.Content, "same image") || strings.Contains(m.Content, "Dependent image") {
				t.Fatalf("legacy image batch leaked bare text (size%d): %+v", size, m)
			}
		}
		if len(sent) != 4 || sent[1].Content != "Earlier marked text" || sent[3].Content != "Current text" {
			t.Fatalf("legacy block eviction lost independent text: %d messages", len(sent))
		}
	}
}

func TestQueuedLegacySteeringPrepareDoesNotInventTurnBoundary(t *testing.T) {
	for _, size := range []int{600000, 800000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			ctx := context.Background()
			const session = "queued-steering"
			if err := repo.StartSession(ctx, domain.Session{ID: session, ConversationID: conv.ID}); err != nil {
				t.Fatal(err)
			}
			legacy := []domain.Message{
				{Role: "user", SourceMessageID: "original", Content: strings.Repeat("x", size)},
				{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "read", Name: "web_read", Arguments: json.RawMessage(`{}`)}}},
				{Role: "tool", ToolCallID: "read", Content: "Original tool evidence"},
			}
			for i, m := range legacy {
				if err := repo.AppendMessage(ctx, session, fmt.Sprintf("old-%d", i), m); err != nil {
					t.Fatal(err)
				}
			}
			for i, id := range []string{"followup", "next"} {
				if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: id, Conversation: conv, Text: id}, int64(10+i)); err != nil {
					t.Fatal(err)
				}
			}
			// This is the old queued activity shape: only new follow-up IDs and
			// no explicit original ID, even though the activity has a known key.
			turn, err := a.Prepare(ctx, RunInput{Key: "spanning", SessionID: session, ConversationID: conv.ID, IncomingIDs: []string{"followup"}})
			if err != nil {
				t.Fatal(err)
			}
			if size > 750000 {
				if len(turn.Messages) != 1 || !turn.Messages[0].ContextOverflow {
					t.Fatal("ambiguous queued payload discarded evidence instead of failing explicitly")
				}
			} else {
				if domain.ModelTurnStart(turn.Messages, "spanning") != 1 || len(turn.Messages) != 5 || len(turn.Messages[1].Content) != size {
					t.Fatal("old follow-up-only payload failed to protect its ambiguous original")
				}
			}
			stored, err := repo.Messages(ctx, session)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range stored {
				if m.TurnID != "" {
					t.Fatal("queued legacy follow-up invented durable boundary")
				}
			}
			if err := repo.AppendMessage(ctx, session, "old-reply", domain.Message{Role: "assistant", Content: "Dependent follow-up reply"}); err != nil {
				t.Fatal(err)
			}
			var sent []domain.Message
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []domain.Message `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				sent = body.Messages
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()
			a.Model = httpRoutingModel(t, server.URL, 128000, false)
			next, err := a.Prepare(ctx, RunInput{Key: "independent", SessionID: session, ConversationID: conv.ID, IncomingIDs: []string{"next"}, OriginalIncomingID: "next"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.ModelCall(ctx, ModelInput{Turn: next}); err != nil {
				t.Fatal(err)
			}
			if len(sent) != 2 || sent[1].Content != "next" {
				t.Fatalf("next independent turn received a partial legacy turn: %d messages", len(sent))
			}
		})
	}
}
