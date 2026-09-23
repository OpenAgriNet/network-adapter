package pipeline

// cron.go is a standard five-field cron parser and a "when did this last fire"
// search over it.
//
// Five fields, no extensions: minute, hour, day-of-month, month, day-of-week.
// Deliberately NOT the six-field seconds variant some libraries accept -- the
// two are indistinguishable by shape, so a deployment that pasted a six-field
// expression into a five-field parser would run hourly while believing it ran
// daily. Rejecting the extra field is the only way that mistake is visible.
//
// This is hand-rolled rather than pulled in as a dependency because the whole
// surface used here is one expression evaluated against one instant, and the
// part worth being careful about -- searching BACKWARDS, in a named timezone,
// across month and year boundaries -- is not what a cron library's next-fire
// API is shaped for.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

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
