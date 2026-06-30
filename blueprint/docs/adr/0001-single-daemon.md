# ADR 0001 — One daemon per user

**Status:** Accepted

Ícaro will have a single local daemon that owns the DB, the Docker lifecycle, the scheduler, and previews. The CLI and Wails are clients only. This eliminates the race between the native app and `icaro serve`, centralizes logs/events, and makes PID control robust.

Consequence: every operational feature must be reachable through the daemon's contract and testable without Wails.
