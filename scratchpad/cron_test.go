package scratchpad

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Spec {
	t.Helper()
	s, err := ParseCron(expr)
	if err != nil {
		t.Fatalf("ParseCron(%q): %v", expr, err)
	}
	return s
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{
		"", "0 0", "60 0 * * *", "0 24 * * *", "* * 0 * *", "* * * 13 *",
		"* * * * 8", "a b c d e", "*/0 * * * *", "5-1 * * * *", "1, * * * *",
	} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q): want error, got nil", expr)
		}
	}
}

func TestNextDaily(t *testing.T) {
	s := mustParse(t, "30 9 * * *") // 09:30 every day
	from := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	got, ok := s.Next(from)
	if !ok {
		t.Fatal("no next")
	}
	want := time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// Exactly on the minute → strictly after.
	from = time.Date(2026, 8, 23, 9, 30, 0, 0, time.UTC)
	got, _ = s.Next(from)
	if !got.Equal(want) {
		t.Fatalf("on-boundary: got %v want %v", got, want)
	}
}

func TestNextWeekdayMorning(t *testing.T) {
	s := mustParse(t, "0 8 * * 1-5") // 08:00 Mon-Fri
	from := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) // a Sunday
	got, ok := s.Next(from)
	if !ok {
		t.Fatal("no next")
	}
	want := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC) // Monday
	if !got.Equal(want) || got.Weekday() != time.Monday {
		t.Fatalf("got %v (%s) want %v", got, got.Weekday(), want)
	}
}

func TestNextDowSevenIsSunday(t *testing.T) {
	s := mustParse(t, "0 0 * * 7") // midnight Sundays (7 == Sunday)
	from := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) // Sunday noon
	got, ok := s.Next(from)
	if !ok {
		t.Fatal("no next")
	}
	want := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC) // next Sunday
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextDomDowOrSemantics(t *testing.T) {
	// dom=1 OR dow=Monday (both restricted → either matches).
	s := mustParse(t, "0 0 1 * 1")
	from := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) // Sunday
	got, ok := s.Next(from)
	if !ok {
		t.Fatal("no next")
	}
	want := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC) // Monday (dow match, before the 1st)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextStepAndList(t *testing.T) {
	s := mustParse(t, "*/15 9-10 * * *") // :00/:15/:30/:45 during 09:00-10:59
	from := time.Date(2026, 8, 23, 9, 20, 0, 0, time.UTC)
	got, ok := s.Next(from)
	if !ok {
		t.Fatal("no next")
	}
	want := time.Date(2026, 8, 23, 9, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// After the window closes → next day.
	from = time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC)
	got, _ = s.Next(from)
	want = time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("post-window: got %v want %v", got, want)
	}
}

func TestNextMonthJump(t *testing.T) {
	s := mustParse(t, "0 12 15 * *") // noon on the 15th
	from := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	got, ok := s.Next(from)
	if !ok {
		t.Fatal("no next")
	}
	want := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextRunWrapper(t *testing.T) {
	from := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	ns, err := NextRun("30 9 * * *", from)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC).UnixNano()
	if ns != want {
		t.Fatalf("got %d want %d", ns, want)
	}
	if _, err := NextRun("not a cron", from); err == nil {
		t.Fatal("want error for bad spec")
	}
}
