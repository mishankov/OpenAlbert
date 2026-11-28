package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/inspector"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInspectionLegacyOrderAndArtifactScopeSQLite(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLite(t)
	var err error
	id := fmt.Sprintf("inspection-scope-%d", time.Now().UnixNano())
	for _, session := range []string{id, id + "-other"} {
		if err = repo.StartSession(ctx, domain.Session{ID: session, ConversationID: id}); err != nil {
			t.Fatal(err)
		}
	}
	// Duplicate turn references must not duplicate historical traces. Similar
	// run-key prefixes in another session must not expose its model checkpoints.
	for _, turn := range []struct{ key, session, run string }{
		{id, id, id}, {id + "-duplicate", id, id}, {id + "-other", id + "-other", id + "-other"},
	} {
		raw, _ := json.Marshal(map[string]any{"Input": map[string]string{"SessionID": turn.session, "Key": turn.run}})
		if err = repo.SaveCheckpoint(ctx, "turn-done/"+turn.key, raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, run := range []string{id, id + "-other"} {
		for _, step := range []int{2, 10, 11, 12} {
			if err = repo.SaveCheckpoint(ctx, fmt.Sprintf("model/%s/%d", run, step), json.RawMessage(`{"Message":{"role":"assistant","content":"historical"}}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, step := range []int{10, 2} {
		if err = repo.AppendMessage(ctx, id, fmt.Sprintf("%s/assistant/%d", id, step), domain.Message{Role: "assistant", Content: id + "-referenced"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []int{2, 10} {
		status := "running"
		if step == 2 {
			status = "completed"
		}
		if err = repo.SaveTrace(ctx, domain.Trace{ID: fmt.Sprintf("%s-trace-%d", id, step), SessionID: id, TurnID: id, Step: step, Kind: "model", Status: status, StartedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, artifact := range []struct{ name, conversation, session string }{
		{"direct", id, id}, {"referenced", id, id + "-other"},
		{"unrelated", id, id + "-other"}, {"foreign", id + "-other", id},
	} {
		if err = repo.SaveArtifact(ctx, domain.Artifact{ID: id + "-" + artifact.name, SessionID: artifact.session, ConversationID: artifact.conversation, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	d, ok, err := repo.InspectSession(ctx, id, 0)
	if err != nil || !ok || len(d.Messages) != 2 {
		t.Fatalf("detail: found=%v err=%v", ok, err)
	}
	var historical []string
	for _, trace := range d.Traces {
		if trace.Historical {
			historical = append(historical, trace.ID)
		}
	}
	want := []string{"model/" + id + "/10", "model/" + id + "/11", "model/" + id + "/12"}
	if !reflect.DeepEqual(historical, want) || len(d.Traces) != len(want)+2 {
		t.Fatalf("historical order, scope or completed-trace deduplication: got %v, want %v; total=%d", historical, want, len(d.Traces))
	}
	// A later page must still include artifacts referenced by earlier messages.
	d, ok, err = repo.InspectSession(ctx, id, d.Messages[1].Seq)
	if err != nil || !ok || len(d.Messages) != 0 {
		t.Fatalf("later page: found=%v err=%v", ok, err)
	}
	var artifacts []string
	for _, artifact := range d.Artifacts {
		artifacts = append(artifacts, artifact.ID)
	}
	if want := []string{id + "-direct", id + "-referenced"}; !reflect.DeepEqual(artifacts, want) {
		t.Fatalf("artifact scope across pages: got %v, want %v", artifacts, want)
	}
}

func TestInspectionSQLite(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLite(t)
	var err error
	id := fmt.Sprintf("inspection-%d", time.Now().UnixNano())
	conv := domain.Conversation{ID: id, Title: "Личный чат", Private: true, ChatID: 7, OwnerID: 7}
	if _, err = repo.AcceptIncoming(ctx, domain.Incoming{ID: id, Conversation: conv}, 0); err != nil {
		t.Fatal(err)
	}
	if err = repo.StartSession(ctx, domain.Session{ID: id, ConversationID: id}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		m := domain.Message{Role: "user", Content: fmt.Sprintf("Сообщение %d", i)}
		if i == 0 {
			m = domain.Message{Role: "assistant", Reasoning: "Provider reasoning", ToolCalls: []domain.ToolCall{{ID: "c", Name: "memory_search", Arguments: json.RawMessage(`{"query":"пончики"}`)}}}
		}
		if err = repo.AppendMessage(ctx, id, fmt.Sprint(i), m); err != nil {
			t.Fatal(err)
		}
	}
	turn := json.RawMessage(fmt.Sprintf(`{"Input":{"SessionID":%q,"Key":%q,"Initiative":true},"Stats":{"ModelCalls":1}}`, id, id))
	if err = repo.SaveCheckpoint(ctx, "turn-done/"+id, turn); err != nil {
		t.Fatal(err)
	}
	key := "model/" + id + "/0"
	if err = repo.SaveCheckpoint(ctx, key, json.RawMessage(`{"Message":{"role":"assistant","content":"Молчу"},"InputTokens":100}`)); err != nil {
		t.Fatal(err)
	}
	legacy, ok, err := repo.InspectTrace(ctx, id, key)
	if err != nil || !ok || !legacy.Historical {
		t.Fatalf("legacy trace: %+v %v", legacy, err)
	}
	if _, ok, err = repo.InspectTrace(ctx, "another-session", key); err != nil || ok {
		t.Fatal("legacy trace escaped its session")
	}
	tr := domain.Trace{ID: id, SessionID: id, TurnID: id, Trigger: "initiative", Kind: "model", Name: "test-model", Status: "running", StartedAt: time.Now().UTC(), Input: json.RawMessage(`{"messages":[{"role":"system","content":"PRIVATE CONTEXT"}]}`)}
	if err = repo.SaveTrace(ctx, tr); err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC()
	tr.EndedAt = &end
	tr.Status = "completed"
	tr.Output = json.RawMessage(`{"Message":{"content":"внутренний итог"}}`)
	if err = repo.SaveTrace(ctx, tr); err != nil {
		t.Fatal(err)
	}
	full, ok, err := repo.InspectTrace(ctx, id, id)
	if err != nil || !ok || full.Status != "completed" || len(full.Input) == 0 {
		t.Fatalf("full trace: %+v %v", full, err)
	}
	if _, ok, err = repo.InspectTrace(ctx, "another", id); err != nil || ok {
		t.Fatal("trace escaped its session")
	}
	rows, err := repo.InspectSessions(ctx, inspector.Filter{Query: "пончики", Trigger: "initiative"})
	if err != nil || len(rows) != 1 || rows[0].MessageCount != 205 || rows[0].ToolCount != 1 {
		t.Fatalf("list: %+v %v", rows, err)
	}
	if rows, err = repo.InspectSessions(ctx, inspector.Filter{Query: id, Before: rows[0].Seq}); err != nil || len(rows) != 0 {
		t.Fatalf("session cursor ignored: %+v %v", rows, err)
	}
	d, ok, err := repo.InspectSession(ctx, id, 0)
	if err != nil || !ok || len(d.Messages) != 200 || d.NextAfter == 0 || len(d.Traces) != 1 || len(d.Traces[0].Input) != 0 || len(d.Traces[0].Output) != 0 || len(d.Turns) != 1 {
		t.Fatalf("detail pagination/metadata: %+v %v", d, err)
	}
	if d.Messages[0].Message.Reasoning != "Provider reasoning" || d.Messages[0].RecordedAt == nil {
		t.Fatal("reasoning or actual recording timestamp lost")
	}
	d2, ok, err := repo.InspectSession(ctx, id, d.NextAfter)
	if err != nil || !ok || len(d2.Messages) != 5 || d2.NextAfter != 0 || d2.Messages[0].Seq <= d.Messages[199].Seq {
		t.Fatalf("message cursor: %+v %v", d2, err)
	}
	if _, ok, err = repo.InspectSession(ctx, "missing", 0); err != nil || ok {
		t.Fatal("missing session not distinguished")
	}
}

func TestPendingAudioIsVisibleBeforeMessagePreparation(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLite(t)
	var e error
	id := fmt.Sprintf("pending-audio-%d", time.Now().UnixNano())
	conv := domain.Conversation{ID: id, OwnerID: 42, Private: true}
	received := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	caption := "Моя подпись [Voice transcript] <script>"
	_, e = repo.AcceptIncoming(ctx, domain.Incoming{ID: id, Conversation: conv, Text: caption, CreatedAt: received}, 0)
	if e != nil {
		t.Fatal(e)
	}
	if e = repo.StartSession(ctx, domain.Session{ID: id, ConversationID: id}); e != nil {
		t.Fatal(e)
	}
	if e = repo.SaveArtifact(ctx, domain.Artifact{ID: "source", SessionID: id, ConversationID: id, Name: "voice.ogg", MIME: "audio/ogg", Data: []byte("x")}); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(domain.AudioResult{SessionID: id, ConversationID: id, IncomingID: id, ArtifactID: "source", Status: "queued", AuthorID: 42, AuthorName: "Анна", QueuedAt: time.Now()})
	if e = repo.SaveCheckpoint(ctx, "audio/"+id, raw); e != nil {
		t.Fatal(e)
	}
	d, ok, e := repo.InspectSession(ctx, id, 0)
	if e != nil || !ok || len(d.Messages) != 0 || len(d.Audio) != 1 || len(d.Artifacts) != 1 || d.Audio[0].AuthorID != 42 {
		t.Fatalf("pending audio missing %+v %v", d, e)
	}
	if d.Audio[0].OriginalText == nil || *d.Audio[0].OriginalText != caption || d.Audio[0].ReceivedAt == nil || !d.Audio[0].ReceivedAt.Equal(received) {
		t.Fatal("original caption or Telegram timestamp lost")
	}
	// Even a mismatched source ID in a diagnostic projection cannot expose the
	// caption of another personal/group conversation.
	other := domain.Conversation{ID: id + "-other", OwnerID: 43, Private: true}
	_, e = repo.AcceptIncoming(ctx, domain.Incoming{ID: other.ID, Conversation: other, Text: "PRIVATE_OTHER_SCOPE"}, 0)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ = json.Marshal(domain.AudioResult{SessionID: id, ConversationID: id, IncomingID: other.ID, ArtifactID: "other-source", Status: "failed"})
	if e = repo.SaveCheckpoint(ctx, "audio/"+other.ID, raw); e != nil {
		t.Fatal(e)
	}
	d, _, e = repo.InspectSession(ctx, id, 0)
	if e != nil {
		t.Fatal(e)
	}
	for _, audio := range d.Audio {
		if audio.ArtifactID == "other-source" && (audio.OriginalText != nil || audio.ReceivedAt != nil) {
			t.Fatal("source metadata crossed conversation scope")
		}
	}
}

func TestMemoryAnalyticsMetadataSQLite(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLite(t)

	prefix := fmt.Sprintf("mem-analytics-%d", time.Now().UnixNano())
	ownerID := time.Now().UnixNano()
	scope := fmt.Sprintf("personal:%d", ownerID)
	otherScope := fmt.Sprintf("personal:%d", ownerID+1)
	codingConv := domain.Conversation{ID: "coding:" + prefix + "-mine", Title: "Coding", OwnerID: ownerID, Private: true}
	otherCodingConv := domain.Conversation{ID: "coding:" + prefix + "-other", Title: "Other coding", OwnerID: ownerID + 1, Private: true}
	if err := repo.SaveConversation(ctx, codingConv); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveConversation(ctx, otherCodingConv); err != nil {
		t.Fatal(err)
	}

	originStamp := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	for _, s := range []domain.Session{
		{ID: prefix + "-msg", ConversationID: scope, CreatedAt: originStamp},
		{ID: prefix + "-task-trace", ConversationID: scope, CreatedAt: originStamp.Add(time.Hour)},
		{ID: prefix + "-task-cp", ConversationID: scope, CreatedAt: originStamp.Add(2 * time.Hour)},
		{ID: prefix + "-init", ConversationID: scope, CreatedAt: originStamp.Add(3 * time.Hour)},
		{ID: prefix + "-coding", ConversationID: codingConv.ID, CreatedAt: originStamp.Add(4 * time.Hour)},
		{ID: prefix + "-other", ConversationID: otherScope},
		{ID: prefix + "-other-coding", ConversationID: otherCodingConv.ID},
	} {
		if err := repo.StartSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.CreateTask(ctx, domain.Task{ID: prefix + "-t1", ConversationID: scope, Title: "Дайджест новостей", Goal: "Собрать новости", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTask(ctx, domain.Task{ID: prefix + "-t2", ConversationID: scope, Title: "Проверка цен", Goal: "Проверить цены", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTask(ctx, domain.Task{ID: prefix + "-t-other", ConversationID: otherScope, Title: "Чужая задача", Goal: "Секрет", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if err := repo.SaveTrace(ctx, domain.Trace{
		ID: prefix + "-tr-task", SessionID: prefix + "-task-trace", TurnID: "turn-1",
		Trigger: "task", TaskID: prefix + "-t1", Kind: "model", Name: "m", Status: "completed", StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCheckpoint(ctx, "turn-done/"+prefix+"-cp-task", json.RawMessage(fmt.Sprintf(`{"Input":{"SessionID":%q,"TaskID":%q}}`, prefix+"-task-cp", prefix+"-t2"))); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCheckpoint(ctx, "turn-done/"+prefix+"-cp-init", json.RawMessage(fmt.Sprintf(`{"Input":{"SessionID":%q,"Initiative":true}}`, prefix+"-init"))); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTrace(ctx, domain.Trace{
		ID: prefix + "-tr-other", SessionID: prefix + "-other", TurnID: "turn-other",
		Trigger: "task", TaskID: prefix + "-t-other", Kind: "model", Name: "m", Status: "completed", StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	origins, err := repo.InspectSessionOrigins(ctx, scope, []string{
		prefix + "-msg",
		prefix + "-task-trace",
		prefix + "-task-cp",
		prefix + "-init",
		prefix + "-coding",
		prefix + "-other",
		prefix + "-other-coding",
		prefix + "-missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(origins) != 5 {
		t.Fatalf("expected 5 scoped origins (including owner coding session), got %+v", origins)
	}
	if got := origins[prefix+"-msg"]; got.Trigger != "message" || !got.CreatedAt.Equal(originStamp) {
		t.Fatalf("message trigger/created_at: %+v", got)
	}
	if got := origins[prefix+"-task-trace"]; got.Trigger != "task" || got.TaskID != prefix+"-t1" || got.TaskTitle != "Дайджест новостей" {
		t.Fatalf("trace task origin: %+v", got)
	}
	if got := origins[prefix+"-task-cp"]; got.Trigger != "task" || got.TaskID != prefix+"-t2" || got.TaskTitle != "Проверка цен" {
		t.Fatalf("checkpoint task origin: %+v", got)
	}
	if got := origins[prefix+"-init"]; got.Trigger != "initiative" || got.TaskID != "" {
		t.Fatalf("initiative origin: %+v", got)
	}
	if got := origins[prefix+"-coding"]; got.Trigger != "coding" {
		t.Fatalf("owner coding origin: %+v", got)
	}
	if _, leaked := origins[prefix+"-other"]; leaked {
		t.Fatal("other conversation session leaked across scope")
	}
	if _, leaked := origins[prefix+"-other-coding"]; leaked {
		t.Fatal("other owner coding session leaked across scope")
	}

	// Consolidation checkpoints: absent -> parsed timestamptz ordering (including fractional seconds) -> scope isolation.
	if _, _, found, err := repo.InspectLatestConsolidation(ctx, scope); err != nil || found {
		t.Fatalf("expected no consolidation yet: found=%v err=%v", found, err)
	}
	if err := repo.SaveCheckpoint(ctx, "memory-consolidation/"+prefix+"-whole-sec", json.RawMessage(fmt.Sprintf(`{"scope":%q,"completed_at":"2026-10-22T12:00:00Z","before_entries":30,"after_entries":25}`, scope))); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCheckpoint(ctx, "memory-consolidation/"+prefix+"-frac-sec", json.RawMessage(fmt.Sprintf(`{"scope":%q,"completed_at":"2026-10-22T12:00:00.500Z","before_entries":25,"after_entries":19}`, scope))); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCheckpoint(ctx, "memory-consolidation/"+prefix+"-other", json.RawMessage(fmt.Sprintf(`{"scope":%q,"completed_at":"2026-10-25T01:00:00Z","before_entries":99,"after_entries":1}`, otherScope))); err != nil {
		t.Fatal(err)
	}
	cid, raw, found, err := repo.InspectLatestConsolidation(ctx, scope)
	if err != nil || !found || cid != "memory-consolidation/"+prefix+"-frac-sec" {
		t.Fatalf("latest consolidation (fractional timestamp order): id=%q found=%v err=%v raw=%s", cid, found, err, raw)
	}
	// Also verify malformed completed_at timestamps do not fail inserts and sort NULLS LAST,
	// and explicit timezone offsets compare accurately in UTC.
	if err := repo.SaveCheckpoint(ctx, "memory-consolidation/"+prefix+"-bad-ts", json.RawMessage(fmt.Sprintf(`{"scope":%q,"completed_at":"2026-99-99T00:00:00Z"}`, scope))); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCheckpoint(ctx, "memory-consolidation/"+prefix+"-tz-offset", json.RawMessage(fmt.Sprintf(`{"scope":%q,"completed_at":"2026-10-22T15:00:01+03:00"}`, scope))); err != nil {
		t.Fatal(err)
	}
	cid, raw, found, err = repo.InspectLatestConsolidation(ctx, scope)
	if err != nil || !found || cid != "memory-consolidation/"+prefix+"-tz-offset" {
		t.Fatalf("latest consolidation (tz offset / bad timestamp): id=%q found=%v err=%v raw=%s", cid, found, err, raw)
	}
}

func TestInspectSessionOriginsIndexPlansSQLite(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLite(t)

	prefix := fmt.Sprintf("idx-plan-%d", time.Now().UnixNano())
	scope := "personal:" + prefix
	now := time.Now().UTC()

	tx, err := repo.beginImmediate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for g := 1; g <= 200; g++ {
		taskID := ""
		if g%10 == 0 {
			taskID = fmt.Sprintf("%s-t-%d", prefix, g)
		}
		initVal := "false"
		if g%10 == 1 {
			initVal = "true"
		}
		cp := fmt.Sprintf(`{"Input":{"SessionID":"%s-bg-%d","TaskID":%q,"Initiative":%s}}`, prefix, g, taskID, initVal)
		if _, err := upsertCheckpointSQL(tx.exec, ctx, fmt.Sprintf("turn-done/%s-%d", prefix, g), []byte(cp), true); err != nil {
			t.Fatal(err)
		}
		consAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(g) * time.Second)
		consCP := fmt.Sprintf(`{"scope":%q,"completed_at":%q}`, scope, consAt.Format(time.RFC3339Nano))
		if _, err := upsertCheckpointSQL(tx.exec, ctx, fmt.Sprintf("memory-consolidation/%s-%04d", prefix, g), []byte(consCP), true); err != nil {
			t.Fatal(err)
		}
		trigger := "message"
		trTask := ""
		if g%5 == 0 {
			trigger = "task"
			trTask = fmt.Sprintf("%s-task-%d", prefix, g)
		}
		if err := upsertTraceSQL(tx.exec, ctx, domain.Trace{
			ID:        fmt.Sprintf("%s-tr-%d", prefix, g),
			SessionID: fmt.Sprintf("%s-tr-sess-%d", prefix, g),
			TurnID:    "turn-1",
			Trigger:   trigger,
			TaskID:    trTask,
			Kind:      "model",
			Status:    "completed",
			StartedAt: now.Add(time.Duration(g) * time.Millisecond),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	traceTaskSess := prefix + "-tr-sess-5"
	traceMsgSess := prefix + "-tr-sess-1"
	cpTaskSess := prefix + "-bg-10"
	cpInitSess := prefix + "-bg-11"
	for _, sid := range []string{traceTaskSess, traceMsgSess, cpTaskSess, cpInitSess} {
		if err := repo.StartSession(ctx, domain.Session{ID: sid, ConversationID: scope, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.exec(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}

	idsJSON, _ := json.Marshal([]string{traceTaskSess, traceMsgSess, cpTaskSess, cpInitSess})
	explain := func(query string, args ...any) string {
		t.Helper()
		rows, err := repo.query(ctx, `EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatalf("EXPLAIN QUERY PLAN failed: %v", err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(lines, "\n")
	}

	originPlan := explain(inspectSessionOriginsSQL, scope, string(idsJSON))
	for _, wantIdx := range []string{
		"openalbert_traces_session_trigger",
		"openalbert_traces_session_task",
		"openalbert_checkpoints_turn_done_origin",
		"openalbert_checkpoints_turn_done_task",
	} {
		if !strings.Contains(originPlan, wantIdx) {
			t.Fatalf("InspectSessionOrigins plan missing index %s:\n%s", wantIdx, originPlan)
		}
	}

	latestPlan := explain(inspectLatestConsolidationSQL, scope)
	if !strings.Contains(latestPlan, "openalbert_checkpoints_consolidation_latest") {
		t.Fatalf("InspectLatestConsolidation did not use openalbert_checkpoints_consolidation_latest:\n%s", latestPlan)
	}
	if strings.Contains(latestPlan, "USE TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("InspectLatestConsolidation unexpectedly sorted rows:\n%s", latestPlan)
	}
}

func TestInspectSessionOriginsSchemaIndexesAndQueryContract(t *testing.T) {
	for _, want := range []string{
		"trigger_kind TEXT",
		"task_id TEXT",
		"CREATE INDEX IF NOT EXISTS openalbert_traces_session_trigger",
		"CREATE INDEX IF NOT EXISTS openalbert_traces_session_task",
		"turn_session_id TEXT",
		"turn_task_id TEXT",
		"turn_initiative INTEGER",
		"consolidation_scope TEXT",
		"consolidation_completed_at TEXT",
		"CREATE INDEX IF NOT EXISTS openalbert_checkpoints_turn_done_origin",
		"CREATE INDEX IF NOT EXISTS openalbert_checkpoints_turn_done_task",
		"CREATE INDEX IF NOT EXISTS openalbert_checkpoints_consolidation_latest",
		"ON openalbert_checkpoints(consolidation_scope, consolidation_completed_at DESC, id DESC)",
		"search_provider TEXT",
		"search_cost_usd REAL",
		"search_rate_limited INTEGER",
		"search_completed_at TEXT",
		"search_outcome_at TEXT",
		"CREATE INDEX IF NOT EXISTS openalbert_traces_search_usage",
	} {
		if !strings.Contains(schema, want) {
			t.Fatalf("schema.sql missing expected column/index clause %q", want)
		}
	}
	for _, want := range []string{
		"tr.trigger_kind",
		"tr.task_id",
		"cp.turn_session_id IS NOT NULL",
		"cp.turn_session_id = sc.session_id",
		"cp.turn_task_id",
		"cp.turn_initiative",
	} {
		if !strings.Contains(inspectSessionOriginsSQL, want) {
			t.Fatalf("inspectSessionOriginsSQL missing expected scalar/indexed lookup %q", want)
		}
	}
	if strings.Contains(inspectSessionOriginsSQL, "tr.data") || strings.Contains(inspectSessionOriginsSQL, "cp.data") {
		t.Fatalf("inspectSessionOriginsSQL must not read tr.data or cp.data JSON directly:\n%s", inspectSessionOriginsSQL)
	}
	if strings.Contains(sessionTriggerSQL, "tr.data") || strings.Contains(sessionTriggerSQL, "cp.data") {
		t.Fatalf("sessionTriggerSQL must not read tr.data or cp.data JSON directly:\n%s", sessionTriggerSQL)
	}
	if !strings.Contains(inspectLatestConsolidationSQL, "consolidation_scope = ?1") || !strings.Contains(inspectLatestConsolidationSQL, "ORDER BY consolidation_completed_at DESC, id DESC") {
		t.Fatalf("inspectLatestConsolidationSQL does not use indexed consolidation ordering:\n%s", inspectLatestConsolidationSQL)
	}
	if strings.Contains(inspectSearchUsageSQL, "data") {
		t.Fatalf("inspectSearchUsageSQL must read scalar columns, not JSON data:\n%s", inspectSearchUsageSQL)
	}
}

func TestInspectSearchUsageSQLite(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLite(t)

	base := time.Date(2031, 4, 1, 0, 0, 0, 0, time.UTC)
	cost := 0.007
	okCompleted := base.Add(48*time.Hour + 3*time.Second)
	boundaryCompleted := base.Add(time.Second)
	idPrefix := fmt.Sprintf("search-usage-%d", time.Now().UnixNano())
	for _, tr := range []domain.Trace{
		{
			ID: idPrefix + "-ok", SessionID: idPrefix, TurnID: "t1", Trigger: "message",
			Kind: "tool", Name: "web_search", Status: "completed",
			StartedAt: base.Add(48 * time.Hour),
			Search:    &domain.SearchTrace{Provider: "exa", Mode: "auto", ResolvedMode: "auto", CostUSD: &cost, HTTPStatus: 200, RateLimited: true, ResultCount: 5, CompletedAt: &okCompleted},
		},
		{
			ID: idPrefix + "-402", SessionID: idPrefix, TurnID: "t2", Trigger: "task", TaskID: "task-1",
			Kind: "tool", Name: "web_search", Status: "failed",
			StartedAt: base.Add(72 * time.Hour),
			Search:    &domain.SearchTrace{Provider: "exa", Mode: "lite", HTTPStatus: 402},
		},
		{
			ID: idPrefix + "-boundary-402", SessionID: idPrefix, TurnID: "t4", Trigger: "message",
			Kind: "tool", Name: "web_search", Status: "failed",
			StartedAt: base.Add(-time.Second),
			Search:    &domain.SearchTrace{Provider: "exa", Mode: "auto", HTTPStatus: 402, CompletedAt: &boundaryCompleted},
		},
		{
			ID: idPrefix + "-outside", SessionID: idPrefix, TurnID: "t3", Trigger: "message",
			Kind: "tool", Name: "web_search", Status: "completed",
			StartedAt: base.Add(-24 * time.Hour),
			Search:    &domain.SearchTrace{Provider: "exa", Mode: "auto", CostUSD: &cost, HTTPStatus: 200, ResultCount: 3},
		},
	} {
		if err := repo.SaveTrace(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := repo.InspectSearchUsage(ctx, base, base.AddDate(0, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	var matched []inspector.SearchUsageRecord
	for _, r := range rows {
		if strings.HasPrefix(r.TraceID, idPrefix) {
			matched = append(matched, r)
		}
	}
	if len(matched) != 3 {
		t.Fatalf("expected 3 in-window search traces (including UTC month-boundary outcome), got %+v", matched)
	}
	if matched[0].TraceID != idPrefix+"-402" || matched[0].HTTPStatus != 402 || matched[0].Trigger != "task" {
		t.Fatalf("unexpected newest record: %+v", matched[0])
	}
	if matched[1].TraceID != idPrefix+"-ok" || matched[1].CostUSD == nil || *matched[1].CostUSD != 0.007 || matched[1].ResultCount != 5 || !matched[1].RateLimited || !matched[1].CompletedAt.Equal(okCompleted) {
		t.Fatalf("unexpected ok record: %+v", matched[1])
	}
	if matched[2].TraceID != idPrefix+"-boundary-402" || matched[2].HTTPStatus != 402 || !matched[2].CompletedAt.Equal(boundaryCompleted) {
		t.Fatalf("UTC month-boundary 402 outcome missing or misordered: %+v", matched[2])
	}

	// Verify historical model/<turn>/<step> InspectTrace redacts web_search and codemode tool calls.
	turnKey := idPrefix + "-hist-turn"
	if err := repo.SaveCheckpoint(ctx, "turn-done/"+turnKey, json.RawMessage(`{"Input":{"Key":"`+turnKey+`","SessionID":"`+idPrefix+`"}}`)); err != nil {
		t.Fatal(err)
	}
	modelCheckpoint := json.RawMessage(`{"Message":{"role":"assistant","tool_calls":[{"id":"c1","name":"web_search","arguments":{"query":"HIST_SECRET_QUERY","include_domains":["hist.example"]}},{"id":"c2","name":"codemode","arguments":{"code":"searchTools('exa'); 'HIST_CM_SECRET'"}}]}}`)
	if err := repo.SaveCheckpoint(ctx, "model/"+turnKey+"/1", modelCheckpoint); err != nil {
		t.Fatal(err)
	}
	histTrace, found, err := repo.InspectTrace(ctx, idPrefix, "model/"+turnKey+"/1")
	if err != nil || !found {
		t.Fatalf("InspectTrace historical model lookup failed: found=%v err=%v", found, err)
	}
	histJSON := string(histTrace.Output)
	for _, forbidden := range []string{"HIST_SECRET_QUERY", "hist.example", "HIST_CM_SECRET"} {
		if strings.Contains(histJSON, forbidden) {
			t.Fatalf("historical InspectTrace leaked %q: %s", forbidden, histJSON)
		}
	}
}
