package backup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Compose 2 supports start without --wait. Inspect readiness explicitly instead
// of requiring a newer plugin than the one shipped in the controller image.
func (r *Runner) resumeServices(ctx context.Context, services []string) error {
	if _, err := r.compose(ctx, append([]string{"start"}, services...)...); err != nil {
		return err
	}
	raw, err := r.compose(ctx, append([]string{"ps", "--all", "--quiet"}, services...)...)
	if err != nil {
		return err
	}
	ids := strings.Fields(string(raw))
	if len(ids) < len(services) {
		return errors.New("not all stopped service containers were found")
	}
	command := r.Command
	if command == nil {
		command = RunCommand
	}
	readyCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		ready := true
		for _, id := range ids {
			raw, err = command(readyCtx, "docker", "inspect", id, "--format", "{{json .State}}")
			if err != nil {
				return err
			}
			var state struct {
				Running bool
				Health  *struct{ Status string }
			}
			if json.Unmarshal(raw, &state) != nil {
				return errors.New("cannot inspect restarted service readiness")
			}
			if !state.Running || (state.Health != nil && state.Health.Status != "healthy") {
				ready = false
			}
		}
		if ready {
			return nil
		}
		select {
		case <-readyCtx.Done():
			return errors.New("restarted services did not become ready")
		case <-tick.C:
		}
	}
}
