package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSchedulerRestartCatchesUpOneMissedDeadline(t *testing.T) {
	c, _ := fixture(t)
	now := time.Now().UTC()
	deadline := now.Add(-time.Hour)
	old := State{
		Version:     1,
		Enabled:     true,
		Time:        c.Time,
		Timezone:    c.Timezone,
		Phase:       "completed",
		UpdatedAt:   now.Add(-25 * time.Hour),
		NextRunAt:   deadline,
		LastSuccess: &Copy{ID: "previous", CreatedAt: now.Add(-25 * time.Hour)},
	}
	if err := AtomicJSON(filepath.Join(c.StateDir, "status.json"), old); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Config: c}
	next, err := r.initialize(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(deadline) {
		t.Fatalf("missed deadline discarded: %s", next)
	}
	s, err := (Reader{Dir: c.StateDir}).Read()
	if err != nil || s.Phase != "completed" || s.LastSuccess == nil || s.LastSuccess.ID != "previous" {
		t.Fatalf("restart erased backup result %+v %v", s, err)
	}
	// A deliberate schedule change starts with its new future deadline.
	r.Config.Time = "04:15"
	next, err = r.initialize(context.Background(), now)
	if err != nil || !next.After(now) {
		t.Fatalf("old schedule replayed after reconfiguration: %s %v", next, err)
	}
}

func TestSchedulerRestartRecordsInterruptedUpload(t *testing.T) {
	c, _ := fixture(t)
	now := time.Now().UTC()
	old := State{
		Version:     1,
		Enabled:     true,
		Time:        c.Time,
		Timezone:    c.Timezone,
		RunID:       "interrupted",
		Phase:       "uploading",
		UpdatedAt:   now,
		HeartbeatAt: now,
	}
	if err := AtomicJSON(filepath.Join(c.StateDir, "status.json"), old); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Config: c}
	if _, err := r.initialize(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	s, err := (Reader{Dir: c.StateDir}).Read()
	if err != nil || s.Phase != "failed" || s.ErrorCode != "interrupted_backup" {
		t.Fatalf("interrupted upload hidden: %+v %v", s, err)
	}
	events, err := (Reader{Dir: c.StateDir}).Events()
	if err != nil || len(events) != 1 || events[0].Kind != "failed" {
		t.Fatalf("missing interruption event: %+v %v", events, err)
	}
}

func TestManualRequestCompletesAndCleansWorkDir(t *testing.T) {
	c, key := fixture(t)
	remote := &memoryRemote{}
	r := &Runner{Config: c, Remote: remote}
	if err := r.Request(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, io.Discard) }()

	reader := Reader{Dir: c.StateDir}
	var final State
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		if s, err := reader.Read(); err == nil && s.Phase == "completed" {
			final = s
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if final.Phase != "completed" || final.LastSuccess == nil {
		t.Fatalf("manual request did not complete: %+v", final)
	}
	if _, err := os.Stat(filepath.Join(c.StateDir, "request.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request.json not removed after completion: %v", err)
	}
	assertWorkDirEmpty(t, c.WorkDir)

	archivePath := filepath.Join(t.TempDir(), "manual.age")
	if err := os.WriteFile(archivePath, remote.archiveBytes(final.LastSuccess.ID), 0600); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(context.Background(), archivePath, identity, filepath.Join(t.TempDir(), "restored")); err != nil {
		t.Fatalf("manual backup restore failed: %v", err)
	}
}

func TestManualRequestFailureMarksFailedAndCleansWorkDir(t *testing.T) {
	c, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	remote := &memoryRemote{
		uploadErr: errors.New("simulated remote failure"),
		onUpload:  cancel,
	}
	r := &Runner{Config: c, Remote: remote}
	if err := r.Request(); err != nil {
		t.Fatal(err)
	}
	if err := r.Serve(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, err := (Reader{Dir: c.StateDir}).Read()
	if err != nil || s.Phase != "failed" {
		t.Fatalf("failed request reported as successful: %+v %v", s, err)
	}
	assertWorkDirEmpty(t, c.WorkDir)
}
