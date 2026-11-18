package browser

import (
	"context"
	"testing"
)

func TestDeferredTaskCleanupDoesNotCloseResumedRun(t *testing.T) {
	var drivers []*fakeDriver
	m := testManager(t, func(context.Context) (driver, error) {
		d := &fakeDriver{}
		drivers = append(drivers, d)
		return d, nil
	})
	old := owner("old-run", true)
	old.TaskID = "nba"
	if _, err := perform(m, old); err != nil {
		t.Fatal(err)
	}
	cleanup := m.PrepareCloseTask("nba")
	resumed := owner("resumed-run", true)
	resumed.TaskID = "nba"
	if _, err := perform(m, resumed); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if !drivers[0].closed.Load() || drivers[1].closed.Load() {
		t.Fatal("deferred cleanup selected resumed browser")
	}
}
