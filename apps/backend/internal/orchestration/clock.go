package orchestration

import (
	"context"
	"time"
)

// Clock abstracts wall-clock time and timers so scheduling, idle timeouts, and
// retries can run deterministically under virtual time in tests.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is the minimal time.Timer interface used by orchestration loops.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// RealClock delegates to the standard library time package.
type RealClock struct{}

func (RealClock) Now() time.Time                 { return time.Now() }
func (RealClock) NewTimer(d time.Duration) Timer { return &realTimer{t: time.NewTimer(d)} }

type realTimer struct {
	t *time.Timer
}

func (r *realTimer) C() <-chan time.Time        { return r.t.C }
func (r *realTimer) Stop() bool                 { return r.t.Stop() }
func (r *realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

func sleepCtx(ctx context.Context, clk Clock, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	if clk == nil {
		clk = RealClock{}
	}
	timer := clk.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C():
		return true
	}
}
