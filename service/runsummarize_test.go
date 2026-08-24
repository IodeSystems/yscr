package service

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

type stubRunner struct{ reply string }

func (r *stubRunner) ChatStream(ctx context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	ch <- llm.StreamChunk{Content: r.reply, Done: true}
	close(ch)
	return ch, nil
}

func TestRunSummarize_Distills(t *testing.T) {
	s := &Server{runner: &stubRunner{reply: "All 42 tests passed."}}
	out, err := s.runSummarize(context.Background(), "go test ./...", strings.Repeat("ok line\n", 200))
	if err != nil {
		t.Fatal(err)
	}
	if out != "All 42 tests passed." {
		t.Fatalf("got %q", out)
	}
}

func TestRunSummarize_DashMeansNothing(t *testing.T) {
	s := &Server{runner: &stubRunner{reply: "-"}}
	out, err := s.runSummarize(context.Background(), "ls", "x\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("expected empty for dash, got %q", out)
	}
}

func TestRunSummarize_NoRunner(t *testing.T) {
	s := &Server{}
	if _, err := s.runSummarize(context.Background(), "ls", "x\n"); err == nil {
		t.Fatal("expected error with no runner")
	}
}
