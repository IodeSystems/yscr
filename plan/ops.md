# Subplan — ops: supervision, durability, auth

> Production posture for the daemon. Dev auto-reload (`.air.toml` in `yscr-air`
> tmux) is not supervision.

## Requirements
1. **systemd unit** for yscr — reboot-persistence; replaces the tmux air
   session as the running mode (air stays for dev).
2. **Durable session registries** — openai/claude-code forget their sessions
   across restart. The claude index (`~/.claude/sessions/*.json`) is on disk;
   wire List back from it on start so adoption self-heals after a reboot.
3. **Optional auth** — LAN-only today, deferred; if the service ever leaves the
   VPN zone, bearer-token like autowork3's client-token.

## Status
◻ todo — not started.
