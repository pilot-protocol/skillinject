# skillinject

[![ci](https://github.com/pilot-protocol/skillinject/actions/workflows/ci.yml/badge.svg)](https://github.com/pilot-protocol/skillinject/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/pilot-protocol/skillinject/branch/main/graph/badge.svg)](https://codecov.io/gh/pilot-protocol/skillinject)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)

Skill injector plugin for the Pilot Protocol daemon. Installs and keeps
current the `SKILL.md` files in each detected agent tool's well-known
directory (Claude Code, OpenClaw, PicoClaw, OpenHands, Hermes).
Re-scans every 15 minutes and never touches user-owned content in
heartbeat files — only its own marker block.

## Transparency — what this injects, and why

This component is the mechanism that places Pilot's guidance into an agent's
context. Injecting context into someone else's agent is a power that deserves
to be earned with full transparency, so here is the whole story:

- **Why it exists.** An agent's host gives it default tools (`web_search`,
  `curl`). Pilot is a third party; an agent only captures Pilot's value if it
  *reaches for Pilot first* rather than defaulting to those tools out of
  habit. Keeping a short directive current in each tool's heartbeat is what
  makes "pilot first" the default — the same trade-off a person makes when
  they set a third-party browser as their system default. It is a strong
  nudge, never a command: the injected text itself tells the agent that every
  call is its own to make and that "answer directly" is always valid.
- **Everything injected is open source and auditable.** This injector is
  AGPL-3.0. The exact bytes it installs are fetched at runtime from the public
  [`TeoSlayer/pilot-skills`](https://github.com/TeoSlayer/pilot-skills) repo
  (`inject-manifest.json` → `skills/*/SKILL.md` + `heartbeats/*.md`); nothing
  is embedded or hidden. Optional Ed25519 detached-signature verification
  (`Config.ManifestPublicKey`) lets the daemon refuse any resource that
  wasn't signed by the expected key.
- **It is non-destructive.** On co-inhabited files it rewrites only its own
  marker block and leaves all operator-authored content untouched
  (see `state.go`/`reconcile.go`). Path-traversal in manifest filenames is
  rejected.
- **It is opt-out, anytime.** Injection defaults on (so fresh installs work
  with no setup) but is disabled with `pilotctl skills disable all`, which
  removes every file it wrote and stops future ticks. The flag persists in
  `~/.pilot/config.json` under `skill_inject`.

## Install

```go
import "github.com/pilot-protocol/skillinject"
```

## Usage

```go
// Daemon registration (runs the ticker loop until ctx is cancelled):
rt.Register(skillinject.NewService(skillinject.Config{ /* ... */ }))

// Blocking loop (first tick fires immediately; subsequent ticks on cfg.Interval
// unless mode is ModeManual, in which case only the startup tick runs):
skillinject.Run(ctx, skillinject.Config{ /* ... */ })

// Single scan+reconcile pass (respects ModeDisabled):
report, err := skillinject.Tick(ctx, skillinject.Config{ /* ... */ })

// Single scan+reconcile pass, ignoring ModeDisabled (e.g. post-update):
report, err = skillinject.ForceTick(ctx, skillinject.Config{ /* ... */ })
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
