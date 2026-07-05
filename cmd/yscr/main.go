// Command yscr runs the fleet-concierge service: the concierge + source
// plugins + the embedded PWA (with Web Push background notifications).
//
// Config: ~/.yscr/config.json (override with -config). Secrets via env:
// YSCR_LLM_KEY, YSCR_AUTOWORK_TOKEN. On first run a VAPID keypair is
// generated + saved so push notifications work.
//
// Note: browser Push + service workers require a secure context — serve over
// HTTPS, or use http://localhost during development.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/iodesystems/yscr/config"
	"github.com/iodesystems/yscr/service"
)

func main() {
	cfgPath := flag.String("config", "", "config file path (default ~/.yscr/config.json)")
	listen := flag.String("listen", "", "override the listen address")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("yscr: load config: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	srv, err := service.New(cfg)
	if err != nil {
		log.Fatalf("yscr: build service: %v", err)
	}

	srv.Start() // fleet watcher → SSE + web push

	log.Printf("yscr listening on %s — PWA + concierge (llm=%s, autowork=%v, openai=%v, claude-code=%v)",
		cfg.Listen, cfg.LLM.BaseURL, cfg.Autowork.Enabled, cfg.OpenAISessions, cfg.ClaudeCode.Enabled)
	if err := http.ListenAndServe(cfg.Listen, srv.Handler()); err != nil {
		log.Fatalf("yscr: serve: %v", err)
	}
}
