package main

import "testing"

func TestInstallHook_Empty(t *testing.T) {
	s := map[string]any{}
	if !installHook(s, "yscr hook-question") {
		t.Fatal("should report changed on empty settings")
	}
	pre := s["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 1 {
		t.Fatalf("PreToolUse = %v", pre)
	}
	m := pre[0].(map[string]any)
	if m["matcher"] != "AskUserQuestion" {
		t.Errorf("matcher = %v", m["matcher"])
	}
	h := m["hooks"].([]any)[0].(map[string]any)
	if h["command"] != "yscr hook-question" || h["type"] != "command" {
		t.Errorf("hook = %v", h)
	}
}

func TestInstallHook_Idempotent(t *testing.T) {
	s := map[string]any{}
	installHook(s, "yscr hook-question")
	if installHook(s, "yscr hook-question") {
		t.Error("second install should be a no-op")
	}
}

// Preserves an unrelated existing PreToolUse hook and appends to the matcher.
func TestInstallHook_PreservesExisting(t *testing.T) {
	s := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "lint"}}},
			},
		},
	}
	if !installHook(s, "yscr hook-question") {
		t.Fatal("should add AskUserQuestion matcher")
	}
	pre := s["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 2 { // Bash preserved + AskUserQuestion added
		t.Fatalf("PreToolUse = %v", pre)
	}
}
