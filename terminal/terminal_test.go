package terminal

import (
	"strings"
	"testing"
)

func TestSegments_ProgramChangeClosesAndOpens(t *testing.T) {
	s := NewSession("p1", "bash", Lines, 1)
	s.AppendLine("hello")
	s.ProgramChanged("vim", Frames, 2) // closes bash segment, opens frames
	s.AppendFrame(3, "frame one")
	s.ProgramChanged("bash", Lines, 4) // back to lines
	s.AppendLine("after vim")

	segs := s.Segments()
	if len(segs) != 3 {
		t.Fatalf("want 3 segments, got %d: %+v", len(segs), segs)
	}
	if segs[0].Program != "bash" || segs[0].Kind != Lines || len(segs[0].Lines) != 1 || segs[0].End != 2 {
		t.Errorf("seg0 = %+v", segs[0])
	}
	if segs[1].Program != "vim" || segs[1].Kind != Frames || len(segs[1].Frames) != 1 || segs[1].Start != 2 || segs[1].End != 4 {
		t.Errorf("seg1 = %+v", segs[1])
	}
	if segs[2].Program != "bash" || segs[2].Kind != Lines || len(segs[2].Lines) != 1 || segs[2].End != 0 {
		t.Errorf("seg2 (open) = %+v", segs[2])
	}
}

func TestSegments_SameProgramNoNewSegment(t *testing.T) {
	s := NewSession("p1", "bash", Lines, 1)
	s.ProgramChanged("bash", Lines, 2) // no-op
	s.AppendLine("x")
	if len(s.Segments()) != 1 {
		t.Fatalf("same program must not split; got %d segments", len(s.Segments()))
	}
}

func TestAppendLine_OnlyLinesSegments(t *testing.T) {
	s := NewSession("p1", "vim", Frames, 1)
	s.AppendLine("ignored")
	if segs := s.Segments(); len(segs[0].Lines) != 0 {
		t.Errorf("frames segment must not keep lines: %+v", segs[0])
	}
}

func TestAppendFrame_OnlyFramesSegments(t *testing.T) {
	s := NewSession("p1", "bash", Lines, 1)
	s.AppendFrame(2, "ignored")
	if segs := s.Segments(); len(segs[0].Frames) != 0 {
		t.Errorf("lines segment must not keep frames: %+v", segs[0])
	}
}

func TestQueryHistory_TailDefault(t *testing.T) {
	s := NewSession("p1", "bash", Lines, 1)
	for i := 0; i < 50; i++ {
		s.AppendLine(strings.Repeat("x", i) + " line")
	}
	out := s.QueryHistory(Query{}) // default tail 200 → everything here
	if !strings.HasSuffix(out, strings.Repeat("x", 49)+" line") {
		t.Errorf("tail should end with the newest line; got %q", out[len(out)-40:])
	}
	out = s.QueryHistory(Query{Tail: 3})
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], strings.Repeat("x", 47)) {
		t.Errorf("tail 3 = %q", lines)
	}
}

func TestQueryHistory_Head(t *testing.T) {
	s := NewSession("p1", "bash", Lines, 1)
	for i := 0; i < 10; i++ {
		s.AppendLine("line")
	}
	out := s.QueryHistory(Query{Head: 2})
	if strings.Count(out, "line") != 2 || !strings.HasPrefix(out, "line\nline") {
		t.Errorf("head 2 = %q", out)
	}
}

func TestQueryHistory_GrepWithContext(t *testing.T) {
	s := NewSession("p1", "bash", Lines, 1)
	for _, ln := range []string{"a", "b", "ERROR boom", "d", "e", "f", "another error here", "h"} {
		s.AppendLine(ln)
	}
	out := s.QueryHistory(Query{Grep: "error", Context: 1})
	want := []string{"b", "ERROR boom", "d", "⋮", "f", "another error here", "h"}
	got := strings.Split(out, "\n")
	if len(got) != len(want) {
		t.Fatalf("grep ctx = %q; want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestQueryHistory_SegmentTarget(t *testing.T) {
	s := NewSession("p1", "bash", Lines, 1)
	s.AppendLine("shell line")
	s.ProgramChanged("vim", Frames, 2)
	s.AppendFrame(3, "F1")
	s.AppendFrame(4, "F1\nchanged")

	// Segment 0 only: the shell line, no frames.
	i0 := 0
	out := s.QueryHistory(Query{SegmentIdx: &i0})
	if out != "shell line" {
		t.Errorf("seg0 = %q", out)
	}
	// Segment 1 only: frames as diffs (first full, second diffed).
	i1 := 1
	out = s.QueryHistory(Query{SegmentIdx: &i1})
	if !strings.Contains(out, "F1") || !strings.Contains(out, "+changed") {
		t.Errorf("seg1 = %q", out)
	}
	// Program filter.
	out = s.QueryHistory(Query{Program: "bash"})
	if out != "shell line" {
		t.Errorf("program=bash = %q", out)
	}
}

func TestQueryHistory_FramesAcrossSegments(t *testing.T) {
	s := NewSession("p1", "vim", Frames, 1)
	s.AppendFrame(2, "A")
	s.ProgramChanged("bash", Lines, 3)
	s.AppendLine("back in shell")
	segs := s.Segments()
	if len(segs) != 2 || segs[0].Program != "vim" || segs[1].Program != "bash" {
		t.Fatalf("segments after one change = %+v", segs)
	}
	s.ProgramChanged("htop", Frames, 4) // a DIFFERENT program → new frames segment
	s.AppendFrame(5, "C") // its first frame renders full
	segs = s.Segments()
	t.Logf("segments now: %+v", segs)
	out := s.QueryHistory(Query{})
	if !strings.Contains(out, "C") || !strings.Contains(out, "back in shell") {
		t.Errorf("mixed history = %q", out)
	}
}

func TestQueryHistory_LimitKeepsTail(t *testing.T) {
	s := NewSession("p1", "bash", Lines, 1)
	for i := 0; i < 100; i++ {
		s.AppendLine("line")
	}
	out := s.QueryHistory(Query{Limit: 10})
	lines := strings.Split(out, "\n")
	if len(lines) != 11 || !strings.HasPrefix(lines[0], "… (90 earlier lines elided)") {
		t.Errorf("limit = %q", lines[:2])
	}
}

func TestDiffFrames(t *testing.T) {
	a := "l1\nl2\nl3\nl4"
	b := "l1\nL2\nl3\nl4"
	if d := DiffFrames(a, b); d != "+L2" {
		t.Errorf("middle change = %q", d)
	}
	if d := DiffFrames(a, a); d != "" {
		t.Errorf("identical = %q", d)
	}
	b2 := "l1\nl2\nNEW\nl3\nl4"
	if d := DiffFrames(a, b2); d != "+NEW" {
		t.Errorf("insertion = %q", d)
	}
	b3 := "l1\nl2\nl3"
	if d := DiffFrames(a, b3); d != "" {
		t.Errorf("trailing removal elides (no + lines) = %q", d)
	}
}

func TestDiffFrames_CapsLongDiffs(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 100; i++ {
		a.WriteString("same\n")
		b.WriteString("DIFF\n")
	}
	d := DiffFrames(strings.TrimRight(a.String(), "\n"), strings.TrimRight(b.String(), "\n"))
	if !strings.Contains(d, "… (40 changed lines)") {
		t.Errorf("long diff not capped: %d lines", strings.Count(d, "\n"))
	}
}

func TestGrepLines_MergesOverlappingContext(t *testing.T) {
	lines := []string{"a", "hit1", "hit2", "c"}
	out := grepLines(lines, "hit", 1)
	want := []string{"a", "hit1", "hit2", "c"} // contexts overlap → one run
	if len(out) != len(want) {
		t.Fatalf("merged = %q", out)
	}
}

func TestSession_ConcurrentFeed(t *testing.T) {
	s := NewSession("p1", "bash", Lines, 1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			s.AppendLine("x")
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		s.QueryHistory(Query{Tail: 5})
		s.Segments()
	}
	<-done
	if len(s.Segments()) != 1 {
		t.Fatal("segment count changed under concurrency")
	}
}
