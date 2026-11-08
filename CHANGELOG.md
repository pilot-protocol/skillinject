# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security

- Signatures are required. Every file a tick fetches (the manifest, the
  entrypoint `SKILL.md`, heartbeat templates, helper scripts and plugin
  files) must carry a detached Ed25519 signature at `<url>.sig` by the key
  now compiled in as `DefaultManifestPublicKeyHex`
  (`95effdc0863c8e37d65abb292f338856ac703705041a23246c8e468d7426fb31`).
  Before, verification ran only when an operator set a key, which stock
  installs never did.
- Fail closed: a tick fetches and verifies everything it will use before
  it writes anything. A missing, malformed or non-matching signature on
  any of those files makes the tick write nothing (no skill copy, marker,
  helper, plugin file, allow-list change, retired-surface removal or cache
  file), return an error wrapping `ErrSignature`, and log
  "skillinject refused content that failed its signature check; nothing
  changed". The host keeps what the last good tick wrote. `Plan` refuses
  the same way, and `Uninstall` falls back to the cached manifest. A file
  that cannot be fetched at all (a 404 on the file itself) is still an
  error row for that file, as before.
- The signature covers the file's repo path as well as its bytes
  (`"pilot-skills-sig-v1" NUL <path> NUL <bytes>`, base64 in the `.sig`),
  so a signed file cannot be served in place of another.

### Changed

- The key overrides keep working and are the only way to change the key:
  `Config.ManifestPublicKey`, then `PILOT_SKILLINJECT_MANIFEST_PUBKEY` (the
  name pilot-daemon reads, now honoured by the library too, so pilotctl
  verifies with the daemon's key), then `PILOT_SKILLINJECT_PUBKEY`, then
  `~/.pilot/skillinject.pub`, then the built-in key. An override that does
  not decode refuses every fetch rather than falling back.
- `Config.RequireSignedManifest` and `PILOT_SKILLINJECT_REQUIRE_SIG` are
  deprecated and ignored: there is no unverified mode left to opt out of.
- A `.sig` is now the base64 text of the signature, not 64 raw bytes.

### Added

- `cmd/sign-skills`: `sign` writes the `.sig` files for a pilot-skills
  checkout (key from stdin, a file or an environment variable, never
  printed; refuses a key other than the built-in one unless `-any-key`),
  `verify` checks a checkout, `genkey` makes a key into a 0600 file, and
  `pubkey` prints a key's public half.
- `SignFile`, `VerifyFile`, `SignedFiles`, `ParsePublicKey`,
  `ErrSignature`, `SignatureSuffix`, `ManifestName` and
  `EnvDaemonManifestPublicKey`.

- Gated tools: a new optional manifest key, `gatedTools`, for skill
  targets whose directory is too generic to detect by existence alone.
  A gated tool is active only while its `requireMarker` file exists and
  names the tool's target, and that file must be inside `~/.pilot`. The
  marker holds `skills_dir=<dir>` and `skill_format=<muse|canonical>`
  lines; the tool is on only when `skills_dir` is the row's `skillsDir`
  (after resolving symlinks) and `skill_format` its `skillFormat`. An empty
  marker, one for another folder, and one for the canonical frontmatter
  turn nothing on, so an installer run for another agent's skills folder,
  or with the Muse frontmatter turned off, never makes the daemon write or
  rewrite `~/workspace/skills`. The marker must be a regular file of at
  most 4 KiB. Without a matching marker nothing under the tool's `rootDir`
  is read, written or removed, by a tick or by `Uninstall`.
  Only the entrypoint skill copy is installed; there is no heartbeat or
  plugin. `rootDir` must be inside the home directory, `skillsDir` inside
  `rootDir`. Every file operation goes through an `os.Root` on `rootDir`,
  and a symlink at the skill file or at any directory between `rootDir`
  and it is refused with an error row. Writes use an `O_EXCL` temp file
  with a random name. A gated row that resolves to a regular tool's skill
  copy is refused, so the two never rewrite each other. The key is new on
  purpose: every released version decodes the manifest with plain
  `json.Unmarshal` into a struct without it, so released daemons ignore
  these rows. As a `tools` row, the same entry would be installed by every
  released daemon on any host where the directory exists, with no marker
  check.
- `skillFormat: "muse"` (`SkillFormatMuse`) for Meta Muse, which loads
  skills from `~/workspace/skills`: the entrypoint SKILL.md frontmatter is
  rewritten to `name: "<entrypoint, - replaced by _>"` and a one-line
  quoted `description` (folded values joined, capped at 1024 bytes), and
  every other key is dropped. The body is unchanged. The output is byte
  for byte what the pilot-skills Muse installer (`muse/install.sh`) writes,
  so the two do not rewrite each other's copy.
  `TestMuseSkillMD_MatchesInstaller` runs the installer's own shell
  function against the Go port.

- `Config.ProxyCommand` and credential refresh for the default HTTP
  client. Egress proxies that rotate the credentials in `HTTPS_PROXY`
  (Meta Muse) answered every tick after the first rotation with 407,
  because the daemon keeps its launch-time environment. When
  `Config.HTTPClient` is nil, the client is now a
  `netproxy.RefreshingTransport` whose refresh command is
  `Config.ProxyCommand`, else `$PILOT_PROXY_CMD`, else `"proxy_cmd"` in
  `~/.pilot/config.json`, the same settings pilot-daemon reads. It runs at
  the start of each tick, once a minute while it runs, and on a 407, and
  the refused request is retried once. `PILOT_PROXY` or `config.json`
  `"proxy"` set to off/none/no/false/direct turns it off. Without a
  command the client is the plain one, and a 407 reported as a
  `*netproxy.ConnectError` (pilot-daemon's `http.DefaultTransport` does so
  after refreshing its own credentials) is retried once; a rejection the
  proxy garbles ("malformed HTTP status code", Meta Muse's form, which never
  reaches the transport's CONNECT hook) is retried once after 2s, when the
  transport's resolver has had a chance to re-read the credentials in the
  background.

### Changed

- `canonicalPath` resolves a path that does not exist yet through its
  nearest existing ancestor, not only its parent directory, so two paths
  that will name the same file compare equal before either is written.

## [v0.2.4] - 2026-09-23

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
