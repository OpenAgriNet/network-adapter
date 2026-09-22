package pipeline

// cron_test.go pins the cron expression parser and, more importantly,
// prevFiring -- the "when was this last supposed to run" question the whole
// schedule rests on. An off-by-one-day answer here does not crash anything; it
// publishes yesterday's prices forever while every log line looks healthy.

import (
	"testing"
	"time"
)

func TestParseCronAcceptsStandardExpressions(t *testing.T) {
	// minute hour day-of-month month day-of-week
	tests := map[string]struct {
		expr    string
		match   []time.Time // instants the expression must fire at
		noMatch []time.Time
	}{
		"every day at midnight": {
			expr:    "0 0 * * *",
			match:   []time.Time{time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)},
			noMatch: []time.Time{time.Date(2026, 9, 21, 0, 1, 0, 0, time.UTC)},
		},
		"a specific minute past a specific hour": {
			expr:    "30 6 * * *",
			match:   []time.Time{time.Date(2026, 9, 21, 6, 30, 0, 0, time.UTC)},
			noMatch: []time.Time{time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC)},
		},
		"a step": {
			expr: "*/15 * * * *",
			match: []time.Time{
				time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC),
				time.Date(2026, 9, 21, 3, 45, 0, 0, time.UTC),
			},
			noMatch: []time.Time{time.Date(2026, 9, 21, 3, 20, 0, 0, time.UTC)},
		},
		"a list": {
			expr: "0 0,12 * * *",
			match: []time.Time{
				time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
			},
			noMatch: []time.Time{time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC)},
		},
		"a range": {
			expr: "0 9-17 * * *",
			match: []time.Time{
				time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC),
				time.Date(2026, 9, 21, 17, 0, 0, 0, time.UTC),
			},
			noMatch: []time.Time{time.Date(2026, 9, 21, 18, 0, 0, 0, time.UTC)},
		},
		"a stepped range": {
			expr: "0 0-12/6 * * *",
			match: []time.Time{
				time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC),
			},
			noMatch: []time.Time{time.Date(2026, 9, 21, 18, 0, 0, 0, time.UTC)},
		},
		"day of month": {
			// 2026-09-01 is a Tuesday.
			expr:    "0 0 1 * *",
			match:   []time.Time{time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
			noMatch: []time.Time{time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
		},
		"named month": {
			expr:    "0 0 1 SEP *",
			match:   []time.Time{time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
			noMatch: []time.Time{time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		},
		"named weekday": {
			// 2026-09-21 is a Monday.
			expr:    "0 0 * * MON",
			match:   []time.Time{time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)},
			noMatch: []time.Time{time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)},
		},
		// Standard cron accepts both 0 and 7 for Sunday. 2026-09-20 is one.
		"sunday as seven": {
			expr:    "0 0 * * 7",
			match:   []time.Time{time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)},
			noMatch: []time.Time{time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			schedule, err := parseCron(tc.expr)
			if err != nil {
				t.Fatalf("parseCron(%q): %v", tc.expr, err)
			}
			for _, at := range tc.match {
				if !schedule.matches(at) {
					t.Errorf("%q does not fire at %s, but should", tc.expr, at.Format(time.RFC3339))
				}
			}
			for _, at := range tc.noMatch {
				if schedule.matches(at) {
					t.Errorf("%q fires at %s, but should not", tc.expr, at.Format(time.RFC3339))
				}
			}
		})
	}
}

// Standard cron ORs day-of-month against day-of-week when both are
// restricted, rather than ANDing them. Getting this backwards makes
// "0 0 1 * MON" fire roughly never instead of on two kinds of day.
func TestParseCronOrsRestrictedDayFields(t *testing.T) {
	schedule, err := parseCron("0 0 1 * MON")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	// 2026-09-01 is the first of the month (a Tuesday).
	if !schedule.matches(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("did not fire on the first of the month")
	}
	// 2026-09-21 is a Monday, not the first.
	if !schedule.matches(time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)) {
		t.Error("did not fire on Monday")
	}
	if schedule.matches(time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)) {
		t.Error("fired on a Tuesday that is not the first")
	}
}

func TestParseCronRejectsUnusableExpressions(t *testing.T) {
	for name, expr := range map[string]string{
		"empty":                "  ",
		"too few fields":       "0 0 * *",
		"too many fields":      "0 0 * * * *",
		"minute out of range":  "60 0 * * *",
		"hour out of range":    "0 24 * * *",
		"day zero":             "0 0 0 * *",
		"month out of range":   "0 0 1 13 *",
		"weekday out of range": "0 0 * * 8",
		"inverted range":       "0 9-5 * * *",
		"zero step":            "*/0 * * * *",
		"not a number":         "zero 0 * * *",
		"unknown name":         "0 0 * SMARCH *",
		// A seconds field is a non-standard extension; accepting it silently
		// would shift every other field by one and run hourly instead of
		// daily.
		"six fields with seconds": "0 0 0 * * *",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCron(expr); err == nil {
				t.Errorf("parseCron(%q) was accepted", expr)
			}
		})
	}
}

func TestPrevFiring(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	tests := map[string]struct {
		expr string
		now  time.Time
		want time.Time
	}{
		"midday, daily midnight schedule": {
			expr: "0 0 * * *",
			now:  time.Date(2026, 9, 21, 12, 0, 0, 0, ist),
			want: time.Date(2026, 9, 21, 0, 0, 0, 0, ist),
		},
		// Exactly at the firing instant: that instant IS the previous
		// firing, not yesterday's.
		"exactly on the firing minute": {
			expr: "0 0 * * *",
			now:  time.Date(2026, 9, 21, 0, 0, 0, 0, ist),
			want: time.Date(2026, 9, 21, 0, 0, 0, 0, ist),
		},
		// A second before it, the previous firing is the day before.
		"one second before the firing minute": {
			expr: "0 0 * * *",
			now:  time.Date(2026, 9, 20, 23, 59, 59, 0, ist),
			want: time.Date(2026, 9, 20, 0, 0, 0, 0, ist),
		},
		// The answer must be in the schedule's zone, and 19:00 UTC is
		// already the 22nd there.
		"an instant given in another zone": {
			expr: "0 0 * * *",
			now:  time.Date(2026, 9, 21, 19, 0, 0, 0, time.UTC),
			want: time.Date(2026, 9, 22, 0, 0, 0, 0, ist),
		},
		// Must skip back over months, not just days.
		"a yearly schedule": {
			expr: "0 0 1 1 *",
			now:  time.Date(2026, 9, 21, 12, 0, 0, 0, ist),
			want: time.Date(2026, 1, 1, 0, 0, 0, 0, ist),
		},
		"a weekly schedule": {
			// 2026-09-21 is a Monday; the previous Sunday is the 20th.
			expr: "0 0 * * SUN",
			now:  time.Date(2026, 9, 21, 12, 0, 0, 0, ist),
			want: time.Date(2026, 9, 20, 0, 0, 0, 0, ist),
		},
		"within the same hour": {
			expr: "*/15 * * * *",
			now:  time.Date(2026, 9, 21, 12, 44, 0, 0, ist),
			want: time.Date(2026, 9, 21, 12, 30, 0, 0, ist),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			schedule, err := parseCron(tc.expr)
			if err != nil {
				t.Fatalf("parseCron: %v", err)
			}
			got, err := schedule.prevFiring(tc.now, ist)
			if err != nil {
				t.Fatalf("prevFiring: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("prevFiring = %s, want %s", got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

// Feb 29 only exists in a leap year, so the search has to look back further
// than a year before concluding there is no previous firing.
func TestPrevFiringLooksBackPastAYear(t *testing.T) {
	schedule, err := parseCron("0 0 29 2 *")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	got, err := schedule.prevFiring(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatalf("prevFiring: %v", err)
	}
	want := time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("prevFiring = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// An expression that can never fire must report that rather than returning a
// zero time a caller would read as "the epoch, so everything is overdue".
func TestPrevFiringRefusesAnImpossibleDate(t *testing.T) {
	schedule, err := parseCron("0 0 30 2 *")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	if _, err := schedule.prevFiring(time.Now(), time.UTC); err == nil {
		t.Error("February 30th reported a previous firing")
	}
}
