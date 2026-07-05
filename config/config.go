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

	// LLM is the concierge's own endpoint (corrallm / OpenRouter).
	LLM LLMConfig `json:"llm"`

	// Autowork, when Enabled, registers the autowork source plugin.
	Autowork AutoworkConfig `json:"autowork"`

	// OpenAISessions enables the openai source (agentkit conversations on the
	// same LLM endpoint the concierge uses).
	OpenAISessions bool `json:"openai_sessions"`

	// ClaudeCode enables the tmux Claude Code source.
	ClaudeCode ClaudeCodeConfig `json:"claude_code"`

	// VAPID holds the web-push keypair (auto-generated on first run if empty).
	VAPID VAPIDConfig `json:"vapid"`

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
}

type VAPIDConfig struct {
	Public  string `json:"public"`
	Private string `json:"private"`
	Subject string `json:"subject"` // mailto: or https: contact for push services
}

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
	if c.LLM.BaseURL == "" {
		c.LLM.BaseURL = "http://192.168.1.76:8111"
	}
	if c.LLM.Model == "" {
		c.LLM.Model = "qwen3-6-27b-mpt"
	}
	if len(c.ClaudeCode.Command) == 0 {
		c.ClaudeCode.Command = []string{"claude"}
	}
	if c.VAPID.Subject == "" {
		c.VAPID.Subject = "mailto:yscr@localhost"
	}
	// Secret env overrides.
	if v := os.Getenv("YSCR_LLM_KEY"); v != "" {
		c.LLM.APIKey = v
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
