// Command yscr runs the fleet-concierge service: the concierge + source
// plugins + the embedded PWA (with Web Push background notifications).
//
// Subcommands:
//
//	yscr            run the service (default)
//	yscr panes      analyze: list every live Claude tmux pane yscr would adopt
//
// Config: ~/.yscr/config.json (override with -config). Secrets via env:
// YSCR_LLM_KEY, YSCR_AUTOWORK_TOKEN. On first run a VAPID keypair is
// generated + saved so push notifications work.
//
// Note: browser Push + service workers require a secure context — serve over
// HTTPS, or use http://localhost during development.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/iodesystems/yscr/config"
	"github.com/iodesystems/yscr/plugins/claudecode"
	"github.com/iodesystems/yscr/service"
)

func main() {
	// Subcommand dispatch (first non-flag arg). Default: run the service.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "panes":
			runPanes(os.Args[2:])
			return
		case "hook-question": // PreToolUse hook body (reads stdin)
			runHookQuestion()
			return
		case "install-hook": // merge the AskUserQuestion hook into ~/.claude/settings.json
			runInstallHook()
			return
		}
	}
	runServe(os.Args[1:])
}

func runServe(argv []string) {
	fs := flag.NewFlagSet("yscr", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path (default ~/.yscr/config.json)")
	listen := fs.String("listen", "", "override the listen address")
	_ = fs.Parse(argv)

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

// runPanes prints the pane↔session join — every live Claude session mapped to
// the tmux pane hosting it (the panes yscr adopts and can drive). No daemon.
func runPanes(argv []string) {
	fs := flag.NewFlagSet("yscr panes", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path (default ~/.yscr/config.json)")
	_ = fs.Parse(argv)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("yscr panes: load config: %v", err)
	}
	cc := claudecode.New(claudecode.Config{Command: cfg.ClaudeCode.Command})
	panes := cc.Panes(context.Background())
	if len(panes) == 0 {
		fmt.Println("no live Claude sessions found")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SID\tPANE\tSTATUS\tNAME\tCWD")
	for _, p := range panes {
		pane := p.Pane
		if pane == "" {
			pane = "—" // alive, but not inside tmux
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", short(p.SID), pane, p.Status, p.Name, p.Cwd)
	}
	_ = w.Flush()
}

// short trims a UUID to its first segment for a compact table.
func short(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}
