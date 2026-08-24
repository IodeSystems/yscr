// Ambient auto-narration — the materiality-gated voice channel (audio subplan
// #2 + #3).
//
// The per-session Narrate toggle re-LLMs every tick unconditionally. Ambient
// narration runs for EVERY session, so it needs a two-stage gate ported from
// the materiality model ported from an earlier design (L1 distill / L2 gate):
//
//	L1 distill — fold the raw output buffer into a one-line world-model
//	    snapshot (deterministic: the last meaningful line). The snapshot only
//	    advances when it CHANGES, so an unchanged fold never bumps the rev.
//	L2 materiality gate — speak only when the world-model advanced since the
//	    last spoken line AND nothing is already in flight; a no-op distill
//	    must not fire the gate (that's how the drone dies).
//
// The LLM then phrases the advance as one spoken sentence. Delivery: SSE
// "narration" events (same shape as per-session narrate, so the PWA renders
// them identically) + a web-push Notify for when the phone isn't looking.
//
// Ships OFF (ambient.enabled), mirroring the cue's safe-defaults posture.
package service

import (
	"strconv"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/config"
	"github.com/iodesystems/yscr/source"
)

const defaultAmbientInterval = 8 * time.Second // one L1 distill per session per tick

// ambientCell is one session's narration pipeline state (L1 rev + L2 gate),
// The in-memory progress state per session. Ephemeral: a restart just
// re-distills, which is cheap and harmless.
type ambientCell struct {
	mu           sync.Mutex
	snapshot     string // L1 world-model (last meaningful line)
	spokenRev    int    // L2: rev last spoken
	distillRev   int    // advances only when the snapshot changed
	nudged       bool   // an utterance is in flight
	lastSpokenAt time.Time
}

// ambientHub tracks active ambient narrations (source/id → cancel).
type ambientHub struct {
	mu       sync.Mutex
	active   map[string]context.CancelFunc
	cells    map[string]*ambientCell
	interval time.Duration // distill cadence; overridable in tests
	minSpeak time.Duration // L2: min gap between one session's utterances (default 60s)
	quiet    func() bool   // quiet-hours gate (nil = never quiet); checked per tick
	now      func() time.Time
}

func newAmbientHub(cfg config.AmbientConfig) *ambientHub {
	minSpeak := 60 * time.Second
	if cfg.MinIntervalSeconds > 0 {
		minSpeak = time.Duration(cfg.MinIntervalSeconds) * time.Second
	}
	return &ambientHub{
		active:   map[string]context.CancelFunc{},
		cells:    map[string]*ambientCell{},
		interval: defaultAmbientInterval,
		minSpeak: minSpeak,
		quiet:    quietHours(cfg.QuietStart, cfg.QuietEnd),
		now:      time.Now,
	}
}

// quietHours parses "HH:MM" bounds into a gate. Empty/invalid bounds → never
// quiet. start > end wraps midnight (22:00–07:00 is quiet 10pm→7am).
func quietHours(start, end string) func() bool {
	parse := func(s string) (int, int, bool) {
		parts := strings.Split(s, ":")
		if len(parts) != 2 {
			return 0, 0, false
		}
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, 0, false
		}
		return h, m, true
	}
	sh, sm, ok1 := parse(start)
	eH, em, ok2 := parse(end)
	if !ok1 || !ok2 {
		return func() bool { return false }
	}
	sMin, eMin := sh*60+sm, eH*60+em
	if sMin == eMin {
		return func() bool { return false }
	}
	return func() bool {
		t := time.Now()
		now := t.Hour()*60 + t.Minute()
		if sMin < eMin {
			return now >= sMin && now < eMin
		}
		return now >= sMin || now < eMin // wraps midnight
	}
}

func (h *ambientHub) start(key string, cancel context.CancelFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.active[key]; !ok {
		h.active[key] = cancel
	}
	c, ok := h.cells[key]
	if !ok {
		c = &ambientCell{lastSpokenAt: h.now().Add(-h.minSpeak)} // first advance may speak immediately
		h.cells[key] = c
	} else if h.minSpeak > 0 && time.Since(c.lastSpokenAt) < h.minSpeak {
		// re-started inside the min gap (a stream closed and reopened): backdate
		// so a pending advance isn't held by the pre-restart clock
		c.lastSpokenAt = h.now().Add(-h.minSpeak)
	}
}

func (h *ambientHub) stop(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cancel, ok := h.active[key]; ok {
		cancel()
		delete(h.active, key)
	}
	delete(h.cells, key)
}

func (h *ambientHub) cell(key string) *ambientCell {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.cells[key]
	if !ok {
		c = &ambientCell{}
		h.cells[key] = c
	}
	return c
}

// ambientLoop tails one session's output and runs the L1→L2 pipeline. It ends
// when the session leaves the fleet (the watcher stops it) or its stream closes.
func (s *Server) ambientLoop(ctx context.Context, key, srcID, id, title string, ch <-chan source.Event) {
	c := s.ambient.cell(key)
	ticker := time.NewTicker(s.ambient.interval)
	defer ticker.Stop()

	var buf []string
	distill := func() {
		if len(buf) == 0 {
			return
		}
		delta := strings.Join(buf, "\n")
		buf = buf[:0]
		snapshot := l1Distill(delta)
		if snapshot == "" {
			return // nothing meaningful in this delta — don't touch the rev
		}
		c.mu.Lock()
		changed := snapshot != c.snapshot
		if changed {
			c.snapshot = snapshot
			c.distillRev++
		}
		// L2 gate: speak only on a real advance, one utterance in flight,
		// outside quiet hours, and at least minSpeak since the last utterance.
		speak := changed && !c.nudged && c.distillRev > c.spokenRev
		if speak {
			quietNow := s.ambient.quiet != nil && s.ambient.quiet()
			tooSoon := time.Since(c.lastSpokenAt) < s.ambient.minSpeak
			if quietNow || tooSoon {
				c.mu.Unlock()
				return // the advance is still recorded (snapshot/rev moved on);
				// it will be spoken at the next qualifying tick
			}
			c.nudged = true
		}
		c.mu.Unlock()
		if !speak {
			return
		}
		text := s.ambientLine(ctx, title, snapshot)
		c.mu.Lock()
		c.nudged = false
		c.spokenRev = c.distillRev
		c.lastSpokenAt = time.Now()
		c.mu.Unlock()
		if text == "" {
			return
		}
		s.sse.broadcast(sseMsg{event: "narration", data: mustJSON(map[string]string{
			"source": srcID, "id": id, "title": title, "text": text,
		})})
		s.Notify(title, text)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				distill()
				return
			}
			buf = append(buf, ev.Content)
		case <-ticker.C:
			distill()
		}
	}
}

// l1Distill is the deterministic L1 fold: the last meaningful line of a raw
// output delta. It collapses a burst to one world-model snapshot without an
// LLM call; "meaningful" = not prompt noise, redraws, or pure whitespace.
func l1Distill(delta string) string {
	lines := strings.Split(strings.TrimSpace(delta), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || isNoise(l) {
			continue
		}
		if len(l) > 200 {
			l = l[len(l)-200:]
		}
		return l
	}
	return ""
}

// isNoise rejects the lines that carry no world-model content: shell prompts,
// bare keystroke echoes, ANSI-free redraw artifacts.
func isNoise(l string) bool {
	if strings.HasSuffix(l, "$") || strings.HasSuffix(l, "#") || strings.HasSuffix(l, "%") {
		return true
	}
	if l == "y" || l == "n" || l == "q" || l == ":" || l == "." {
		return true
	}
	return false
}

// ambientLine phrases a world-model advance as one spoken sentence (L2
// utterance). Same posture as per-session narrate: magnitude, not recitation.
func (s *Server) ambientLine(ctx context.Context, title, snapshot string) string {
	if s.narr == nil || s.narr.runner == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, narrateTimeout)
	defer cancel()
	ch, err := s.narr.runner.ChatStream(ctx, []llm.Message{
		{Role: "system", Content: ambientSystem},
		{Role: "user", Content: fmt.Sprintf("Session: %s\nCurrent state: %s", title, snapshot)},
	}, nil, nil)
	if err != nil {
		return ""
	}
	var out strings.Builder
	for chunk := range ch {
		if chunk.Error != "" {
			return ""
		}
		out.WriteString(chunk.Content)
		if chunk.Done {
			break
		}
	}
	out2 := strings.TrimSpace(out.String())
	if out2 == "-" || out2 == "" {
		return ""
	}
	return out2
}

const ambientSystem = `You are a fleet concierge narrating a work session ALOUD to someone who is not looking at the screen. You are given the session's current state — one line distilled from its recent output.

Say what just became true in ONE natural spoken sentence, present tense, at most 20 words — convey magnitude without reciting details; never a list, never quoted output. If the state says nothing worth saying aloud, reply with exactly "-" and nothing else.`
