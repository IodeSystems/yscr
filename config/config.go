// Package config loads the yscr service configuration: the concierge's LLM
// endpoint, which source plugins to register, and the web-push (VAPID) keys.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the yscr service configuration. Loaded from a JSON file
// (~/.yscr/config.json by default) with env overrides for secrets.
type Config struct {
	// Listen is the HTTP bind address (default ":8600").
	Listen string `json:"listen"`

	// Database is the Postgres DSN for durable state (concierge conversation +
	// push subscriptions). Empty → in-memory (ephemeral). env YSCR_DATABASE_URL.
	Database string `json:"database"`

	// LLM is the concierge's own endpoint (corrallm / OpenRouter).
	LLM LLMConfig `json:"llm"`

	// Autowork, when Enabled, registers the autowork source plugin.
	Autowork AutoworkConfig `json:"autowork"`

	// OpenAISessions enables the openai source (agentkit conversations on the
	// same LLM endpoint the concierge uses).
	OpenAISessions bool `json:"openai_sessions"`

	// ClaudeCode enables the tmux Claude Code source.
	ClaudeCode ClaudeCodeConfig `json:"claude_code"`

	// Audio is the STT/TTS backend (corrallm/oidio); defaults to the LLM
	// endpoint. Empty BaseURL disables the /api/audio/* proxy.
	Audio AudioConfig `json:"audio"`

	// VAPID holds the web-push keypair (auto-generated on first run if empty).
	VAPID VAPIDConfig `json:"vapid"`

	// Cue is the outbound task scheduler (see the cue package). OFF by default —
	// it's a personal, autonomous dispatcher that Posts/Spawns into live sessions,
	// so it stays disabled until explicitly turned on, and DryRun defaults ON so
	// the first enabled run only LOGS intended dispatches instead of acting.
	Cue CueConfig `json:"cue"`

	// path is where this config was loaded from (for saving generated keys).
	path string `json:"-"`
}

type LLMConfig struct {
	BaseURL string `json:"base_url"` // e.g. http://192.168.1.76:8111
	Model   string `json:"model"`    // e.g. qwen3-6-27b-mpt
	APIKey  string `json:"api_key"`  // env YSCR_LLM_KEY overrides
}

type AutoworkConfig struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url"` // e.g. http://127.0.0.1:8402
	Token   string `json:"token"`    // client bearer; env YSCR_AUTOWORK_TOKEN overrides
}

type ClaudeCodeConfig struct {
	Enabled bool     `json:"enabled"`
	Command []string `json:"command"` // default ["claude"]
	// TerminalPanes also registers the generic terminal adapter — it adopts every
	// non-claude, normal-screen (alt=0) tmux pane (shells, builds, log tails) into
	// the fleet. Off by default: it can flood the fleet with every open shell.
	TerminalPanes bool `json:"terminal_panes"`
}

type AudioConfig struct {
	BaseURL  string `json:"base_url"`  // corrallm/oidio; default = LLM.BaseURL
	APIKey   string `json:"api_key"`   // default = LLM.APIKey; env YSCR_AUDIO_KEY
	STTModel string `json:"stt_model"` // batch transcription model (e.g. parakeet)
	RTModel  string `json:"rt_model"`  // streaming STT model for /v1/realtime (e.g. realtime-stt)
	TTSModel string `json:"tts_model"` // speech model (e.g. kokoro)
	TTSVoice string `json:"tts_voice"` // voice id (backend default if empty)
	// DebugSave tees each transcription upload to DebugDir + exposes
	// GET /api/audio/debug[/{file}] for playback. Off by default — it persists
	// all captured mic audio. For diagnosing VAD cutoff vs recorder clipping.
	DebugSave bool   `json:"debug_save"`
	DebugDir  string `json:"debug_dir"` // default ~/.yscr/debug-audio
}

type VAPIDConfig struct {
	Public  string `json:"public"`
	Private string `json:"private"`
	Subject string `json:"subject"` // mailto: or https: contact for push services
}

// CueConfig tunes the outbound task scheduler. The caps map onto cue.Caps; the
// rails (Enabled/DryRun/MaxPerHour/MaxSpawns) bound autonomous action.
type CueConfig struct {
	Enabled     bool  `json:"enabled"`              // master switch (default off)
	DryRun      *bool `json:"dry_run"`              // log intended dispatches, don't act (default true)
	GenInterval int   `json:"gen_interval_seconds"` // generator tick cadence (default 120)

	PerSessionCap int `json:"per_session_cap"`         // cue.Caps.PerSession (default 1)
	GlobalCap     int `json:"global_cap"`              // cue.Caps.Global (0 = unlimited)
	MaxSpawns     int `json:"max_spawns"`              // cue.Caps.MaxSpawns (0 = unlimited)
	MaxPerHour    int `json:"max_per_hour"`            // hard rate cap on live dispatches (0 = unlimited)
	CompletionTTL int `json:"completion_ttl_seconds"` // reclaim an in-flight task after this long (default 1800)

	Goals []string `json:"goals"` // standing goals the generator plans against (phase 4)
}

// DryRunEnabled reports the effective dry-run flag (defaults true when unset, so
// enabling the cue without an explicit dry_run does NOT act live).
func (c CueConfig) DryRunEnabled() bool { return c.DryRun == nil || *c.DryRun }

// DefaultPath is ~/.yscr/config.json.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".yscr", "config.json")
}

// Load reads the config file (creating a minimal default if absent), applies
// env overrides, and fills defaults.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	c := &Config{path: path}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, err
		}
	}
	c.path = path

	if c.Listen == "" {
		c.Listen = ":8600"
	}
	if c.Database == "" {
		c.Database = "postgres://yscr:yscr@127.0.0.1:8001/yscr?sslmode=disable"
	}
	if v := os.Getenv("YSCR_DATABASE_URL"); v != "" {
		c.Database = v
	}
	if c.LLM.BaseURL == "" {
		c.LLM.BaseURL = "http://192.168.1.76:8111"
	}
	if c.LLM.Model == "" {
		// The corrallm "chat" lane: Qwen3-6-27B-MPT with fallback to gemma-4-12b
		// under contention (a bare model name would pin it, no fallback).
		c.LLM.Model = "chat"
	}
	if len(c.ClaudeCode.Command) == 0 {
		c.ClaudeCode.Command = []string{"claude"}
	}
	if c.VAPID.Subject == "" {
		c.VAPID.Subject = "mailto:yscr@localhost"
	}
	// Audio defaults to the LLM endpoint (corrallm serves both).
	if c.Audio.BaseURL == "" {
		c.Audio.BaseURL = c.LLM.BaseURL
	}
	if c.Audio.APIKey == "" {
		c.Audio.APIKey = c.LLM.APIKey
	}
	if c.Audio.STTModel == "" {
		c.Audio.STTModel = "stt"
	}
	if c.Audio.RTModel == "" {
		c.Audio.RTModel = "realtime-stt"
	}
	if c.Audio.TTSModel == "" {
		c.Audio.TTSModel = "tts"
	}
	// Cue scheduler defaults (safe: stays off; caps mirror cue's own defaults).
	if c.Cue.GenInterval <= 0 {
		c.Cue.GenInterval = 120
	}
	if c.Cue.PerSessionCap <= 0 {
		c.Cue.PerSessionCap = 1
	}
	if c.Cue.CompletionTTL <= 0 {
		c.Cue.CompletionTTL = 1800
	}
	// Secret env overrides.
	if v := os.Getenv("YSCR_LLM_KEY"); v != "" {
		c.LLM.APIKey = v
		if c.Audio.APIKey == "" {
			c.Audio.APIKey = v
		}
	}
	if v := os.Getenv("YSCR_AUDIO_KEY"); v != "" {
		c.Audio.APIKey = v
	}
	if v := os.Getenv("YSCR_AUTOWORK_TOKEN"); v != "" {
		c.Autowork.Token = v
	}
	return c, nil
}

// Save writes the config back (used to persist auto-generated VAPID keys).
func (c *Config) Save() error {
	if c.path == "" {
		c.path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, b, 0o600)
}
