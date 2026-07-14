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

// turnTimeout caps a single turn so a wedged LLM/tool call can't block a
// session's queue forever. Generous: turns may fan out to source tools.
const turnTimeout = 5 * time.Minute

type convReq struct {
	msg  string
	done chan convRes
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

		// Background context, not any caller's: a coalesced turn serves several
		// callers, so no single request's cancellation should abort it. Bounded so a
		// stuck turn can't wedge the session.
		ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
		reply, err := c.runTurn(ctx, sessionID, strings.Join(msgs, "\n"))
		cancel()

		for _, r := range batch {
			r.done <- convRes{reply: reply, err: err} // done is buffered(1); never blocks
		}
	}
}
