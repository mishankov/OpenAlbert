package backup

import (
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
		return fmt.Sprintf("[Unit]\nDescription=OpenAlbert backup %s\nWants=network-online.target\nAfter=network-online.target\n\n[Service]\nType=oneshot\nUser=root\nUMask=0077\nEnvironmentFile=%s\nExecStart=%s %s --config %s\nTimeoutStartSec=infinity\nTimeoutStopSec=300\nKillMode=mixed\n", command, quote(envPath), quote(binary), command, quote(configPath))
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
	lock, err := r.lock()
	if errors.Is(err, ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()

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
