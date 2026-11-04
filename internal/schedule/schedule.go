// Package schedule decides when a node offers its models to the network:
// the owner's serving hours, such as overnight or at weekends. It knows only
// about the clock; the agent applies the answer.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata" // --serve-tz must work on machines without a zone database
)

const minutesPerDay = 24 * 60

// window is one daily span on the days in the mask. An end at or before the
// start runs past midnight and belongs to the day it starts on.
type window struct {
	days       uint8 // bit n = time.Weekday(n)
	start, end int   // minutes since midnight; end may be 1440
}

func (w window) on(d time.Weekday) bool { return w.days&(1<<uint(d)) != 0 }

func (w window) open(d time.Weekday, minute int) bool {
	if w.end > w.start {
		return w.on(d) && minute >= w.start && minute < w.end
	}
	if w.on(d) && minute >= w.start {
		return true
	}
	return w.on((d+6)%7) && minute < w.end
}

// Schedule is a set of serving windows in one time zone. A nil Schedule is
// always open, which is the agent's default.
type Schedule struct {
	windows []window
	loc     *time.Location
	specs   []string
}

// Parse reads windows of the form "22:00-07:00" (every day) or
// "mon-fri@20:00-07:00" and "sat,sun@00:00-24:00". An empty tz means the
// machine's local time; otherwise it is an IANA name such as "Asia/Dubai".
func Parse(specs []string, tz string) (*Schedule, error) {
	if len(specs) == 0 {
		if tz != "" {
			return nil, fmt.Errorf("--serve-tz needs --serve-hours")
		}
		return nil, nil
	}
	loc := time.Local
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("--serve-tz %q: %w", tz, err)
		}
		loc = l
	}
	s := &Schedule{loc: loc, specs: specs}
	for _, spec := range specs {
		w, err := parseWindow(spec)
		if err != nil {
			return nil, fmt.Errorf("--serve-hours %q: %w", spec, err)
		}
		s.windows = append(s.windows, w)
	}
	return s, nil
}

func parseWindow(spec string) (window, error) {
	w := window{days: 0x7f}
	hours := strings.TrimSpace(spec)
	if days, rest, ok := strings.Cut(hours, "@"); ok {
		mask, err := parseDays(days)
		if err != nil {
			return w, err
		}
		w.days, hours = mask, rest
	}
	from, to, ok := strings.Cut(hours, "-")
	if !ok {
		return w, fmt.Errorf("want HH:MM-HH:MM, optionally after days and @, as in mon-fri@20:00-07:00")
	}
	var err error
	if w.start, err = parseClock(from, false); err != nil {
		return w, err
	}
	if w.end, err = parseClock(to, true); err != nil {
		return w, err
	}
	if w.start == w.end {
		return w, fmt.Errorf("start and end are the same; use 00:00-24:00 for a whole day")
	}
	return w, nil
}

func parseClock(s string, allow24 bool) (int, error) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || len(mm) != 2 {
		return 0, fmt.Errorf("time %q: want HH:MM", s)
	}
	h, herr := strconv.Atoi(hh)
	m, merr := strconv.Atoi(mm)
	if herr != nil || merr != nil || h < 0 || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q: want HH:MM", s)
	}
	total := h*60 + m
	if total > minutesPerDay || (total == minutesPerDay && !allow24) {
		return 0, fmt.Errorf("time %q is out of range", s)
	}
	return total, nil
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// parseDays reads "mon", "sat,sun" or a range such as "mon-fri" or "fri-mon".
func parseDays(s string) (uint8, error) {
	var mask uint8
	for _, part := range strings.Split(strings.ToLower(s), ",") {
		from, to, isRange := strings.Cut(strings.TrimSpace(part), "-")
		if !isRange {
			to = from
		}
		a, okA := dayNames[from]
		b, okB := dayNames[to]
		if !okA || !okB {
			return 0, fmt.Errorf("days %q: use mon, tue, wed, thu, fri, sat, sun", part)
		}
		for d := a; ; d = (d + 1) % 7 {
			mask |= 1 << uint(d)
			if d == b {
				break
			}
		}
	}
	return mask, nil
}

// Open reports whether the node should serve at t.
func (s *Schedule) Open(t time.Time) bool {
	if s == nil {
		return true
	}
	t = t.In(s.loc)
	minute := t.Hour()*60 + t.Minute()
	for _, w := range s.windows {
		if w.open(t.Weekday(), minute) {
			return true
		}
	}
	return false
}

// Next returns the first minute after t at which Open changes, and false when
// it never does, as with every day 00:00-24:00.
func (s *Schedule) Next(t time.Time) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	now := s.Open(t)
	at := t.In(s.loc).Truncate(time.Minute)
	for i := 0; i <= 8*minutesPerDay; i++ {
		at = at.Add(time.Minute)
		if s.Open(at) != now {
			return at, true
		}
	}
	return time.Time{}, false
}

// String names the schedule for logs.
func (s *Schedule) String() string {
	if s == nil {
		return "always"
	}
	return strings.Join(s.specs, " ") + " (" + s.loc.String() + ")"
}
