package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestProgressRepositoryContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { progressContract(t, NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
		if url == "" {
			t.Skip("set OPENALBERT_TEST_DATABASE_URL for PostgreSQL")
		}
		s, err := Open(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		progressContract(t, s)
	})
}

func progressContract(t *testing.T, repo domain.Repository) {
	ctx := context.Background()
	now := time.Now().UTC()
	id := fmt.Sprintf("progress-%d", now.UnixNano())
	conv := domain.Conversation{ID: id + "/chat", ChatID: 7, OwnerID: 7, Private: true}
	p := domain.Progress{TurnID: id, Conversation: conv, StartedAt: now.Add(-time.Minute)}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	claims := make(chan bool, 8)
	for range 8 {
		go func() {
			claimed, err := repo.ClaimCheckpoint(ctx, id+"/send-claim", []byte(`true`))
			if err != nil {
				t.Error(err)
			}
			claims <- claimed
		}()
	}
	winners := 0
	for range 8 {
		if <-claims {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("send claim has %d winners", winners)
	}
	check(repo.StartProgress(ctx, p))
	check(repo.ReportProgress(ctx, id, 3, "current"))
	check(repo.StartProgress(ctx, p)) // Retried Prepare must not erase public text or delivery metadata.
	check(repo.ReportProgress(ctx, id, 1, "stale"))
	find := func(want string) *domain.Progress {
		t.Helper()
		rows, err := repo.DueProgress(ctx, now, 1000)
		check(err)
		for _, row := range rows {
			if row.TurnID == want {
				return &row
			}
		}
		return nil
	}
	row := find(id)
	if row == nil || row.Text != "current" {
		t.Fatal("Prepare/retry erased newer progress")
	}
	claimed, ok, err := repo.SetProgressDelivery(ctx, id, row.Delivery.Version, domain.ProgressDelivery{State: "sending"})
	check(err)
	if !ok {
		t.Fatal("could not claim status")
	}
	_, ok, err = repo.SetProgressDelivery(ctx, id, row.Delivery.Version, domain.ProgressDelivery{State: "sending"})
	check(err)
	if ok {
		t.Fatal("two senders claimed one creation")
	}
	check(repo.ReportProgress(ctx, id, 4, "new while sending"))
	check(repo.EndProgress(ctx, id, "cancelled"))
	check(repo.EndProgress(ctx, id, "failed"))
	check(repo.ReportProgress(ctx, id, 5, "late update"))
	saved, ok, err := repo.SetProgressDelivery(ctx, id, claimed.Delivery.Version, domain.ProgressDelivery{State: "ready", MessageID: "42"})
	check(err)
	if !ok || saved.Text != "new while sending" || saved.Outcome != "cancelled" || saved.Delivery.MessageID != "42" {
		t.Fatalf("concurrent source/delivery updates lost: %+v", saved)
	}
	row = find(id)
	if row == nil || row.Outcome != "cancelled" {
		t.Fatal("stop overwritten by stale updates")
	}
	_, _, err = repo.SetProgressDelivery(ctx, id, row.Delivery.Version, domain.ProgressDelivery{State: "done", MessageID: "42"})
	check(err)
	if find(id) != nil {
		t.Fatal("terminal status is still pending")
	}

	id += "/reply"
	p.TurnID = id
	check(repo.StartProgress(ctx, p))
	other := conv
	other.ID += "/other"
	check(repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: id + "/group", TurnID: id, Conversation: other, Text: "public"}))
	check(repo.SetOutgoingState(ctx, id+"/group", "sending"))
	check(repo.SetOutgoingState(ctx, id+"/group", "sent"))
	if find(id) == nil {
		t.Fatal("cross-chat post suppressed private progress")
	}
	for _, part := range []string{"text", "file"} {
		check(repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: id + "/" + part, TurnID: id, Conversation: conv, Text: "reply"}))
	}
	if find(id) != nil {
		t.Fatal("created a status after final reply was queued")
	}
	check(repo.EndProgress(ctx, id, "waiting_delivery"))
	check(repo.SetOutgoingState(ctx, id+"/text", "sending"))
	check(repo.SetOutgoingState(ctx, id+"/text", "sent"))
	if find(id) != nil {
		t.Fatal("partial reply removed progress")
	}
	check(repo.SetOutgoingState(ctx, id+"/file", "sending"))
	check(repo.SetOutgoingState(ctx, id+"/file", "sent"))
	row = find(id)
	if row == nil || row.Outcome != "delivered" {
		t.Fatal("delivered answer not ready for cleanup")
	}
	check(repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: id + "/unknown", TurnID: id, Conversation: conv, Text: "extra part"}))
	check(repo.SetOutgoingState(ctx, id+"/unknown", "sending"))
	check(repo.SetOutgoingState(ctx, id+"/unknown", "unknown"))
	row = find(id)
	if row == nil || row.Outcome != "delivery_failed" {
		t.Fatal("unknown delivery treated as success")
	}

	// A directly acknowledged widget removes progress, but pending ordinary
	// reply parts must still be delivered first.
	id += "/widget"
	p.TurnID = id
	check(repo.StartProgress(ctx, p))
	check(repo.ConfirmProgressReply(ctx, id))
	if find(id) != nil {
		t.Fatal("active turn posted progress after delivering widget")
	}
	check(repo.EndProgress(ctx, id, "waiting_delivery"))
	if row = find(id); row == nil || row.Outcome != "delivered" {
		t.Fatal("widget-only reply left progress waiting")
	}
	check(repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: id + "/text", TurnID: id, Conversation: conv, Text: "ordinary part"}))
	if find(id) != nil {
		t.Fatal("widget delivery hid a pending ordinary reply")
	}
	check(repo.SetOutgoingState(ctx, id+"/text", "sending"))
	check(repo.SetOutgoingState(ctx, id+"/text", "sent"))
	if row = find(id); row == nil || row.Outcome != "delivered" {
		t.Fatal("mixed reply did not finish progress")
	}

	for _, topic := range []int{0, 3} {
		groupID := fmt.Sprintf("%s/group/%d", id, topic)
		group := domain.Conversation{ID: groupID, ChatID: -20, OwnerID: 7, ThreadID: topic}
		check(repo.StartProgress(ctx, domain.Progress{TurnID: groupID, Conversation: group, StartedAt: now.Add(-time.Hour)}))
		check(repo.ReportProgress(ctx, groupID, 1, "Internal group progress"))
		if find(groupID) != nil {
			t.Fatal("group progress queued for Telegram delivery")
		}
		saved, ok, err := repo.SetProgressDelivery(ctx, groupID, 0, domain.ProgressDelivery{State: "disabled"})
		check(err)
		if !ok || saved.Text != "Internal group progress" {
			t.Fatal("suppressing group delivery lost internal progress")
		}
		check(repo.EndProgress(ctx, groupID, "failed"))
		if find(groupID) != nil {
			t.Fatal("failed group progress queued a status")
		}
	}

	// Desktop suspension stays out of DueProgress until the route returns to
	// Telegram, while RetryAt defers even immediate cleanup until due.
	routeID := id + "/route"
	desktopConv := conv
	desktopConv.DeliveryChannel = "desktop"
	check(repo.StartProgress(ctx, domain.Progress{TurnID: routeID, Conversation: desktopConv, StartedAt: now.Add(-time.Minute)}))
	if row = find(routeID); row == nil || row.Conversation.DeliveryChannel != "desktop" {
		t.Fatal("ready desktop progress not returned for suspension")
	}
	saved, ok, err = repo.SetProgressDelivery(ctx, routeID, row.Delivery.Version, domain.ProgressDelivery{State: "suspended"})
	check(err)
	if !ok {
		t.Fatal("could not suspend desktop progress")
	}
	if find(routeID) != nil {
		t.Fatal("suspended desktop progress stayed due")
	}
	check(repo.StartProgress(ctx, domain.Progress{TurnID: routeID, Conversation: conv, StartedAt: now.Add(-time.Minute)}))
	if row = find(routeID); row == nil || row.Delivery.State != "suspended" {
		t.Fatal("suspended progress did not resume after returning to Telegram")
	}
	_, ok, err = repo.SetProgressDelivery(ctx, routeID, saved.Delivery.Version, domain.ProgressDelivery{
		State:    "ready",
		Removing: true,
		RetryAt:  now.Add(time.Minute),
	})
	check(err)
	if !ok {
		t.Fatal("could not defer progress retry")
	}
	if find(routeID) != nil {
		t.Fatal("future RetryAt returned progress early")
	}
}

func TestPostgresProgressRestartRecovery(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENALBERT_TEST_DATABASE_URL for PostgreSQL")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	prefix := fmt.Sprintf("restart-%d", now.UnixNano())
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"creating", "editing"} {
		p := domain.Progress{TurnID: prefix + id, Conversation: domain.Conversation{ID: prefix}, StartedAt: now.Add(-time.Minute)}
		if err = s.StartProgress(ctx, p); err != nil {
			t.Fatal(err)
		}
		delivery := domain.ProgressDelivery{State: "sending"}
		if id == "editing" {
			delivery.MessageID = "42"
		}
		if _, _, err = s.SetProgressDelivery(ctx, p.TurnID, 0, delivery); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"creating", "editing"} {
		var state, messageID string
		if err = s.pool.QueryRow(ctx, `SELECT data->'delivery'->>'state',data->'delivery'->>'message_id' FROM openalbert_progress WHERE id=$1`, prefix+id).Scan(&state, &messageID); err != nil {
			t.Fatal(err)
		}
		if id == "creating" && state != "disabled" {
			t.Fatal("ambiguous creation retried after restart")
		}
		if id == "editing" && (state != "ready" || messageID != "42") {
			t.Fatal("saved status ID lost after restart")
		}
	}
}

func TestPostgresDueProgressQueryPlan(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENALBERT_TEST_DATABASE_URL for PostgreSQL")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	now := time.Now().UTC().Truncate(time.Second)
	prefix := fmt.Sprintf("plan-%d-", time.Now().UTC().UnixNano())
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM openalbert_progress WHERE id LIKE $1`, prefix+"%"); err != nil {
			t.Errorf("cleanup openalbert_progress: %v", err)
		}
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM openalbert_outgoing WHERE id LIKE $1`, prefix+"%"); err != nil {
			t.Errorf("cleanup openalbert_outgoing: %v", err)
		}
	})

	// Seed a production-sized backlog of terminal/suspended progress rows and
	// historical outgoing rows so the planner mirrors the Raspberry Pi dataset.
	if _, err := s.pool.Exec(ctx, `
INSERT INTO openalbert_progress(id, data)
SELECT
 $1 || g,
 jsonb_build_object(
  'turn_id', $1 || g,
  'conversation', jsonb_build_object(
   'id', $1 || 'conv-' || (g % 5),
   'chat_id', CASE WHEN g % 20 = 0 THEN -100 ELSE 7 END,
   'delivery_channel', CASE WHEN g > 800 AND g <= 875 THEN 'desktop' ELSE '' END
  ),
  'started_at', ($2::timestamptz - (g || ' seconds')::interval),
  'step', 1,
  'text', 'working',
  'status', CASE WHEN g > 875 THEN 'active' ELSE 'waiting_delivery' END,
  'delivery', jsonb_build_object(
   'version', 2,
   'state', CASE
    WHEN g <= 750 THEN 'done'
    WHEN g <= 800 THEN 'disabled'
    WHEN g <= 875 THEN 'suspended'
    WHEN g = 876 THEN 'suspended'
    ELSE 'ready'
   END,
   'message_id', CASE WHEN g <= 750 THEN '42' ELSE '' END,
   'last_text', '',
   'updated_at', '0001-01-01T00:00:00Z',
   'retry_at', '0001-01-01T00:00:00Z'
  )
 )
FROM generate_series(1, 881) g`, prefix, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO openalbert_outgoing(id, data, state, attempts)
SELECT
 $1 || 'out-' || g,
 jsonb_build_object(
  'id', $1 || 'out-' || g,
  'turn_id', $1 || ((g % 750) + 1),
  'conversation', jsonb_build_object('id', $1 || 'conv-' || (((g % 750) + 1) % 5), 'chat_id', 7),
  'text', 'sent reply',
  'enqueued_at', ($2::timestamptz - (g || ' seconds')::interval),
  'state', 'sent'
 ),
 'sent',
 1
FROM generate_series(1, 1233) g`, prefix, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `VACUUM ANALYZE openalbert_progress`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `VACUUM ANALYZE openalbert_outgoing`); err != nil {
		t.Fatal(err)
	}

	assertFastPlan := func(label string) {
		t.Helper()
		var planJSON string
		if err := s.pool.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+dueProgressSQL,
			now, now.Add(-domain.ProgressDelay), now.Add(-domain.ProgressInterval), 50).Scan(&planJSON); err != nil {
			t.Fatalf("%s: EXPLAIN failed: %v", label, err)
		}
		type planNode struct {
			NodeType       string     `json:"Node Type"`
			Relation       string     `json:"Relation Name"`
			IndexName      string     `json:"Index Name"`
			ParentRelation string     `json:"Parent Relationship"`
			SubplanName    string     `json:"Subplan Name"`
			Plans          []planNode `json:"Plans"`
		}
		var root []struct {
			Plan planNode `json:"Plan"`
			JIT  *struct {
				Functions int `json:"Functions"`
			} `json:"JIT"`
		}
		if err := json.Unmarshal([]byte(planJSON), &root); err != nil || len(root) == 0 {
			t.Fatalf("%s: parse EXPLAIN JSON: %v\n%s", label, err, planJSON)
		}
		if root[0].JIT != nil && root[0].JIT.Functions > 0 {
			t.Fatalf("%s: DueProgress triggered JIT (%d functions):\n%s", label, root[0].JIT.Functions, planJSON)
		}
		sawDueIndex := false
		var walk func(n planNode)
		walk = func(n planNode) {
			if n.NodeType == "Seq Scan" && (n.Relation == "openalbert_progress" || n.Relation == "openalbert_outgoing") {
				t.Fatalf("%s: unexpected Seq Scan on %s:\n%s", label, n.Relation, planJSON)
			}
			if n.ParentRelation == "SubPlan" || n.SubplanName != "" {
				t.Fatalf("%s: unexpected correlated SubPlan %q:\n%s", label, n.SubplanName, planJSON)
			}
			if n.IndexName == "openalbert_progress_due" {
				sawDueIndex = true
			}
			for _, child := range n.Plans {
				walk(child)
			}
		}
		walk(root[0].Plan)
		if !sawDueIndex {
			t.Fatalf("%s: DueProgress did not use openalbert_progress_due:\n%s", label, planJSON)
		}
	}

	rows, err := s.DueProgress(ctx, now, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, row := range rows {
		if len(row.TurnID) >= len(prefix) && row.TurnID[:len(prefix)] == prefix {
			found++
		}
	}
	// Rows 876 (suspended non-desktop) and 877..881 (ready active, started > 30s ago) = 6 due rows.
	if found != 6 {
		t.Fatalf("expected 6 due rows for prefix, got %d", found)
	}
	assertFastPlan("with-candidates")

	// Idle state (all prefix rows done) must also stay on the partial index without JIT.
	if _, err := s.pool.Exec(ctx, `UPDATE openalbert_progress SET data=jsonb_set(data,'{delivery,state}','"done"'::jsonb) WHERE id LIKE $1`, prefix+"%"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `VACUUM ANALYZE openalbert_progress`); err != nil {
		t.Fatal(err)
	}
	assertFastPlan("idle")
}
