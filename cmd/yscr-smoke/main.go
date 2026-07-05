// Command yscr-smoke drives the openai source plugin against a live
// OpenAI-spec endpoint (corrallm / OpenRouter) — an end-to-end "via corrallm"
// check. It spawns one conversation and prints the model's reply.
//
// Usage:
//
//	YSCR_BASE=http://192.168.1.76:8111 \
//	YSCR_KEY=$CORRALLM_KEY \
//	YSCR_MODEL=qwen3-6-27b-mpt \
//	go run ./cmd/yscr-smoke "say hello in five words"
//
// Defaults target corrallm-local. The base URL is the endpoint root (the llm
// client appends /v1/chat/completions).
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/yscr/plugins/openai"
	"github.com/iodesystems/yscr/source"
	"github.com/iodesystems/yscr/store"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	base := env("YSCR_BASE", "http://192.168.1.76:8111")
	model := env("YSCR_MODEL", "qwen3-6-27b-mpt")
	key := os.Getenv("YSCR_KEY")

	prompt := "Say hello in five words."
	if len(os.Args) > 1 {
		prompt = strings.Join(os.Args[1:], " ")
	}

	fmt.Fprintf(os.Stderr, "→ %s (%s)\n", base, model)

	runner := llm.NewClient(base, key, model)
	p := openai.New(runner, store.NewMem(), "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref, err := p.Spawn(ctx, source.SpawnSpec{Title: "smoke", Prompt: prompt})
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn failed: %v\n", err)
		os.Exit(1)
	}
	st, err := p.State(ctx, ref.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "state failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(st.Summary)
}
