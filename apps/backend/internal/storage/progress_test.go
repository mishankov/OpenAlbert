package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestProgressRepositoryContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { progressContract(t, NewMemory()) })
	t.Run("sqlite", func(t *testing.T) { progressContract(t, openTestSQLite(t)) })
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

func TestSQLiteProgressRestartRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	prefix := fmt.Sprintf("restart-%d", now.UnixNano())
	dbPath := filepath.Join(t.TempDir(), "progress.db")
	s, err := Open(ctx, dbPath)
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
	s, err = Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"creating", "editing"} {
		var state, messageID string
		if err = s.queryRow(ctx, `SELECT json_extract(data, '$.delivery.state'), COALESCE(json_extract(data, '$.delivery.message_id'), '') FROM openalbert_progress WHERE id = ?1`, prefix+id).Scan(&state, &messageID); err != nil {
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

func TestSQLiteDueProgressQueryPlan(t *testing.T) {
	ctx := context.Background()
	s := openTestSQLite(t)

	now := time.Now().UTC().Truncate(time.Second)
	prefix := fmt.Sprintf("plan-%d-", time.Now().UTC().UnixNano())

	// Seed a production-sized backlog of terminal/suspended progress rows and historical outgoing rows.
	tx, err := s.beginImmediate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	zeroTS := formatTime(time.Time{})
	for g := 1; g <= 881; g++ {
		turnID := fmt.Sprintf("%s%d", prefix, g)
		convID := fmt.Sprintf("%sconv-%d", prefix, g%5)
		chatID := int64(7)
		if g%20 == 0 {
			chatID = -100
		}
		channel := ""
		if g > 800 && g <= 875 {
			channel = "desktop"
		}
		status := "waiting_delivery"
		if g > 875 {
			status = "active"
		}
		dState := "ready"
		msgID := ""
		switch {
		case g <= 750:
			dState, msgID = "done", "42"
		case g <= 800:
			dState = "disabled"
		case g <= 876:
			dState = "suspended"
		}
		startedAt := now.Add(-time.Duration(g) * time.Second)
		p := domain.Progress{
			TurnID:       turnID,
			Conversation: domain.Conversation{ID: convID, ChatID: chatID, DeliveryChannel: channel},
			StartedAt:    startedAt,
			Step:         1,
			Text:         "working",
			Status:       status,
			Delivery:     domain.ProgressDelivery{Version: 2, State: dState, MessageID: msgID},
		}
		raw, err := encode(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.exec(ctx, `INSERT INTO openalbert_progress(
 id, conversation_id, delivery_channel, started_at, status, direct_reply,
 delivery_state, delivery_version, delivery_removing, delivery_updated_at, delivery_retry_at, data
) VALUES(?1, ?2, ?3, ?4, ?5, 0, ?6, 2, 0, ?7, ?7, ?8)`,
			turnID, convID, channel, formatTime(startedAt), status, dState, zeroTS, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for g := 1; g <= 1233; g++ {
		outID := fmt.Sprintf("%sout-%d", prefix, g)
		turnNum := (g % 750) + 1
		turnID := fmt.Sprintf("%s%d", prefix, turnNum)
		convID := fmt.Sprintf("%sconv-%d", prefix, turnNum%5)
		out := domain.Outgoing{
			ID:           outID,
			TurnID:       turnID,
			Conversation: domain.Conversation{ID: convID, ChatID: 7},
			Text:         "sent reply",
			EnqueuedAt:   now.Add(-time.Duration(g) * time.Second),
			State:        "sent",
			Attempts:     1,
		}
		raw, err := encode(out)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.exec(ctx, `INSERT INTO openalbert_outgoing(id, turn_id, conversation_id, session_id, data, state, attempts)
 VALUES(?1, ?2, ?3, '', ?4, 'sent', 1)`, outID, turnID, convID, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}

	assertFastPlan := func(label string) {
		t.Helper()
		rows, err := s.query(ctx, `EXPLAIN QUERY PLAN `+dueProgressSQL,
			formatTime(now), formatTime(now.Add(-domain.ProgressDelay)), formatTime(now.Add(-domain.ProgressInterval)), 50)
		if err != nil {
			t.Fatalf("%s: EXPLAIN QUERY PLAN failed: %v", label, err)
		}
		defer rows.Close()
		var lines []string
		sawDueIndex := false
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, detail)
			if strings.Contains(detail, "openalbert_progress_due") {
				sawDueIndex = true
			}
			if (strings.HasPrefix(detail, "SCAN p") || strings.HasPrefix(detail, "SCAN openalbert_progress") || strings.HasPrefix(detail, "SCAN o") || strings.HasPrefix(detail, "SCAN openalbert_outgoing")) && !strings.Contains(detail, "USING INDEX") && !strings.Contains(detail, "USING COVERING INDEX") {
				t.Fatalf("%s: unexpected full table scan: %s", label, detail)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !sawDueIndex {
			t.Fatalf("%s: DueProgress did not use openalbert_progress_due:\n%s", label, strings.Join(lines, "\n"))
		}
	}

	rows, err := s.DueProgress(ctx, now, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, row := range rows {
		if strings.HasPrefix(row.TurnID, prefix) {
			found++
		}
	}
	// Rows 876 (suspended non-desktop) and 877..881 (ready active, started > 30s ago) = 6 due rows.
	if found != 6 {
		t.Fatalf("expected 6 due rows for prefix, got %d", found)
	}
	assertFastPlan("with-candidates")

	if _, err := s.exec(ctx, `UPDATE openalbert_progress SET delivery_state = 'done', data = json_set(data, '$.delivery.state', 'done') WHERE id LIKE ?1`, prefix+"%"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	assertFastPlan("idle")
}
