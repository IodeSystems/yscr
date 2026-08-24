// runsummarize.go — LLM-distilled completion summary for long command outputs
// (the scratchpad polish item). The concierge's run_command reports the raw
// tail for short output; past a threshold this distills it into a few lines of
// what happened, keeping the tail underneath. Best-effort: any error falls back
// to the raw tail in the concierge.
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

const runSummarizeSystem = `You summarize the output of a terminal command that just finished, for a busy user who asked the concierge to run it. In 1-3 short sentences: what happened (success/failure), and the one or two facts that matter (counts, errors, key results). Do not recite the output. If it failed, say what failed and the first useful error line. Reply "-" if nothing is worth saying.`

// newRunSummarizer returns the run_command completion summarizer, or nil when
// there's no LLM runner (short outputs are fine without one).
func (s *Server) runSummarize(ctx context.Context, command, output string) (string, error) {
	if s.runner == nil {
		return "", fmt.Errorf("no llm runner")
	}
	out := strings.TrimSpace(output)
	if len(out) > 6000 {
		out = out[len(out)-6000:]
	}
	ctx2, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ch, err := s.runner.ChatStream(ctx2, []llm.Message{
		{Role: "system", Content: runSummarizeSystem},
		{Role: "user", Content: fmt.Sprintf("Command: %s\n\nOutput:\n%s", command, out)},
	}, nil, nil)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for chunk := range ch {
		if chunk.Error != "" {
			return "", fmt.Errorf("%s", chunk.Error)
		}
		b.WriteString(chunk.Content)
		if chunk.Done {
			break
		}
	}
	s2 := strings.TrimSpace(b.String())
	if s2 == "-" || s2 == "" {
		return "", nil
	}
	return s2, nil
}

