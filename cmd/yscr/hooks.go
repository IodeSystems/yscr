package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/iodesystems/yscr/plugins/pane/claude"
)

// runHookQuestion is the `yscr hook-question` PreToolUse hook body: it reads the
// AskUserQuestion payload from stdin and drops it, keyed by session_id, into the
// pending dir where the claude-code plugin reads it. It must never block the
// tool — any problem just means no structured question (the pane fallback still
// works), so it always exits 0.
func runHookQuestion() {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	var pl struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(b, &pl) != nil || pl.SessionID == "" {
		return
	}
	dir := claude.DefaultPendingDir()
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, pl.SessionID+".json"), b, 0o644)
}

// runInstallHook merges the PreToolUse/AskUserQuestion hook into
// ~/.claude/settings.json (idempotent; backs up first).
func runInstallHook() {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".claude", "settings.json")

	settings := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(b, &settings) != nil {
			log.Fatalf("yscr install-hook: %s is not valid JSON; not touching it", path)
		}
		if err := os.WriteFile(path+".bak", b, 0o644); err != nil {
			log.Fatalf("yscr install-hook: backup failed: %v", err)
		}
	}

	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "yscr"
	}
	command := exe + " hook-question"

	if !installHook(settings, command) {
		fmt.Printf("hook already installed: %s\n", command)
		return
	}
	out, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Fatalf("yscr install-hook: %v", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		log.Fatalf("yscr install-hook: write %s: %v", path, err)
	}
	fmt.Printf("installed PreToolUse:AskUserQuestion → %s\n(backup: %s.bak)\n", command, path)
}

// installHook ensures settings has a PreToolUse hook for AskUserQuestion running
// command. Returns whether it changed settings (false = already present). Pure,
// so it's unit-testable.
func installHook(settings map[string]any, command string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	pre, _ := hooks["PreToolUse"].([]any)
	newHook := map[string]any{"type": "command", "command": command}

	for _, e := range pre {
		m, _ := e.(map[string]any)
		if m == nil || m["matcher"] != "AskUserQuestion" {
			continue
		}
		hs, _ := m["hooks"].([]any)
		for _, h := range hs {
			if hm, _ := h.(map[string]any); hm != nil && hm["command"] == command {
				return false // already present
			}
		}
		m["hooks"] = append(hs, newHook) // add to the existing matcher
		return true
	}
	hooks["PreToolUse"] = append(pre, map[string]any{
		"matcher": "AskUserQuestion",
		"hooks":   []any{newHook},
	})
	return true
}
