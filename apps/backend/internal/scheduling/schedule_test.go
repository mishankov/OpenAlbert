package scheduling

import (
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func instant(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNextSlots(t *testing.T) {
	for _, tc := range []struct {
		name, anchor, now, want, spec, zone string
		seconds                             int
	}{
		{name: "ancient anchor", anchor: "1600-01-01T17:00:00.5Z", now: "2026-10-25T17:02:23Z", want: "2026-10-26T17:00:00.5Z", seconds: 86400},
		{name: "fraction borrowed", anchor: "2026-10-25T17:00:00.5Z", now: "2026-10-26T17:00:00.1Z", want: "2026-10-26T17:00:00.5Z", seconds: 86400},
		{name: "daily anchor", anchor: "2026-10-25T17:00:00Z", now: "2026-10-25T17:02:23.751376473Z", want: "2026-10-26T17:00:00Z", seconds: 86400},
		{name: "midnight", anchor: "2026-12-31T23:59:00Z", now: "2027-01-01T00:01:23Z", want: "2027-01-01T23:59:00Z", seconds: 86400},
		{name: "three days missed", anchor: "2026-10-22T17:00:00Z", now: "2026-10-25T17:02:23Z", want: "2026-10-26T17:00:00Z", seconds: 86400},
		{name: "exact slot consumed", anchor: "2026-10-25T17:00:00Z", now: "2026-10-26T17:00:00Z", want: "2026-10-27T17:00:00Z", seconds: 86400},
		{name: "future legacy slot unchanged", anchor: "2026-10-26T17:02:23.751376473Z", now: "2026-10-25T17:00:00Z", want: "2026-10-26T17:02:23.751376473Z", seconds: 86400},
		{name: "legacy phase preserved", anchor: "2026-10-25T17:02:23.751376473Z", now: "2026-10-25T17:05:00Z", want: "2026-10-26T17:02:23.751376473Z", seconds: 86400},
		{name: "explicit future reschedule", anchor: "2026-10-27T17:00:00Z", now: "2026-10-25T17:02:23Z", want: "2026-10-27T17:00:00Z", seconds: 86400},
		{name: "UTC calendar", anchor: "2026-10-25T17:00:00Z", now: "2026-10-25T17:02:23Z", want: "2026-10-26T17:00:00Z", spec: "0 17 * * *", zone: "UTC"},
		{name: "Moscow calendar", now: "2026-10-25T17:02:23Z", want: "2026-10-26T17:00:00Z", spec: "0 20 * * *", zone: "Europe/Moscow"},
		{name: "cron missed days", anchor: "2026-10-22T17:00:00Z", now: "2026-10-25T17:02:23Z", want: "2026-10-26T17:00:00Z", spec: "0 17 * * *", zone: "UTC"},
		{name: "spring calendar", now: "2026-03-28T16:02:23Z", want: "2026-03-29T15:00:00Z", spec: "0 17 * * *", zone: "Europe/Berlin"},
		{name: "autumn calendar", now: "2026-10-24T15:02:23Z", want: "2026-10-25T16:00:00Z", spec: "0 17 * * *", zone: "Europe/Berlin"},
		{name: "spring gap skipped", now: "2026-03-28T01:31:00Z", want: "2026-03-30T00:30:00Z", spec: "30 2 * * *", zone: "Europe/Berlin"},
		{name: "fold first occurrence", now: "2026-10-24T00:31:00Z", want: "2026-10-25T00:30:00Z", spec: "30 2 * * *", zone: "Europe/Berlin"},
		{name: "fold second occurrence", now: "2026-10-25T00:31:00Z", want: "2026-10-25T01:30:00Z", spec: "30 2 * * *", zone: "Europe/Berlin"},
		{name: "midnight DST gap", now: "2018-11-03T03:01:00Z", want: "2018-11-05T02:00:00Z", spec: "0 0 * * *", zone: "America/Sao_Paulo"},
		{name: "elapsed interval across spring", anchor: "2026-03-28T17:00:00+01:00", now: "2026-03-28T17:02:23+01:00", want: "2026-03-29T16:00:00Z", seconds: 86400},
		{name: "elapsed interval across autumn", anchor: "2026-10-24T17:00:00+02:00", now: "2026-10-24T17:02:23+02:00", want: "2026-10-25T15:00:00Z", seconds: 86400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := domain.Task{ScheduleSeconds: tc.seconds, ScheduleCron: tc.spec, ScheduleTimezone: tc.zone}
			if tc.anchor != "" {
				task.NextRunAt = instant(tc.anchor)
			}
			got, err := Next(task, instant(tc.now))
			if err != nil || !got.Equal(instant(tc.want)) {
				t.Fatalf("got %v, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestTenRunsDoNotDrift(t *testing.T) {
	for _, calendar := range []bool{false, true} {
		task := domain.Task{NextRunAt: instant("2026-10-25T17:00:00Z"), ScheduleSeconds: 86400}
		if calendar {
			task.ScheduleSeconds = 0
			task.ScheduleCron = "0 17 * * *"
			task.ScheduleTimezone = "UTC"
		}
		for n := 0; n < 10; n++ {
			want := instant("2026-10-25T17:00:00Z").AddDate(0, 0, n+1)
			next, err := Next(task, task.NextRunAt.Add(143*time.Second+751376473*time.Nanosecond))
			if err != nil || !next.Equal(want) {
				t.Fatalf("calendar=%v run=%d got=%v err=%v", calendar, n, next, err)
			}
			task.NextRunAt = next
		}
	}
}

func TestInvalidSchedules(t *testing.T) {
	for _, task := range []domain.Task{
		{ScheduleSeconds: -1}, {ScheduleSeconds: int(MaxIntervalSeconds) + 1},
		{ScheduleCron: "0 17 * * *"}, {ScheduleTimezone: "UTC"},
		{ScheduleCron: "0 17 * * *", ScheduleTimezone: "Local"},
		{ScheduleCron: "0 17 * * *", ScheduleTimezone: "bad/zone"},
		{ScheduleCron: "0 17 * * *", ScheduleTimezone: "UTC", ScheduleSeconds: 86400},
		{ScheduleCron: "@every 24h", ScheduleTimezone: "UTC"},
		{ScheduleCron: "CRON_TZ=UTC 0 17 * * *", ScheduleTimezone: "UTC"},
		{ScheduleCron: "0 25 * * *", ScheduleTimezone: "UTC"},
	} {
		if err := Validate(task); err == nil {
			t.Fatalf("accepted invalid schedule %+v", task)
		}
	}
	if _, err := Next(domain.Task{ScheduleCron: "0 17 31 2 *", ScheduleTimezone: "UTC"}, instant("2026-01-01T00:00:00Z")); err == nil {
		t.Fatal("accepted impossible calendar")
	}
}
