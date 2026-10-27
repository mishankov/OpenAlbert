// Package scheduling calculates durable task slots independently of run duration.
package scheduling

import (
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
	"openalbert/apps/backend/internal/domain"
)

const MaxIntervalSeconds = int64((1<<63 - 1) / int64(time.Second))

func calendar(t domain.Task) (cron.Schedule, error) {
	if t.ScheduleTimezone == "" || t.ScheduleTimezone == "Local" {
		return nil, errors.New("schedule_timezone must be an explicit IANA timezone")
	}
	loc, err := time.LoadLocation(t.ScheduleTimezone)
	if err != nil {
		return nil, fmt.Errorf("schedule_timezone: %w", err)
	}
	// Timezone belongs to its own field; descriptors such as @every would reintroduce drift.
	if len(strings.Fields(t.ScheduleCron)) != 5 {
		return nil, errors.New("schedule_cron requires five fields")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(t.ScheduleCron)
	if err != nil {
		return nil, fmt.Errorf("schedule_cron: %w", err)
	}
	schedule.(*cron.SpecSchedule).Location = loc
	return schedule, nil
}

func Validate(t domain.Task) error {
	if t.ScheduleSeconds < 0 || int64(t.ScheduleSeconds) > MaxIntervalSeconds {
		return errors.New("invalid task interval")
	}
	if t.ScheduleCron == "" {
		if t.ScheduleTimezone != "" {
			return errors.New("schedule_timezone requires schedule_cron")
		}
		return nil
	}
	if t.ScheduleSeconds != 0 {
		return errors.New("use either schedule_cron or schedule_seconds")
	}
	schedule, err := calendar(t)
	if err != nil {
		return err
	}
	// Gregorian calendar schedules must have a slot in a complete leap-year cycle.
	if schedule.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)).IsZero() {
		return errors.New("schedule_cron has no calendar slots")
	}
	return nil
}

func Repeats(t domain.Task) bool { return t.ScheduleSeconds > 0 || t.ScheduleCron != "" }

// Next returns the first slot strictly after now. A future persisted slot wins,
// including an explicit reschedule during the run. Missed slots never form a queue.
// Intervals measure elapsed time; cron measures local calendar time. Cron skips
// nonexistent wall times and runs at both instants of an ambiguous wall time.
func Next(t domain.Task, now time.Time) (time.Time, error) {
	if err := Validate(t); err != nil {
		return time.Time{}, err
	}
	if t.NextRunAt.After(now) {
		return t.NextRunAt.UTC(), nil
	}
	if t.ScheduleCron != "" {
		s, err := calendar(t)
		if err != nil {
			return time.Time{}, err
		}
		next := s.Next(now.UTC())
		if next.IsZero() {
			return next, errors.New("schedule_cron has no slot in the next five years")
		}
		return next.UTC(), nil
	}
	if t.ScheduleSeconds == 0 {
		return time.Time{}, errors.New("task has no repeating schedule")
	}
	anchor := t.NextRunAt
	if anchor.IsZero() {
		anchor = now
	}
	interval := time.Duration(t.ScheduleSeconds) * time.Second
	// Reduce seconds before converting to Duration: time.Sub saturates for
	// old RFC3339 anchors more than roughly 292 years in the past.
	elapsedSeconds := now.Unix() - anchor.Unix()
	remainder := time.Duration(elapsedSeconds%int64(t.ScheduleSeconds))*time.Second + time.Duration(now.Nanosecond()-anchor.Nanosecond())
	if remainder < 0 {
		remainder += interval
	}
	return now.Add(interval - remainder).UTC(), nil
}
