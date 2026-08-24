# Subplan — ops: supervision, durability, auth

> Production posture for the daemon. Dev auto-reload (`.air.toml` in `yscr-air`
> tmux) is not supervision.

## Requirements
1. **systemd unit** for yscr — reboot-persistence; replaces the tmux air
   session as the running mode (air stays for dev).
2. ✅ **Durable session registries** — openai/claude-code forget their sessions
   across restart. The claude index (`~/.claude/sessions/*.json`) is on disk;
   wire List back from it on start so adoption self-heals after a reboot.
3. **Optional auth** — LAN-only today, deferred; if the service ever leaves the
   VPN zone, bearer-token like autowork3's client-token.

## Status
◐ in progress — #1 + #2 done; #3 (optional auth) not started.

### ✅ Durable session registries
- **openai plugin** (`plugins/openai`): `RestoreFromStore(ctx, st)` rebuilds the
  in-memory registry from a durable store — for each persisted session log it
  recovers the title (first user message), last reply as summary, idle status.
  `service.New` now passes the PG store (not Mem) when Postgres is available,
  and calls `RestoreFromStore` after building sources. `store.PG.SessionIDs`
  lists distinct session ids from the entries table. Post to a restored session
  re-enters through `turn()` and appends to the same persisted log.
- **claude plugin**: already durable — `Discover` reads `~/.claude/sessions/
  *.json` on every List call, so adoption self-heals after a reboot with no
  extra wiring. Verified live: kill + restart → fleet re-lists both claude
  sessions + the openai primary (conversation recall intact).

### ✅ systemd unit (user scope)
- `~/.config/systemd/user/yscr.service`: Type=simple, ExecStart at the built
  binary (`tmp/yscr -config ~/.yscr/config.json -listen 127.0.0.1:8600`),
  Restart=on-failure, WantedBy=default.target. Enabled + started; verified
  live (health + fleet serve under systemd). User scope because the dev box
  doesn't grant root for system units; `loginctl enable-linger nthalk` keeps
  it running after logout if desired. The `.air.toml` tmux session stays for
  development hot-reload.

### ◻ Optional auth
- Bearer-token middleware (like autowork3's client-token) gated by a config
  flag; off by default for LAN-only use.
