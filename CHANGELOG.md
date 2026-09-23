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
- The marker `hash=` now covers the entrypoint SKILL.md and the rendered
  block (heartbeat body and disclosure line). It used to be the SKILL.md
  hash alone, so an edit to a heartbeat template never shipped until
  SKILL.md also changed. Existing blocks carry the old hash and are
  rewritten in place once after upgrade, then settle. The marker stays
  `v=1` so older binaries still recognise (and replace, not duplicate)
  the block.
- A file holding more than one marker block is collapsed to one block.
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

## [v0.1.0]

Initial release.
