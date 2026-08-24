// Package terminal is the model for a TERMINAL SESSION and its HISTORIES.
//
// A terminal session is a live program surface (today: one tmux pane — the
// tmux activity yields these; tomorrow: anything else that yields one). Its
// history is not one blob: it is SEGMENTED BY PROGRAM. When the foreground
// program in the surface changes, the current segment closes and a new one
// opens. Each segment carries its own KIND:
//
//   - lines  — normal screen (shells, builds, log tails): the scrollback;
//     head / tail / grep-able line history.
//   - frames — alternate screen (claude, vim, htop): throttled keyframe
//     captures; each frame is reported as the DIFF vs its predecessor.
//
// The kind is a property of the SEGMENT (i.e. of the program running in it),
// not of the surface — so a pane that runs bash, then vim, then bash again has
// three segments: lines / frames / lines. A query over the session walks
// segments; a query can also target one segment (by index or by program).
package terminal

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Kind is how a segment's output is read back.
type Kind string

const (
	Lines  Kind = "lines"  // normal screen: scrollback, head/tail/grep
	Frames Kind = "frames" // alternate screen: throttled keyframes + diffs
)

// Segment is one program-stretch of a session's history.
type Segment struct {
	Program string
	Kind    Kind
	Start   int64 // ns — when the program took the surface
	End     int64 // ns — 0 while open
	Lines   []string
	Frames  []Frame
}

// Frame is one keyframe capture of a frames segment.
type Frame struct {
	At   int64 // ns
	Text string // full rendered frame
}

// diff renders this frame (index i) vs the previous one ("" when unchanged).
func (seg *Segment) diff(i int) string {
	if i <= 0 {
		return seg.Frames[i].Text
	}
	return DiffFrames(seg.Frames[i-1].Text, seg.Frames[i].Text)
}

// Session is the history model for one terminal surface. It is fed by an
// activity (tmux today): AppendLine/AppendFrame are called as output arrives,
// and ProgramChanged closes the open segment when the foreground program flips.
type Session struct {
	mu       sync.Mutex
	id       string
	segments []Segment
	open     *Segment // the live segment (nil only before first feed)
}

// NewSession starts a session already in its first segment (the program that
// is running now). The open segment lives ONLY in s.open — the closed-segment
// slice grows as programs change.
func NewSession(id, program string, kind Kind, at int64) *Session {
	return &Session{id: id, open: &Segment{Program: program, Kind: kind, Start: at}}
}

// ID is the session's address (the surface's stable id — a tmux pane id).
func (s *Session) ID() string { return s.id }

// AppendLine adds output to the open segment. A lines segment keeps its last
// 5000 lines; a frames segment ignores lines (its renderer owns the screen).
func (s *Session) AppendLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil || s.open.Kind != Lines {
		return
	}
	s.open.Lines = append(s.open.Lines, line)
	if len(s.open.Lines) > 5000 {
		s.open.Lines = s.open.Lines[len(s.open.Lines)-5000:]
	}
}

// AppendFrame captures a keyframe into the open segment (frames segments only;
// at most 64 kept).
func (s *Session) AppendFrame(at int64, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil || s.open.Kind != Frames {
		return
	}
	s.open.Frames = append(s.open.Frames, Frame{At: at, Text: text})
	if len(s.open.Frames) > 64 {
		s.open.Frames = s.open.Frames[len(s.open.Frames)-64:]
	}
}

// ProgramChanged closes the open segment and opens a new one when the
// foreground program (or its screen kind) changed. No-op otherwise — callers
// may invoke it on every poll.
func (s *Session) ProgramChanged(program string, kind Kind, at int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open != nil && s.open.Program == program && s.open.Kind == kind {
		return
	}
	if s.open != nil {
		s.open.End = at
		s.segments = append(s.segments, *s.open) // close it into the slice
	}
	s.open = &Segment{Program: program, Kind: kind, Start: at}
}

// Segments returns a copy of the segment list (open segment included, with its
// live lines/frames).
func (s *Session) Segments() []Segment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Segment, 0, len(s.segments)+1)
	out = append(out, s.segments...)
	if s.open != nil {
		out = append(out, *s.open)
	}
	return out
}

// ── queries: head / tail / grep over a session or one segment ───────

// Query asks for a slice of history. Empty fields walk the whole session (all
// segments, in order); SegmentIdx >= 0 targets one segment; Program filters to
// segments of that program. Grep is a case-insensitive substring; Context adds
// surrounding lines. Frames are rendered as their diffs regardless — that IS
// the history of a TUI.
type Query struct {
	SegmentIdx *int   // nil = all segments; else that segment's index
	Program    string // "" = any
	Head       int    // first N output lines
	Tail       int    // last N output lines (default when nothing set)
	Grep       string
	Context    int
	Limit      int // cap total rendered lines (0 → 400)
}

// renderSegment turns one segment into the lines a query sees.
func renderSegment(seg Segment) []string {
	switch seg.Kind {
	case Lines:
		return seg.Lines
	default:
		var out []string
		for i := range seg.Frames {
			out = append(out, fmt.Sprintf("── frame %d @ %s ──", i+1, time.Unix(0, seg.Frames[i].At).Format("15:04:05")))
			if d := seg.diff(i); d != "" {
				out = append(out, strings.Split(d, "\n")...)
			} else {
				out = append(out, "(no change)")
			}
		}
		return out
	}
}

// QueryHistory serves the query against the session.
func (s *Session) QueryHistory(q Query) string {
	s.mu.Lock()
	segs := make([]Segment, len(s.segments))
	copy(segs, s.segments)
	if s.open != nil {
		segs = append(segs, *s.open)
	}
	s.mu.Unlock()
	var sel []Segment
	for i, seg := range segs {
		if q.SegmentIdx != nil && i != *q.SegmentIdx {
			continue
		}
		if q.Program != "" && seg.Program != q.Program {
			continue
		}
		sel = append(sel, seg)
	}
	var lines []string
	for _, seg := range sel {
		r := renderSegment(seg)
		lines = append(lines, r...)
	}
	switch {
	case q.Grep != "":
		lines = grepLines(lines, q.Grep, q.Context)
	case q.Head > 0 && q.Tail > 0:
		lines = append(append([]string{}, lines[:minInt(q.Head, len(lines))]...), lines[maxInt(0, len(lines)-q.Tail):]...)
	case q.Head > 0:
		lines = lines[:minInt(q.Head, len(lines))]
	default:
		t := q.Tail
		if t <= 0 {
			t = 200
		}
		lines = lines[maxInt(0, len(lines)-t):]
	}
	return capJoin(lines, q.Limit)
}

// ── diff + line helpers (shared by the model and its renderers) ─────

// DiffFrames renders frame b vs frame a: shared leading/trailing context is
// elided; the middle shows b's lines with + markers. Cheap (no LCS) — TUI
// repaints touch a small region, so this reads well.
func DiffFrames(a, b string) string {
	al := strings.Split(strings.TrimRight(a, "\n"), "\n")
	bl := strings.Split(strings.TrimRight(b, "\n"), "\n")
	lo := 0
	for lo < len(al) && lo < len(bl) && al[lo] == bl[lo] {
		lo++
	}
	hiA, hiB := len(al), len(bl)
	for hiA > lo && hiB > lo && al[hiA-1] == bl[hiB-1] {
		hiA--
		hiB--
	}
	if lo >= len(bl) && hiA == len(al) {
		return ""
	}
	var out []string
	for _, ln := range bl[lo:hiB] {
		out = append(out, "+"+ln)
	}
	if len(out) > 60 {
		out = append([]string{fmt.Sprintf("… (%d changed lines)", len(out)-60)}, out[len(out)-60:]...)
	}
	return strings.Join(out, "\n")
}

// grepLines returns matches with context; overlapping contexts merge, and
// separate runs are joined by ⋮.
func grepLines(lines []string, pat string, ctx int) []string {
	pat = strings.ToLower(pat)
	mark := make([]bool, len(lines))
	for i, ln := range lines {
		if strings.Contains(strings.ToLower(ln), pat) {
			lo, hi := i-ctx, i+ctx
			if lo < 0 {
				lo = 0
			}
			if hi >= len(lines) {
				hi = len(lines) - 1
			}
			for j := lo; j <= hi; j++ {
				mark[j] = true
			}
		}
	}
	var out []string
	run := false
	for i, m := range mark {
		if m && !run {
			if len(out) > 0 {
				out = append(out, "⋮")
			}
			run = true
		} else if !m && run {
			run = false
		}
		if m {
			out = append(out, lines[i])
		}
	}
	return out
}

// capJoin caps a line slice (keeping the tail — most recent matters).
func capJoin(lines []string, limit int) string {
	if limit <= 0 {
		limit = 400
	}
	if len(lines) > limit {
		lines = append([]string{fmt.Sprintf("… (%d earlier lines elided)", len(lines)-limit)}, lines[len(lines)-limit:]...)
	}
	return strings.Join(lines, "\n")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
