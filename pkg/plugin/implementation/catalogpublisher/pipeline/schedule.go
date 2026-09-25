package pipeline

// schedule.go answers the two questions a tick has to answer before it runs
// anything: is this capability allowed to publish, and is it due?
//
// Both are deliberately pure decisions over values the caller supplies -- a
// registry record, a clock, the last run's timestamp -- so they can be tested
// against real answers without a scheduler, a database, or a live upstream.
// Wiring them to the crawler's own ticker is a separate, thin step.

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// publishAction is the action name the registry uses to say "this capability
// publishes a catalogue, and here is the pipeline that does it".
const publishAction = "publish"

// pipelinePathFor is the registry gate: it returns the pipeline definition the
// registry says this capability publishes with, and refuses otherwise.
//
// The path is NOT derived from the capability's name. Deriving it would mean
// guessing a filesystem layout and silently running a pipeline the registry
// never sanctioned; here a capability with no publish action simply has no
// pipeline, which is a fact rather than an error to paper over.
//
// The caller decides what an absent action means. For a capability that is
// only ever consumed (select-only, as every other capability in this network
// is today) it means "nothing to do", not "misconfigured".
func PipelinePathFor(record *model.ProviderRecord) (string, error) {
	if record == nil {
		return "", fmt.Errorf("no provider record: the registry did not confirm this capability")
	}

	plan, ok := record.Actions[publishAction]
	if !ok {
		return "", fmt.Errorf("%s serves no %q action: the registry lists %s, so there is no pipeline to run",
			record.BindingKey, publishAction, servedActions(record))
	}
	path := strings.TrimSpace(plan.Mappings)
	if path == "" {
		return "", fmt.Errorf("%s declares a %q action with no mappings, so it names no pipeline",
			record.BindingKey, publishAction)
	}
	return path, nil
}

// servedActions lists what a record does serve, for an error that says what is
// there rather than only what is missing.
func servedActions(record *model.ProviderRecord) string {
	if len(record.Actions) == 0 {
		return "no actions at all"
	}
	names := make([]string, 0, len(record.Actions))
	for name := range record.Actions {
		names = append(names, strconv.Quote(name))
	}
	return strings.Join(names, ", ")
}

// loadRegistryPipeline loads the pipeline the registry named, for this
// collector.
//
// It refuses any path that is not the collector's own. The registry is data an
// operator edits; a path pointing at some other capability's pipeline must
// read as "that is not mine to run" rather than quietly loading this one and
// publishing one capability's catalogues under another's name. A binary can
// only run what it embeds.
//
// Both spellings are accepted: the repo-relative path an operator pastes into
// a record, and the bare filename, which is what the embedded filesystem knows.
func loadRegistryPipeline(files Files, registryPath string) (Spec, error) {

	cleaned := path.Clean(strings.TrimSpace(registryPath))
	if cleaned == "." || cleaned == "" {
		return Spec{}, fmt.Errorf("the registry names no pipeline path")
	}
	if cleaned != files.Path && cleaned != path.Clean(files.RegistryPath) {
		return Spec{}, fmt.Errorf("the registry names pipeline %q, which is not this pipeline's own %s; "+
			"this binary can only run the pipelines it embeds", cleaned, files.RegistryPath)
	}
	return LoadSpec(files.FS, files.Path)
}

// TickDecision is everything a tick establishes before doing any work: which
// pipeline the registry sanctioned, what it says, and whether it is due.
type TickDecision struct {
	PipelinePath string
	Spec         Spec
	Due          bool
	Reason       string
}

// decideTick runs the pre-flight in order: the registry gate, then the
// pipeline it named, then that pipeline's own schedule.
//
// An error means the tick must not proceed -- the capability does not publish,
// or names a pipeline this binary does not have. A decision with Due false is
// the normal quiet outcome and carries the reason, so an operator asking "why
// did nothing run at 00:05" gets an answer instead of silence.
func decideTick(files Files, record *model.ProviderRecord, now, lastRun time.Time) (TickDecision, error) {
	pipelinePath, err := PipelinePathFor(record)
	if err != nil {
		return TickDecision{}, err
	}
	spec, err := loadRegistryPipeline(files, pipelinePath)
	if err != nil {
		return TickDecision{}, err
	}
	due, reason, err := dueNow(spec.Schedule, now, lastRun)
	if err != nil {
		return TickDecision{}, err
	}
	return TickDecision{PipelinePath: pipelinePath, Spec: spec, Due: due, Reason: reason}, nil
}

// dueNow decides whether a pipeline should run, given when it last ran.
//
// The rule is one line: find the most recent instant the cron expression
// fired, and run if the last run was before it. That covers every cadence a
// cron expression can state -- daily, hourly, weekdays only -- without this
// function knowing which one it is looking at.
//
// The expression is resolved in the pipeline's OWN timezone, not the host's.
// This one fires at midnight Asia/Kolkata; a host running UTC reading its own
// clock would fire on the wrong calendar day and ask the upstream for
// yesterday.
//
// lastRun is passed in rather than read here so this stays a pure decision.
// It must outlive the process -- a restart at 00:05 must not re-fire a run
// that already happened at 00:01 -- which is what the RunLog in run.go is
// for. A zero lastRun means "never ran".
func dueNow(schedule Schedule, now, lastRun time.Time) (bool, string, error) {
	location, err := time.LoadLocation(schedule.Timezone)
	if err != nil {
		return false, "", fmt.Errorf("schedule.timezone %q: %w", schedule.Timezone, err)
	}
	expr, err := parseCron(schedule.Cron)
	if err != nil {
		return false, "", fmt.Errorf("schedule.cron: %w", err)
	}

	firing, err := expr.prevFiring(now, location)
	if err != nil {
		return false, "", fmt.Errorf("schedule.cron: %w", err)
	}
	stamp := firing.Format("2006-01-02 15:04 MST")

	if lastRun.IsZero() {
		return true, fmt.Sprintf("due: %q last fired %s and this pipeline has never run", schedule.Cron, stamp), nil
	}
	if lastRun.Before(firing) {
		return true, fmt.Sprintf("due: %q fired %s, after the last run at %s",
			schedule.Cron, stamp, lastRun.In(location).Format("2006-01-02 15:04 MST")), nil
	}
	return false, fmt.Sprintf("not due: already ran at %s, at or after the %s firing",
		lastRun.In(location).Format("2006-01-02 15:04 MST"), stamp), nil
}

// cronSchedule is a parsed cron expression: one set of permitted values per
// field, plus whether each day field was restricted (see match's OR rule).
type cronSchedule struct {
	expr string

	minutes  map[int]bool
	hours    map[int]bool
	days     map[int]bool // day of month, 1-31
	months   map[int]bool // 1-12
	weekdays map[int]bool // 0-6, Sunday = 0

	daysRestricted     bool
	weekdaysRestricted bool
}

// cronField describes one field's legal range and any names it accepts.
type cronField struct {
	name     string
	min, max int
	names    map[string]int
}

var cronFields = []cronField{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: map[string]int{
		"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
		"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
	}},
	// 7 is accepted for Sunday as well as 0, as standard cron does, and is
	// normalised to 0 after parsing.
	{name: "day of week", min: 0, max: 7, names: map[string]int{
		"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
	}},
}

// parseCron reads a standard five-field cron expression.
func parseCron(expr string) (cronSchedule, error) {
	parts := strings.Fields(strings.TrimSpace(expr))
	if len(parts) != len(cronFields) {
		return cronSchedule{}, fmt.Errorf("cron %q has %d fields; a standard expression has %d "+
			"(minute hour day-of-month month day-of-week), and the six-field seconds variant is not supported",
			expr, len(parts), len(cronFields))
	}

	sets := make([]map[int]bool, len(parts))
	restricted := make([]bool, len(parts))
	for i, part := range parts {
		set, wildcard, err := parseCronField(part, cronFields[i])
		if err != nil {
			return cronSchedule{}, fmt.Errorf("cron %q: %w", expr, err)
		}
		sets[i], restricted[i] = set, !wildcard
	}

	// Normalise weekday 7 to 0 so both spellings of Sunday compare equal to
	// time.Weekday.
	if sets[4][7] {
		sets[4][0] = true
		delete(sets[4], 7)
	}

	return cronSchedule{
		expr:               strings.TrimSpace(expr),
		minutes:            sets[0],
		hours:              sets[1],
		days:               sets[2],
		months:             sets[3],
		weekdays:           sets[4],
		daysRestricted:     restricted[2],
		weekdaysRestricted: restricted[4],
	}, nil
}

// parseCronField expands one field ("*", "5", "1-4", "*/15", "0-12/6",
// "MON,WED") into the set of values it permits. The second return says whether
// the field was an unrestricted wildcard, which the day fields' OR rule needs.
func parseCronField(field string, spec cronField) (map[int]bool, bool, error) {
	set := make(map[int]bool)
	wildcard := false

	for _, term := range strings.Split(field, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			return nil, false, fmt.Errorf("%s field is empty", spec.name)
		}

		step := 1
		if slash := strings.Index(term, "/"); slash >= 0 {
			parsed, err := strconv.Atoi(term[slash+1:])
			if err != nil || parsed < 1 {
				return nil, false, fmt.Errorf("%s field step %q is not a positive number", spec.name, term[slash+1:])
			}
			step = parsed
			term = term[:slash]
		}

		low, high := spec.min, spec.max
		switch {
		case term == "*":
			if step == 1 {
				wildcard = true
			}
		case strings.Contains(term, "-"):
			bounds := strings.SplitN(term, "-", 2)
			var err error
			if low, err = cronValue(bounds[0], spec); err != nil {
				return nil, false, err
			}
			if high, err = cronValue(bounds[1], spec); err != nil {
				return nil, false, err
			}
			if low > high {
				return nil, false, fmt.Errorf("%s range %q runs backwards", spec.name, term)
			}
		default:
			value, err := cronValue(term, spec)
			if err != nil {
				return nil, false, err
			}
			low, high = value, value
		}

		for v := low; v <= high; v += step {
			set[v] = true
		}
	}

	if len(set) == 0 {
		return nil, false, fmt.Errorf("%s field %q permits no values", spec.name, field)
	}
	return set, wildcard, nil
}

// cronValue reads one number or name, and refuses anything outside the field's
// range -- an out-of-range value is a typo that would otherwise silently never
// match.
func cronValue(token string, spec cronField) (int, error) {
	token = strings.TrimSpace(token)
	if value, ok := spec.names[strings.ToUpper(token)]; ok {
		return value, nil
	}
	value, err := strconv.Atoi(token)
	if err != nil {
		return 0, fmt.Errorf("%s field value %q is neither a number nor a name", spec.name, token)
	}
	if value < spec.min || value > spec.max {
		return 0, fmt.Errorf("%s field value %d is outside %d-%d", spec.name, value, spec.min, spec.max)
	}
	return value, nil
}

// matches reports whether the expression fires at this instant, to the minute.
//
// The day rule is standard cron's, and is an OR rather than an AND: when BOTH
// day-of-month and day-of-week are restricted, either matching is enough. So
// "0 0 1 * MON" fires on the first of the month AND on every Monday, not only
// on Mondays that fall on the first.
func (c cronSchedule) matches(at time.Time) bool {
	if !c.minutes[at.Minute()] || !c.hours[at.Hour()] || !c.months[int(at.Month())] {
		return false
	}
	day, weekday := c.days[at.Day()], c.weekdays[int(at.Weekday())]
	switch {
	case c.daysRestricted && c.weekdaysRestricted:
		return day || weekday
	case c.daysRestricted:
		return day
	case c.weekdaysRestricted:
		return weekday
	default:
		return true
	}
}

// cronLookBack bounds the backwards search. Four years covers the longest gap
// a valid expression can have -- February 29th, which recurs every four years
// and can be up to eight years apart around a skipped century leap year, so
// this is generous rather than exact. Past it, the expression is one that
// cannot fire (February 30th) and the caller must be told so rather than
// handed a zero time it would read as "the epoch, so everything is overdue".
const cronLookBack = 8 * 366 * 24 * time.Hour

// prevFiring returns the most recent instant at or before now that this
// expression fires, resolved in loc.
//
// Backwards, not forwards, because the question being asked is "should this
// have run by now, and has it" -- a next-firing API answers a different
// question and forces the caller to keep state it would otherwise not need.
//
// Whole non-matching days are skipped in one step rather than minute by
// minute, so a yearly expression costs a few hundred iterations rather than
// half a million.
func (c cronSchedule) prevFiring(now time.Time, loc *time.Location) (time.Time, error) {
	local := now.In(loc).Truncate(time.Minute)
	floor := local.Add(-cronLookBack)

	// Start at the current minute and walk back.
	for at := local; at.After(floor); {
		if !c.dayMatches(at) {
			// Nothing this day can match: jump to 23:59 of the day before.
			midnight := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, loc)
			at = midnight.Add(-time.Minute)
			continue
		}
		if c.minutes[at.Minute()] && c.hours[at.Hour()] {
			return at, nil
		}
		at = at.Add(-time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron %q has not fired in the last %d days; it may describe a date that does not occur",
		c.expr, int(cronLookBack.Hours()/24))
}

// dayMatches applies the month and day fields alone -- the part of matches
// that is constant for a whole calendar day.
func (c cronSchedule) dayMatches(at time.Time) bool {
	if !c.months[int(at.Month())] {
		return false
	}
	day, weekday := c.days[at.Day()], c.weekdays[int(at.Weekday())]
	switch {
	case c.daysRestricted && c.weekdaysRestricted:
		return day || weekday
	case c.daysRestricted:
		return day
	case c.weekdaysRestricted:
		return weekday
	default:
		return true
	}
}
