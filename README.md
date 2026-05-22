# skillinject

Skill injector plugin for the Pilot Protocol daemon. Installs and keeps
current the `SKILL.md` files in each detected agent tool's well-known
directory (Claude Code, OpenClaw, PicoClaw, OpenHands, Hermes).
Re-scans every 15 minutes and never touches user-owned content in
heartbeat files — only its own marker block.

## Install

```go
import "github.com/pilot-protocol/skillinject"
```

## Usage

```go
// Daemon registration:
rt.Register(skillinject.NewService(skillinject.Config{ /* ... */ }))

// Standalone (e.g. from a CLI):
report := skillinject.Reconcile(skillinject.Config{ /* ... */ })
_ = report
```

## Layout

| File | What it does |
|---|---|
| `skillinject.go` | Entry point — `Run(ctx, Config)` reconcile loop and manifest fetch. |
| `config.go` | `Config` struct (install paths, marker, fetch URL). |
| `manifest.go` | Parses the manifest JSON fetched from the pilot-skills repo. |
| `reconcile.go` | Per-tick state machine: Absent → install, Drifted → rewrite, Identical → noop. |
| `state.go` | File-state classifier (sha256 + heartbeat-marker parsing). |
| `uninstall.go` | Strip-only on co-inhabited files; delete-safe in pilot-owned subdirs. |
| `plugin_allowlist.go` | OpenClaw allow-list JSON merge and `.pilot-bak` snapshot. |
| `service.go` | `*Service` — `coreapi.Service` adapter. Build tag `!no_skillinject`. |
| `service_disabled.go` | Stub when `-tags no_skillinject` is set. |
| `dockertest/` | Containerised reconcile-loop integration runner. |

## Build tags

| Tag | Effect |
|---|---|
| `no_skillinject` | Compiles a stub that does nothing. |
