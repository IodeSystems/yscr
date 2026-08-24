package concierge

import (
	"context"
	"strings"
	"time"
)

// Per-session serialized dispatch with coalescing.
//
// Turns for one session must not run concurrently: the agent.Store isn't
// concurrency-safe and interleaved turns corrupt the conversation. So each
// session gets one worker goroutine that runs turns strictly in order.
//
// Coalescing gives the "append new work, re-evaluate" behavior: while a turn is
// processing, incoming messages buffer in the session channel; when the worker
// finishes, it drains ALL of them and merges them into a SINGLE follow-up turn
// (rather than one racy turn each). Every caller merged into a turn receives that
// turn's reply. Messages that arrive after a turn has already STARTED are handled
// in the next turn — matching "interrupted before processing" semantics.

// turnTimeout caps ONE ATTEMPT of a turn — the wall-clock a single request may
// spend in corrallm's queue plus its generation — so a wedged LLM/tool call
// can't block a session's queue forever. It is NOT the patience for the whole
// utterance: runTurnWithRetry (retry.go) re-runs the turn on a fresh attempt
// ctx while the provider keeps failing, bounded by turnRetryBudget.
//
// 1h covers the observed longest legitimate request on this box (~78 minutes of
// generation on local-Qwen3.8-27B, 2026-08-19) with headroom for queue time.
const turnTimeout = 1 * time.Hour

type convReq struct {
	msg    string
	medium string // "" | "text" | "speech" — the channel this turn is heard on
	done   chan convRes
}

type convRes struct {
	reply string
	err   error
}

type sessQueue struct {
	ch chan convReq
}

// queue returns the session's dispatcher, lazily starting its worker goroutine.
func (c *Concierge) queue(sessionID string) *sessQueue {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	if c.queues == nil {
		c.queues = map[string]*sessQueue{}
	}
	q := c.queues[sessionID]
	if q == nil {
		q = &sessQueue{ch: make(chan convReq, 64)}
		c.queues[sessionID] = q
		go c.worker(sessionID, q)
	}
	return q
}

// worker serializes turns for one session, coalescing everything queued at the
// start of each turn into one merged turn.
func (c *Concierge) worker(sessionID string, q *sessQueue) {
	for first := range q.ch {
		batch := []convReq{first}
		// Drain anything already queued so successive utterances (and anything that
		// piled up during the previous turn) re-evaluate together.
	drain:
		for {
			select {
			case more := <-q.ch:
				batch = append(batch, more)
			default:
				break drain
			}
		}

		msgs := make([]string, len(batch))
		for i, r := range batch {
			msgs[i] = r.msg
		}
		// The medium the LAST caller is on wins: if any of the coalesced utterances
		// will be heard by ear, the reply must be speakable.
		medium := ""
		for _, r := range batch {
			if r.medium != "" {
				medium = r.medium
			}
		}

		// Background context, not any caller's: a coalesced turn serves several
		// callers, so no single request's cancellation should abort it. The
		// per-ATTEMPT bound is inside runTurnWithRetry (turnTimeout); this one
		// just keeps a wedged session from holding its queue forever if the
		// retry budget itself misbehaves.
		ctx, cancel := context.WithTimeout(context.Background(), turnRetryBudget+turnTimeout)
		reply, err := c.runTurnWithRetry(ctx, sessionID, mediumHint(medium)+strings.Join(msgs, "\n"))
		cancel()

		for _, r := range batch {
			r.done <- convRes{reply: reply, err: err} // done is buffered(1); never blocks
		}
	}
}

// mediumHint prefixes the merged user message with a channel note when the turn
// will be heard by ear. It rides the message (not the system prompt) so it
// coalesces naturally and costs nothing on text-only turns.
func mediumHint(medium string) string {
	if medium == "speech" {
		return "[heard by voice — reply in at most two short sentences; no lists, code, or ids]\n"
	}
	return ""
}
