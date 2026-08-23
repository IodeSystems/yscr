package scratchpad

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec is a parsed 5-field cron expression: minute hour dom month dow.
// It supports the common subset — "*", "*/n", "a", "a-b", and comma lists of
// those — in each field. No names, no @-macros, no L/W/# extensions; anything
// else is a parse error rather than a silent mis-schedule.
type Spec struct {
	Minute []bool // 60
	Hour   []bool // 24
	Dom    []bool // 31
	Month  []bool // 12
	Dow    []bool // 7, 0=Sunday

	domAll bool // dom field was "*" (dom/dow OR-semantics, per cron)
	dowAll bool
}

// ParseCron parses a 5-field cron spec. Every part must be valid — a silently
// dropped part would mis-schedule, so malformed input is an error.
func ParseCron(expr string) (*Spec, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields, got %d", expr, len(fields))
	}
	s := &Spec{}
	var err error
	if s.Minute, err = parseField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute %q: %w", fields[0], err)
	}
	if s.Hour, err = parseField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour %q: %w", fields[1], err)
	}
	s.domAll = fields[2] == "*"
	if s.Dom, err = parseField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day-of-month %q: %w", fields[2], err)
	}
	if s.Month, err = parseField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month %q: %w", fields[3], err)
	}
	s.dowAll = fields[4] == "*"
	if s.Dow, err = parseDow(fields[4]); err != nil {
		return nil, fmt.Errorf("day-of-week %q: %w", fields[4], err)
	}
	return s, nil
}

// Next returns the first time strictly after t matching the spec, scanning at
// minute resolution with field jumps up to a 5-year horizon (enough for any
// real schedule).
func (s *Spec) Next(t time.Time) (time.Time, bool) {
	cand := t.Truncate(time.Minute).Add(time.Minute)
	deadline := cand.AddDate(5, 0, 0)
	for cand.Before(deadline) {
		if !s.Month[int(cand.Month())-1] {
			cand = time.Date(cand.Year(), cand.Month()+1, 1, 0, 0, 0, 0, cand.Location())
			continue
		}
		if !s.Hour[cand.Hour()] {
		cand = time.Date(cand.Year(), cand.Month(), cand.Day(), cand.Hour()+1, 0, 0, 0, cand.Location())
			continue
		}
		if !s.dayMatches(cand) {
			cand = time.Date(cand.Year(), cand.Month(), cand.Day()+1, 0, 0, 0, 0, cand.Location())
			continue
		}
		if !s.Minute[cand.Minute()] {
			cand = cand.Add(time.Minute)
			continue
		}
		return cand, true
	}
	return time.Time{}, false
}

// dayMatches applies cron's dom/dow semantics: when both are restricted, a day
// matches if EITHER does; when one is "*", the other decides.
func (s *Spec) dayMatches(t time.Time) bool {
	domOK := s.Dom[t.Day()-1]
	dowOK := s.Dow[int(t.Weekday())]
	switch {
	case s.domAll && s.dowAll:
		return true
	case s.domAll:
		return dowOK
	case s.dowAll:
		return domOK
	default:
		return domOK || dowOK
	}
}

// parseField parses one numeric field into a set; every part must be valid.
func parseField(f string, lo, hi int) ([]bool, error) {
	out := make([]bool, hi-lo+1)
	for _, part := range strings.Split(f, ",") {
		if err := setRange(out, part, lo, hi); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseDow parses the day-of-week field (0-6, 0=Sunday; 7 accepted as Sunday).
func parseDow(f string) ([]bool, error) {
	if f == "7" || f == "*/7" {
		f = "0"
	}
	return parseField(f, 0, 6)
}

// setRange writes one part ("*", "a", "a-b", with optional "/step") into out.
func setRange(out []bool, part string, lo, hi int) error {
	step := 1
	if i := strings.Index(part, "/"); i >= 0 {
		n, err := strconv.Atoi(part[i+1:])
		if err != nil || n < 1 {
			return fmt.Errorf("bad step in %q", part)
		}
		step = n
		part = part[:i]
	}
	start, end := lo, hi
	switch {
	case part == "*":
		// full range
	case strings.Contains(part, "-"):
		bounds := strings.SplitN(part, "-", 2)
		a, err1 := strconv.Atoi(bounds[0])
		b, err2 := strconv.Atoi(bounds[1])
		if err1 != nil || err2 != nil || a < lo || b > hi || a > b {
			return fmt.Errorf("bad range %q", part)
		}
		start, end = a, b
	default:
		v, err := strconv.Atoi(part)
		if err != nil || v < lo || v > hi {
			return fmt.Errorf("bad value %q", part)
		}
		start, end = v, v
	}
	for v := start; v <= end; v += step {
		out[v-lo] = true
	}
	return nil
}
