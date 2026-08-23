# Subplan — P3 cutover (autowork3 → yscr)

> Delete the in-process YSCR from autowork3; yscr becomes the only concierge.

## Requirements
1. Delete from autowork3: `yscr.go`, `yscr_status.go`, the `notifyYSCR` hook
   (api.go), the `yscr` role + tools + prompt, `sessions.yscr_summary` column.
2. Split the shared membrane cores (`concierge.go`, `confirm_send`) — membrane
   logic → yscr; send-execution stays in autowork3.
3. Repoint the Android client (`com.iodesystems.yscr`) at the yscr service.
4. Fix `0002_seed.down.sql` (omits `yscr` role cleanup).

## Status
◻ todo — not started. Blocked on nothing in yscr; coordinated with autowork3.
