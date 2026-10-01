package pipeline

// schedule_test.go pins the two decisions a tick makes before doing any work.
// Both are cheap to get subtly wrong and expensive to notice: a gate that
// passes when the registry never sanctioned the capability publishes
// something nobody asked for, and a schedule resolved against the wrong UTC
// offset fires on the wrong calendar day, every day, while looking healthy.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

func TestPipelinePathForFindsThePublishAction(t *testing.T) {
	record := &model.ProviderRecord{
		BindingKey: "exampleco|example:Thing",
		Actions: map[string]model.ActionPlan{
			"select":  {Method: "GET", Path: "/v1/fetch", Mappings: "https://example.test/select.yaml"},
			"publish": {Mappings: fixtureRegistryPath},
		},
	}

	path, err := PipelinePathFor(record)
	if err != nil {
		t.Fatalf("pipelinePathFor: %v", err)
	}
	if want := fixtureRegistryPath; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// A select-only capability is the normal case on this network -- every other
// capability registered today is select-only -- so it must read as "nothing to
// publish", not as a broken deployment.
func TestPipelinePathForRefusesASelectOnlyCapability(t *testing.T) {
	record := &model.ProviderRecord{
		BindingKey: "mausamgram|openagrinet:WeatherObservation",
		Actions: map[string]model.ActionPlan{
			"select": {Method: "GET", Path: "/nwpapi/get-daily"},
		},
	}

	_, err := PipelinePathFor(record)
	if err == nil {
		t.Fatal("a select-only capability yielded a pipeline path")
	}
	// The message has to say what IS served, or an operator cannot tell a
	// misconfiguration from a capability that simply does not publish.
	if !strings.Contains(err.Error(), `"select"`) {
		t.Errorf("error %q does not say which actions the capability does serve", err)
	}
}

func TestPipelinePathForRefusesAPublishActionNamingNoPipeline(t *testing.T) {
	record := &model.ProviderRecord{
		BindingKey: "exampleco|example:Thing",
		Actions:    map[string]model.ActionPlan{"publish": {Mappings: "   "}},
	}
	if _, err := PipelinePathFor(record); err == nil {
		t.Fatal("a publish action with no mappings yielded a pipeline path")
	}
}

func TestPipelinePathForRefusesAnUnconfirmedCapability(t *testing.T) {
	if _, err := PipelinePathFor(nil); err == nil {
		t.Fatal("a nil record -- the registry confirming nothing -- yielded a pipeline path")
	}
}

func TestDueNow(t *testing.T) {
	schedule := Schedule{Cron: "0 0 * * *", UTCOffset: "+05:30"}
	ist, err := parseUTCOffset("+05:30")
	if err != nil {
		t.Fatalf("parseUTCOffset: %v", err)
	}

	tests := map[string]struct {
		schedule Schedule // zero value means the midnight schedule above
		now      time.Time
		lastRun  time.Time
		want     bool
	}{
		// 19:00 UTC is 00:30 IST the NEXT day. Against a 06:00 IST schedule
		// the most recent firing is 06:00 on the PREVIOUS IST day, which the
		// pipeline already ran at -- so nothing is due. A host reading the
		// clock in UTC lands on a different day's firing entirely.
		"before the next firing, in IST": {
			schedule: Schedule{Cron: "0 6 * * *", UTCOffset: "+05:30"},
			now:      time.Date(2026, 9, 21, 19, 0, 0, 0, time.UTC),
			lastRun:  time.Date(2026, 9, 21, 6, 0, 0, 0, ist),
			want:     false,
		},
		// Same instant, against the midnight schedule: 00:00 IST has passed
		// on the IST calendar day that just began, and it has never run.
		"just after midnight IST, never run": {
			now:  time.Date(2026, 9, 21, 19, 0, 0, 0, time.UTC),
			want: true,
		},
		"already ran after today's firing time": {
			now:     time.Date(2026, 9, 22, 6, 0, 0, 0, ist),
			lastRun: time.Date(2026, 9, 22, 0, 1, 0, 0, ist),
			want:    false,
		},
		// Yesterday's run must not satisfy today's schedule.
		"last ran yesterday": {
			now:     time.Date(2026, 9, 22, 6, 0, 0, 0, ist),
			lastRun: time.Date(2026, 9, 21, 0, 1, 0, 0, ist),
			want:    true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			using := tc.schedule
			if using == (Schedule{}) {
				using = schedule
			}
			due, _, reason, err := dueNow(using, tc.now, tc.lastRun)
			if err != nil {
				t.Fatalf("dueNow: %v", err)
			}
			if due != tc.want {
				t.Errorf("due = %v, want %v (%s)", due, tc.want, reason)
			}
			if reason == "" {
				t.Error("dueNow gave no reason; an operator asking why nothing ran needs one")
			}
		})
	}
}

func TestDueNowRejectsAnUnusableSchedule(t *testing.T) {
	for name, schedule := range map[string]Schedule{
		"bad utcOffset":     {Cron: "0 0 * * *", UTCOffset: "Asia/Kolkata"},
		"not cron at all":   {Cron: "midnight", UTCOffset: "+05:30"},
		"hour out of range": {Cron: "0 26 * * *", UTCOffset: "+05:30"},
		"no schedule":       {UTCOffset: "+05:30"},
		// A date that never occurs parses fine and then never fires. Silence
		// is the failure mode, so it has to be an error at decision time.
		"a date that never occurs": {Cron: "0 0 30 2 *", UTCOffset: "+05:30"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := dueNow(schedule, time.Now(), time.Time{}); err == nil {
				t.Error("an unusable schedule was accepted")
			}
		})
	}
}

// The registry names the pipeline by its URL, and a run is given the pipeline
// it is to run. loadRegistryPipeline is the join, and it is a boundary: a
// record naming some other URL must not silently run this one.
func TestLoadRegistryPipelineAcceptsItsOwnURL(t *testing.T) {
	for _, named := range []string{fixtureRegistryPath, "  " + fixtureRegistryPath + " "} {
		spec, err := loadRegistryPipeline(context.Background(), fixturePipeline(), named, nil)
		if err != nil {
			t.Fatalf("loadRegistryPipeline(%q): %v", named, err)
		}
		if spec.Metadata.Capability == "" {
			t.Error("loaded a spec with no capability")
		}
	}
}

func TestLoadRegistryPipelineRefusesAnotherPipeline(t *testing.T) {
	for name, named := range map[string]string{
		"another pipeline": fixtureHost + "/testdata/other.yaml",
		"a repo path":      "pkg/plugin/implementation/Example/publish/minimal.yaml",
		"empty":            "  ",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadRegistryPipeline(context.Background(), fixturePipeline(), named, nil); err == nil {
				t.Errorf("loadRegistryPipeline(%q) ran this pipeline anyway", named)
			}
		})
	}
}

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
	ist, err := parseUTCOffset("+05:30")
	if err != nil {
		t.Fatalf("parseUTCOffset: %v", err)
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

// parseUTCOffset accepts the fixed forms a schedule can use and refuses
// everything else, including an IANA zone name -- this engine has no
// zoneinfo database to resolve one against.
func TestParseUTCOffsetAcceptsFixedOffsetsAndZ(t *testing.T) {
	tests := map[string]struct {
		raw             string
		wantSecondsEast int
	}{
		"IST":               {raw: "+05:30", wantSecondsEast: 5*3600 + 30*60},
		"a negative offset": {raw: "-04:00", wantSecondsEast: -4 * 3600},
		"Z means UTC":       {raw: "Z", wantSecondsEast: 0},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			loc, err := parseUTCOffset(tc.raw)
			if err != nil {
				t.Fatalf("parseUTCOffset(%q): %v", tc.raw, err)
			}
			_, gotSeconds := time.Date(2026, 1, 1, 0, 0, 0, 0, loc).Zone()
			if gotSeconds != tc.wantSecondsEast {
				t.Errorf("parseUTCOffset(%q) offset = %ds, want %ds", tc.raw, gotSeconds, tc.wantSecondsEast)
			}
		})
	}
}

func TestParseUTCOffsetRejectsAnythingElse(t *testing.T) {
	for name, raw := range map[string]string{
		"an IANA zone name":   "Asia/Kolkata",
		"no sign":             "05:30",
		"missing the colon":   "+0530",
		"hour out of range":   "+24:00",
		"minute out of range": "+05:60",
		"empty":               "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseUTCOffset(raw); err == nil {
				t.Errorf("parseUTCOffset(%q) was accepted", raw)
			}
		})
	}
}
