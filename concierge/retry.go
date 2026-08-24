package concierge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// The worker's turn loop retries at TWO levels, and a caller cannot be expected
// to care which:
//
//   - request scope — inside llm.Client, before any token is produced: a 429, a
//     5xx, a gateway that is not answering. The client handles these itself; the
//     per-attempt ctx (turnTimeout) bounds how long one attempt may spend there.
//   - turn scope — this file: re-running the WHOLE turn after a fault the client
//     cannot retry, chiefly a stream that DIED MID-GENERATION or an attempt whose
//     1h budget ran out inside corrallm's queue (a slots:1 lane holds for minutes
//     while another session generates). The conversation is in the store, so a
//     retry rebuilds context from it and completed tool calls are not re-run.
//
// Why this exists at all: before it, a single 429-queue-timeout that outlived
// the attempt's ctx killed the turn for good — corrallm logged a 499, agentkit
// refused to retry past a cancelled ctx (correctly: the caller said stop), and
// the concierge reported failure to a user who had done nothing wrong. The
// attempt deadline is now a per-ATTEMPT bound; this budget is the patience.

const (
	// turnRetryBudget caps total wall-clock spent retrying ONE utterance, across
	// attempts and their backoffs. Generous on purpose: the observed longest
	// legitimate request on this box was ~78 minutes of generation, and a queued
	// caller behind it can wait longer still.
	turnRetryBudget = 2 * time.Hour

	// turnRetryMaxAttempts bounds the attempt COUNT so a flapping provider that
	// fails fast cannot spin the budget in milliseconds. The budget is the real
	// guard; this one stops the degenerate case.
	turnRetryMaxAttempts = 30

	turnRetryInitialBackoff = 2 * time.Second
	turnRetryMaxBackoff     = 30 * time.Second
)

// runTurnWithRetry runs one (possibly merged) user message, retrying the whole
// turn while the PROVIDER is the thing that failed. Only ever called by a
// session's worker goroutine — never concurrently for the same session.
func (c *Concierge) runTurnWithRetry(ctx context.Context, sessionID, userMessage string) (string, error) {
	backoff := turnRetryInitialBackoff
	start := time.Now()
	deadline := start.Add(turnRetryBudget)
	for attempt := 1; ; attempt++ {
		res, err := c.runAttempt(ctx, sessionID, userMessage)
		if err == nil {
			return res, nil
		}
		// A cancelled parent ctx is the CALLER (or process shutdown) saying stop.
		// Checked explicitly because a cancelled request arrives wrapped in
		// *url.Error, which satisfies net.Error and so LOOKS transient.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		if attempt >= turnRetryMaxAttempts || !llm.TransientUpstream(err) {
			return "", fmt.Errorf("concierge: turn failed after %d attempt(s): %w", attempt, err)
		}
		if time.Now().Add(backoff).After(deadline) {
			log.Printf("concierge: gave up after %s and %d attempts: %v",
				time.Since(start).Round(time.Second), attempt, err)
			return "", fmt.Errorf("concierge: retry budget %s exhausted after %s (%d attempts): %w",
				turnRetryBudget, time.Since(start).Round(time.Second), attempt, err)
		}
		log.Printf("concierge: turn interrupted — %s; retrying in %s (attempt %d)",
			llm.TransientUpstreamReason(err), backoff, attempt)
		if !sleepOrCancel(ctx, backoff) {
			return "", ctx.Err()
		}
		backoff *= 2
		if backoff > turnRetryMaxBackoff {
			backoff = turnRetryMaxBackoff
		}
	}
}

// runAttempt runs ONE attempt: a fresh per-attempt context (turnTimeout) under
// the caller's, one user message injected, one agent turn. The attempt ctx is
// what corrallm's queue patience and a long generation are measured against;
// it must NOT be shared across attempts, or the first attempt's spent time eats
// the second's budget.
func (c *Concierge) runAttempt(ctx context.Context, sessionID, userMessage string) (string, error) {
	atx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	sess := c.session(sessionID)
	if err := sess.Inject(atx, agent.Entry{Kind: agent.KindUser, Content: userMessage}); err != nil {
		return "", err
	}
	res, err := sess.Turn(atx)
	if err != nil {
		return "", err
	}
	return res.Reply, nil
}

// sleepOrCancel waits d, or returns false as soon as ctx ends. A var so a test
// can exercise the loop without sitting out the real schedule; production never
// replaces it.
var sleepOrCancel = func(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
