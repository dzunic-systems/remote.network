package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, tz string, specs ...string) *Schedule {
	t.Helper()
	s, err := Parse(specs, tz)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// 2 Nov 2026 is a Monday.
func at(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC)
}

func TestOpen(t *testing.T) {
	cases := []struct {
		name  string
		specs []string
		when  time.Time
		want  bool
	}{
		{"same day inside", []string{"09:00-17:00"}, at(2, 9, 0), true},
		{"same day end is exclusive", []string{"09:00-17:00"}, at(2, 17, 0), false},
		{"overnight evening", []string{"22:00-07:00"}, at(2, 23, 30), true},
		{"overnight morning", []string{"22:00-07:00"}, at(3, 6, 59), true},
		{"overnight end is exclusive", []string{"22:00-07:00"}, at(3, 7, 0), false},
		{"overnight midday", []string{"22:00-07:00"}, at(3, 12, 0), false},
		{"weekday window on friday night", []string{"mon-fri@20:00-07:00"}, at(6, 21, 0), true},
		{"friday's window runs into saturday", []string{"mon-fri@20:00-07:00"}, at(7, 6, 0), true},
		{"no window starts on saturday", []string{"mon-fri@20:00-07:00"}, at(7, 21, 0), false},
		{"sunday's window not opened", []string{"mon-fri@20:00-07:00"}, at(2, 6, 0), false},
		{"whole weekend day", []string{"sat,sun@00:00-24:00"}, at(8, 23, 59), true},
		{"weekend window closed on monday", []string{"sat,sun@00:00-24:00"}, at(9, 0, 0), false},
		{"wrapping day range", []string{"fri-mon@10:00-11:00"}, at(8, 10, 30), true},
		{"wrapping day range excludes tuesday", []string{"fri-mon@10:00-11:00"}, at(3, 10, 30), false},
		{"second window", []string{"mon-fri@20:00-07:00", "sat,sun@00:00-24:00"}, at(7, 12, 0), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustParse(t, "UTC", c.specs...)
			if got := s.Open(c.when); got != c.want {
				t.Errorf("Open(%s) = %v, want %v", c.when, got, c.want)
			}
		})
	}
}

func TestOpenUsesTheZone(t *testing.T) {
	s := mustParse(t, "Asia/Dubai", "22:00-07:00") // UTC+4
	if !s.Open(at(2, 18, 0)) {
		t.Error("18:00 UTC is 22:00 in Dubai and should be open")
	}
	if s.Open(at(2, 17, 59)) {
		t.Error("17:59 UTC is 21:59 in Dubai and should be closed")
	}
}

func TestNext(t *testing.T) {
	s := mustParse(t, "UTC", "22:00-07:00")
	if got, ok := s.Next(at(2, 12, 0)); !ok || !got.Equal(at(2, 22, 0)) {
		t.Errorf("next from midday = %s, %v", got, ok)
	}
	if got, ok := s.Next(at(2, 23, 0)); !ok || !got.Equal(at(3, 7, 0)) {
		t.Errorf("next from evening = %s, %v", got, ok)
	}
	if _, ok := mustParse(t, "UTC", "00:00-24:00").Next(at(2, 12, 0)); ok {
		t.Error("an always-open schedule has no next change")
	}
}

func TestNilIsAlwaysOpen(t *testing.T) {
	s, err := Parse(nil, "")
	if err != nil || s != nil {
		t.Fatalf("Parse(nil) = %v, %v", s, err)
	}
	if !s.Open(at(2, 3, 0)) {
		t.Error("nil schedule should be open")
	}
	if _, ok := s.Next(at(2, 3, 0)); ok {
		t.Error("nil schedule has no next change")
	}
	if s.String() != "always" {
		t.Errorf("String = %q", s.String())
	}
}

func TestParseRejects(t *testing.T) {
	for _, spec := range []string{"", "22:00", "25:00-07:00", "22:60-07:00", "24:00-07:00", "9-17",
		"10:00-10:00", "noon@10:00-11:00", "mon-xyz@10:00-11:00", "mon-fri 20:00-07:00"} {
		if _, err := Parse([]string{spec}, ""); err == nil {
			t.Errorf("Parse(%q) accepted", spec)
		}
	}
	if _, err := Parse([]string{"10:00-11:00"}, "Not/AZone"); err == nil {
		t.Error("bad zone accepted")
	}
	if _, err := Parse(nil, "UTC"); err == nil {
		t.Error("--serve-tz without --serve-hours accepted")
	}
}
