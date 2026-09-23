# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Heartbeat blocks are inserted as literal text. Rewrites went through
  `regexp.ReplaceAllString`, which expanded `$5`, `$1`, `${x}` as capture
  groups and collapsed `$$`, so "per-user **$5 budget**" reached every host
  as "per-user ** budget**".
- A heartbeat-only edit now ships. The begin comment keeps
  `hash=<sha256(SKILL.md)[:12]>` as before and adds `r=<hash>`, which
  covers SKILL.md and the rendered block (heartbeat body and disclosure
  line). Existing blocks have no `r=` and are rewritten in place once after
  upgrade, then settle. `hash=` is unchanged and the marker stays `v=1`, so
  an older binary still ticking the same home (daemon not restarted,
  manual-mode post-update tick, pinned binary) finds the block current and
  leaves it alone. It does not put back its garbled `$` text, and the two
  binaries do not rewrite each other's block on every tick.
- A file holding more than one marker block is collapsed to one block.
- A begin comment whose end line was deleted by hand is no longer paired
  with a later block's end. That pairing made the user's text in between
  part of the "block", so the next rewrite or `disable all` deleted it.
- Heartbeat files that are symlinks are edited at their target, so the
  link survives. Before, the atomic rename replaced the link with a regular
  copy. A dangling link is reported as an error, not replaced. Heartbeat
  files keep their mode.
- Tools whose heartbeat files resolve to the same file (for example
  `~/.config/opencode/AGENTS.md -> ~/.claude/CLAUDE.md`) share one block,
  written for the first tool in manifest order. The others report a noop
  row with a `note`. Before, each tool rewrote the file on every tick.
- A rendered heartbeat that contains a marker delimiter is refused with an
  error outcome instead of producing a block that splits on the next
  rewrite.
- The removal hint in every block now reads
  `Remove with: pilotctl skills disable all`. The old
  `pilotctl skills disable` fails with "skill id required".
- `Uninstall` removes the emptied `<skillsDir>/<entrypoint>/` directory.

### Added

- Retired surfaces (`retired.go`): marker blocks, plugins and helpers that
  an earlier manifest installed and the current one no longer manages are
  removed on every tick and by `Uninstall`. Built in: OpenClaw
  `workspace/HEARTBEAT.md` and PicoClaw `workspace/AGENT.md` blocks, the
  `pilotprotocol-prompt-injector` OpenClaw plugin (allow-list entry first,
  then files) and the `~/.pilot/bin/pilot-ask` helper. The manifest can
  add more under a new optional `retired` key. Reports show these as
  `state=retired`, `action=remove` (`StateRetired`, `ActionRemove`).
  Retired paths that resolve to an active heartbeat file are skipped.
- Ticks on a host in disabled mode remove retired surfaces too. They stay
  offline and use the built-in list plus the cached manifest's `retired`
  key. Hosts that ran `disable all` on an older release kept the
  prompt-injector trusted and enabled, and after upgrading they never
  reached the prune. The report has `disabled: true` and the removal rows.
- When `openclaw.json` cannot be parsed as strict JSON (OpenClaw accepts
  JSON5), the retired prompt-injector's `index.mjs` is replaced with a
  no-op plugin instead of reporting an error on every tick. The config is
  never rewritten. The row is `state=retired`, `action=rewrite` with a
  `note`, and `Uninstall` reports it as `neutralized`
  (`RemovalNeutralized`). It is reported once, and when the config parses
  again the plugin is removed as usual.
- `Outcome.Note` and `Removal.Note` explain rows whose state and action
  alone would mislead (shared heartbeat files, neutralized plugins).
- The daemon's tick log line includes `removes=` and `disabled=`, and
  each retired-surface row is logged with its path and action (errors at
  warn level).

## [v0.1.0]

Initial release.
