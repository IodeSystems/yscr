# Subplan — ops: supervision, durability, auth

> Production posture for the daemon. Dev auto-reload (`.air.toml` in `yscr-air`
> tmux) is not supervision.

## Requirements
1. **systemd unit** for yscr — reboot-persistence; replaces the tmux air
   session as the running mode (air stays for dev).
2. ✅ **Durable session registries** — the claude adapter re-derives its sessions from the on-disk index (`~/.claude/sessions/*.json`) on start, so a restart re-lists prior sessions.
   across restart. The claude index (`~/.claude/sessions/*.json`) is on disk;
   wire List back from it on start so adoption self-heals after a reboot.
3. ✅ **Optional auth** — bearer-token middleware, off by default.

## Status
✅ done — all three requirements landed.

### ✅ Durable session registries
- **claude adapter**: `Discover` reads `~/.claude/sessions/*.json` on every List
  call, so adoption self-heals after a reboot with no extra wiring. Verified
  live: kill + restart → session list re-derives the claude sessions.
- (The openai plugin that needed an explicit `RestoreFromStore` was later
  removed — the concierge is just an agentkit session on corrallm.)

### ✅ systemd unit (user scope)
- `~/.config/systemd/user/yscr.service`: Type=simple, ExecStart at the built
  binary (`tmp/yscr -config ~/.yscr/config.json -listen 127.0.0.1:8600`),
  Restart=on-failure, WantedBy=default.target. Enabled + started; verified
  live (health + session API serve under systemd). User scope because the dev box
  doesn't grant root for system units; `loginctl enable-linger nthalk` keeps
  it running after logout if desired. The `.air.toml` tmux session stays for
  development hot-reload.

### ✅ Optional auth
- `config.Auth.Token` (env `YSCR_AUTH_TOKEN` overrides). When set, every `/api/*`
  route requires `Authorization: Bearer <token>` — constant-time compare,
  `WWW-Authenticate` on 401. The PWA shell + static assets stay open (no state
  behind them); the data routes are what's protected. Off by default for
  LAN-only use. `service/auth.go` + tests (off / no-token / wrong-token /
  good-token / shell-open).
