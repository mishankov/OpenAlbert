package backup

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestSchedulerRestartCatchesUpOneMissedDeadline(t *testing.T) {
	c, _ := fixture(t)
	now := time.Now().UTC()
	deadline := now.Add(-time.Hour)
	old := State{Version: 1, Enabled: true, Time: c.Time, Timezone: c.Timezone, Phase: "completed", UpdatedAt: now.Add(-25 * time.Hour), NextRunAt: deadline, LastSuccess: &Copy{ID: "previous", CreatedAt: now.Add(-25 * time.Hour)}}
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
	old := State{Version: 1, Enabled: true, Time: c.Time, Timezone: c.Timezone, RunID: "interrupted", Phase: "uploading", UpdatedAt: now, HeartbeatAt: now}
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

func TestContainerCannotBackUpAnotherProjectsData(t *testing.T) {
	c, _ := fixture(t)
	var steps []string
	r := &Runner{Config: c, HostRoot: "/another-installation", Remote: &memoryRemote{}, Command: fakeCommands(t, c, &steps, false, nil)}
	if err := r.Create(context.Background()); err == nil {
		t.Fatal("accepted other installation's data")
	}
	if len(steps) != 0 {
		t.Fatal("caused downtime despite data mount mismatch")
	}
}

func TestManualRequestUsesSchedulerCleanupOnShutdown(t *testing.T) {
	c, _ := fixture(t)
	var steps []string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := &Runner{Config: c, Remote: &memoryRemote{}}
	r.Command = fakeCommands(t, c, &steps, false, cancel)
	if err := r.Request(); err != nil {
		t.Fatal(err)
	}
	if err := r.Serve(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0] != "stop" || steps[1] != "start" {
		t.Fatalf("scheduler shutdown did not resume services: %v", steps)
	}
	s, err := (Reader{Dir: c.StateDir}).Read()
	if err != nil || s.Phase != "failed" {
		t.Fatalf("cancelled request reported as successful: %+v %v", s, err)
	}
}

func TestResumeWaitsForHealthInsteadOfOnlyContainerRunning(t *testing.T) {
	c, _ := fixture(t)
	var steps []string
	base := fakeCommands(t, c, &steps, false, nil)
	probes := 0
	r := &Runner{Config: c, Command: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "inspect" && args[len(args)-1] == "{{json .State}}" {
			probes++
			status := "starting"
			if probes > 1 {
				status = "healthy"
			}
			return json.Marshal(map[string]any{"Running": true, "Health": map[string]string{"Status": status}})
		}
		return base(ctx, name, args...)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.resumeServices(ctx, []string{"postgres"}); err != nil {
		t.Fatal(err)
	}
	if probes < 2 {
		t.Fatal("accepted a running but not yet healthy service")
	}
}
