package pipeline

// schedule_test.go pins the two decisions a tick makes before doing any work.
// Both are cheap to get subtly wrong and expensive to notice: a gate that
// passes when the registry never sanctioned the capability publishes
// something nobody asked for, and a schedule resolved in the wrong timezone
// fires on the wrong calendar day, every day, while looking healthy.

import (
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
	schedule := Schedule{Cron: "0 0 * * *", Timezone: "Asia/Kolkata"}
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
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
			schedule: Schedule{Cron: "0 6 * * *", Timezone: "Asia/Kolkata"},
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
			due, reason, err := dueNow(using, tc.now, tc.lastRun)
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
		"bad timezone":      {Cron: "0 0 * * *", Timezone: "Mars/Olympus"},
		"not cron at all":   {Cron: "midnight", Timezone: "Asia/Kolkata"},
		"hour out of range": {Cron: "0 26 * * *", Timezone: "Asia/Kolkata"},
		"no schedule":       {Timezone: "Asia/Kolkata"},
		// A date that never occurs parses fine and then never fires. Silence
		// is the failure mode, so it has to be an error at decision time.
		"a date that never occurs": {Cron: "0 0 30 2 *", Timezone: "Asia/Kolkata"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := dueNow(schedule, time.Now(), time.Time{}); err == nil {
				t.Error("an unusable schedule was accepted")
			}
		})
	}
}

// The registry stores a repo-relative path; the embedded filesystem knows only
// a filename. loadRegistryPipeline is the join, and it is also a boundary: a
// path pointing at some other package's pipeline must not silently load ours.
func TestLoadRegistryPipelineAcceptsThisPackagesOwnPath(t *testing.T) {
	for _, path := range []string{
		fixtureRegistryPath,
		"./" + fixtureRegistryPath,
		fixturePipelinePath,
	} {
		t.Run(path, func(t *testing.T) {
			spec, err := loadRegistryPipeline(fixturePipeline(), path)
			if err != nil {
				t.Fatalf("loadRegistryPipeline(%q): %v", path, err)
			}
			if spec.Metadata.Capability == "" {
				t.Error("loaded a spec with no capability")
			}
		})
	}
}

func TestLoadRegistryPipelineRefusesAnotherPackagesPipeline(t *testing.T) {
	for name, path := range map[string]string{
		"another package":    "pkg/plugin/implementation/WeatherObservation/cataloguepublish-imd/imd.yaml",
		"same name, else":    "pkg/plugin/implementation/Other/minimal.yaml",
		"escaping traversal": "pkg/plugin/implementation/Example/cataloguepublish-example/../../minimal.yaml",
		"empty":              "  ",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadRegistryPipeline(fixturePipeline(), path); err == nil {
				t.Errorf("loadRegistryPipeline(%q) loaded the collector's own pipeline anyway", path)
			}
		})
	}
}

// decideTick is the whole pre-flight: gate, then load, then schedule. A live
// tick runs exactly this before it touches the upstream.
func TestDecideTick(t *testing.T) {
	record := &model.ProviderRecord{
		BindingKey: "exampleco|example:Thing",
		Actions: map[string]model.ActionPlan{
			"select":  {Method: "GET", Path: "/v1/fetch"},
			"publish": {Mappings: fixtureRegistryPath},
		},
	}
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	decision, err := decideTick(fixturePipeline(), record, time.Date(2026, 9, 22, 6, 0, 0, 0, ist), time.Time{})
	if err != nil {
		t.Fatalf("decideTick: %v", err)
	}
	if !decision.Due {
		t.Errorf("not due at 06:00 IST having never run: %s", decision.Reason)
	}
	if decision.Spec.Metadata.Capability == "" {
		t.Error("decision carries no spec; the caller has nothing to run")
	}
	if decision.PipelinePath == "" {
		t.Error("decision does not say which pipeline it resolved")
	}

	// Same record an hour later, having just run: not due, and no error --
	// "already ran" is a normal outcome, not a failure.
	ranAt := time.Date(2026, 9, 22, 6, 0, 0, 0, ist)
	decision, err = decideTick(fixturePipeline(), record, ranAt.Add(time.Hour), ranAt)
	if err != nil {
		t.Fatalf("decideTick after a run: %v", err)
	}
	if decision.Due {
		t.Error("due again an hour after running; this publishes twice a day")
	}
}

func TestDecideTickRefusesACapabilityThatDoesNotPublish(t *testing.T) {
	record := &model.ProviderRecord{
		BindingKey: "mausamgram|openagrinet:WeatherObservation",
		Actions:    map[string]model.ActionPlan{"select": {Method: "GET"}},
	}
	if _, err := decideTick(fixturePipeline(), record, time.Now(), time.Time{}); err == nil {
		t.Fatal("a select-only capability produced a tick decision")
	}
}
