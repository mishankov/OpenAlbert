package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type request struct {
	ID string `json:"id"`
}

// Request queues a manual attempt for the scheduler process, so Docker's stop
// signal reaches the same process that owns the active backup and its cleanup.
func (r *Runner) Request() error {
	lock, err := r.lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	if !r.Config.Enabled {
		return errors.New("backups are disabled")
	}
	return AtomicJSON(filepath.Join(r.Config.StateDir, "request.json"), request{ID: rand.Text()})
}

// initialize preserves the previous deadline so a container restart can catch up
// one missed run. A fresh installation waits for its first scheduled deadline.
func (r *Runner) initialize(ctx context.Context, now time.Time) (time.Time, error) {
	if err := r.Recover(ctx); err != nil {
		return time.Time{}, err
	}
	lock, err := r.lock()
	if err != nil {
		return time.Time{}, err
	}
	defer lock.Close()
	old, _ := (Reader{Dir: r.Config.StateDir}).Read()
	r.loadState()
	next := r.Config.NextRun(now)
	if old.Enabled && old.Time == r.Config.Time && old.Timezone == r.Config.Timezone && !old.NextRunAt.IsZero() && !old.NextRunAt.After(now) {
		next = old.NextRunAt
	}
	r.state.NextRunAt = next
	phase, code := old.Phase, old.ErrorCode
	if phase == "" {
		phase = "idle"
	}
	interrupted := phase != "idle" && phase != "completed" && phase != "failed"
	if interrupted {
		phase, code = "failed", "interrupted_backup"
	}
	if err = r.save(phase, code); err != nil {
		return time.Time{}, err
	}
	if interrupted {
		if err = r.emit("failed", code); err != nil {
			return time.Time{}, err
		}
	}
	return next, nil
}

// Serve owns the schedule inside the independent backup container. Create runs
// serially, keeps its own heartbeat, and never stops the controller service.
func (r *Runner) Serve(ctx context.Context, log io.Writer) error {
	next, err := r.initialize(ctx, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintln(log, "Backup scheduler started; next run:", next.Format(time.RFC3339))
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	monitor := time.NewTicker(5 * time.Minute)
	defer monitor.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-monitor.C:
			if err = r.Monitor(time.Now()); err != nil && !errors.Is(err, ErrBusy) {
				fmt.Fprintln(log, "Backup monitor:", err)
			}
		case now := <-tick.C:
			path := filepath.Join(r.Config.StateDir, "request.json")
			var pending request
			requested := readJSON(path, &pending) == nil && pending.ID != ""
			if !r.Config.Enabled || (now.Before(next) && !requested) {
				continue
			}
			next = r.Config.NextRun(now)
			attempt, stop := context.WithTimeout(ctx, 6*time.Hour)
			err = r.Create(attempt)
			stop()
			if requested && !errors.Is(err, ErrBusy) {
				var current request
				if readJSON(path, &current) == nil && current.ID == pending.ID {
					_ = os.Remove(path)
				}
			}
			if err != nil && !errors.Is(err, ErrBusy) {
				fmt.Fprintln(log, "Backup attempt:", err)
			} else if err == nil {
				fmt.Fprintln(log, "Backup completed")
			}
			// Persist the next deadline even after an attempt crosses a day boundary.
			if s, e := (Reader{Dir: r.Config.StateDir}).Read(); e == nil && s.NextRunAt.After(now) {
				next = s.NextRunAt
			}
		}
	}
}
