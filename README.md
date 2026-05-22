# skillinject

Pilot Protocol skill injector — the daemon plugin that installs and
keeps current the `SKILL.md` files in each detected agent tool's
well-known directory (Claude Code, OpenClaw, PicoClaw, OpenHands,
Hermes). Re-scans every 15 minutes; never touches user-owned content
in heartbeat files (only its own marker block).

## Layout

| File | What it does |
|---|---|
| `skillinject.go` | Entry point — `Run(ctx, Config)` (the reconcile loop) + manifest fetch. |
| `config.go` | `Config` struct (install paths, marker, fetch URL). |
| `manifest.go` | Parses the manifest JSON fetched from the pilot-skills repo. |
| `reconcile.go` | Per-tick state machine: Absent → install, Drifted → rewrite, Identical → noop. |
| `state.go` | File-state classifier (sha256 + heartbeat-marker parsing). |
| `uninstall.go` | Strip-only on co-inhabited files; delete-safe in pilot-owned subdirs. |
| `plugin_allowlist.go` | OpenClaw allow-list JSON merge + .pilot-bak snapshot. |
| `service.go` | `*Service` — `coreapi.Service` adapter. Build tag `!no_skillinject`. |
| `service_disabled.go` | Stub when `-tags no_skillinject` is set. |
| `dockertest/` | Containerised reconcile-loop integration runner. |

## Import paths

```go
import "github.com/pilot-protocol/skillinject"

// Daemon registration:
rt.Register(skillinject.NewService(skillinject.Config{...}))

// CLI:
report := skillinject.Reconcile(skillinject.Config{...})
```

## Disabling

`go build -tags no_skillinject` compiles a stub that does nothing.
