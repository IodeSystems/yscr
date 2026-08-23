// Ambient auto-narration — the materiality-gated voice channel (audio subplan
// #2 + #3).
//
// The per-session Narrate toggle re-LLMs every tick unconditionally. Ambient
// narration runs for EVERY session, so it needs a two-stage gate ported from
// autowork3's yscr_status.go:
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
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/source"
)

const defaultAmbientInterval = 8 * time.Second // one L1 distill per session per tick

// ambientCell is one session's narration pipeline state (L1 rev + L2 gate),
// the in-memory half of autowork3's yscrProgress. Ephemeral: a restart just
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
}

func newAmbientHub() *ambientHub {
	return &ambientHub{
		active:   map[string]context.CancelFunc{},
		cells:    map[string]*ambientCell{},
		interval: defaultAmbientInterval,
	}
}

func (h *ambientHub) start(key string, cancel context.CancelFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.active[key]; !ok {
		h.active[key] = cancel
	}
	if _, ok := h.cells[key]; !ok {
		h.cells[key] = &ambientCell{}
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
		// L2 gate: speak only on a real advance, one utterance in flight.
		speak := changed && !c.nudged && c.distillRev > c.spokenRev
		if speak {
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
