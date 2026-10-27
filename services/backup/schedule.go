package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Units writes reviewable systemd units; enabling them is an explicit host step.
func (c Config) Units(binary, configPath, envPath, output string) error {
	for _, path := range []string{binary, configPath, envPath, output} {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\n\r\x00") {
			return errors.New("unit paths must be absolute and single-line")
		}
	}
	quote := func(s string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s) + `"`
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	service := func(command string) string {
		return fmt.Sprintf("[Unit]\nDescription=OpenAlbert backup %s\nWants=network-online.target\nAfter=network-online.target docker.service\n\n[Service]\nType=oneshot\nUser=root\nUMask=0077\nEnvironmentFile=%s\nExecStart=%s %s --config %s\nTimeoutStartSec=infinity\nTimeoutStopSec=300\nKillMode=mixed\n", command, quote(envPath), quote(binary), command, quote(configPath))
	}
	timer := fmt.Sprintf("[Unit]\nDescription=Daily encrypted OpenAlbert backup\n\n[Timer]\nOnCalendar=*-*-* %s:00 %s\nPersistent=true\nAccuracySec=1s\nUnit=openalbert-backup.service\n\n[Install]\nWantedBy=timers.target\n", c.Time, c.Timezone)
	monitor := "[Unit]\nDescription=Check OpenAlbert backup freshness\n\n[Timer]\nOnBootSec=5min\nOnUnitActiveSec=5min\nUnit=openalbert-backup-monitor.service\n\n[Install]\nWantedBy=timers.target\n"
	for name, content := range map[string]string{"openalbert-backup.service": service("create"), "openalbert-backup.timer": timer, "openalbert-backup-monitor.service": service("monitor"), "openalbert-backup-monitor.timer": monitor} {
		if err := os.WriteFile(filepath.Join(output, name), []byte(content), 0644); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) Monitor(now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := r.Recover(ctx); err != nil {
		if errors.Is(err, ErrBusy) {
			return nil
		}
		return err
	}
	s := (Reader{Dir: r.Config.StateDir}).View(now)
	if !r.Config.Enabled {
		return nil
	}
	code := ""
	if s.Phase == "unknown" {
		code = s.ErrorCode
	}
	if s.LastSuccess != nil && now.Sub(s.LastSuccess.CreatedAt) > 26*time.Hour {
		code = "no_recent_backup"
	}
	if s.LastSuccess == nil && now.Sub(s.UpdatedAt) > 26*time.Hour {
		code = "no_successful_backup"
	}
	if code == "" {
		return nil
	}
	// Stable ID makes repeated timer ticks and process restarts idempotent.
	loc, _ := time.LoadLocation(r.Config.Timezone)
	id := "health-" + now.In(loc).Format("20060102") + "-" + code
	path := filepath.Join(r.Config.StateDir, "events", id+".json")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := publicDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return AtomicJSON(path, Event{ID: id, Kind: "overdue", At: now.UTC(), ErrorCode: code})
}

// The journal survives SIGKILL or power loss. The monitor retries service startup
// after the backup lock is released, and never starts previously stopped services.
func (r *Runner) Recover(ctx context.Context) error {
	lock, err := r.lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	path := filepath.Join(r.Config.StateDir, "resume.json")
	var services []string
	if err = readJSON(path, &services); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, name := range services {
		if name == "" || name == ControllerService || strings.ContainsAny(name, "/ -\r\n") {
			return errors.New("invalid resume journal")
		}
	}
	if len(services) == 0 {
		return errors.New("empty resume journal")
	}
	if err = r.resumeServices(ctx, services); err != nil {
		return err
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	r.loadState()
	if err = r.save("failed", "interrupted_backup"); err != nil {
		return err
	}
	return r.emit("failed", "interrupted_backup")
}
