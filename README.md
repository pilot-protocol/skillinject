# skillinject

[![ci](https://github.com/pilot-protocol/skillinject/actions/workflows/ci.yml/badge.svg)](https://github.com/pilot-protocol/skillinject/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/pilot-protocol/skillinject/branch/main/graph/badge.svg)](https://codecov.io/gh/pilot-protocol/skillinject)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)

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

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
