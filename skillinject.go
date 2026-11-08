// SPDX-License-Identifier: AGPL-3.0-or-later

// Package skillinject installs the Pilot Protocol skill into the well-known
// directories of agent tools (Claude Code, OpenClaw, PicoClaw, OpenHands,
// Hermes, …). The configuration — what to inject, where, and what marker
// content to upsert into each tool's heartbeat file — is fetched at
// runtime from the pilot-skills repository on GitHub. There is no embedded
// fallback: a tick that cannot reach the network is logged and skipped;
// the next tick retries.
//
// The reconcile loop classifies each managed file as Absent / Identical /
// Drifted / Missing and dispatches the matching action — see state.go.
//
// Transparency note (this is the context-injection mechanism). Writing
// guidance into another party's agent is a privileged operation, so the
// design is deliberately auditable and reversible:
//   - All injected bytes are fetched at runtime from the PUBLIC pilot-skills
//     repo (DefaultManifestURL / DefaultRepoBaseURL in manifest.go). Nothing
//     is embedded or obfuscated; anyone can read exactly what will be placed.
//   - Every fetched resource must carry an Ed25519 signature by the key
//     built in as DefaultManifestPublicKeyHex (or an operator's override,
//     Config.ManifestPublicKey). A tick that meets an unsigned or altered
//     file writes nothing and keeps the last good state (signature.go).
//   - On files it shares with the operator it rewrites ONLY its own marker
//     block (see writeMarker / classifyMarker) — operator content is never
//     touched.
//   - It is opt-out at any time: GetMode (config.go) honors the
//     `skill_inject` flag, and `pilotctl skills disable all` removes
//     everything this package wrote — including surfaces an earlier
//     manifest installed and the current one has retired (retired.go).
//     Default-on so fresh installs work without a step.
//
// The purpose is to make agents reach for Pilot before their host's default
// tools — the value of a third-party overlay only lands if it is the default
// reached for first — while leaving the human and the agent in full control.
package skillinject

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultInterval is how often the daemon re-runs the scan/reconcile pass
// after the initial startup tick.
const DefaultInterval = 15 * time.Minute

// tickMu serializes concurrent Tick() invocations against the same
// process. Without it, two ticks racing (timer + external pilotctl
// invocation, or rapid retries) each take their own snapshot of
// openclaw.json, mutate it, and swap independently — second swap
// silently overwrites the first's changes. The mutex is the smallest
// fix; the cost is one mutex hold per ~15-min reconcile, which is
// negligible.
var tickMu sync.Mutex

// Config tunes the injector. Zero values use sensible defaults.
type Config struct {
	// Home overrides the user home dir (test hook).
	Home string
	// Interval between scan ticks after the initial startup tick.
	Interval time.Duration
	// ManifestURL overrides the canonical raw GitHub URL for inject-manifest.json.
	ManifestURL string
	// RepoBaseURL overrides the prefix used to resolve relative paths in
	// the manifest (skills/<name>/SKILL.md, heartbeats/<tool>.md).
	RepoBaseURL string
	// HTTPClient overrides the HTTP client used for fetching. When nil,
	// the client follows the proxy environment and, with a ProxyCommand,
	// re-reads rotating proxy credentials (see proxy.go).
	HTTPClient *http.Client
	// ProxyCommand is a shell command that prints the current egress
	// proxy URL, for proxies that rotate the credentials in HTTPS_PROXY
	// (Meta Muse): e.g. bash -c 'printf %s "$https_proxy"' (see proxy.go for the sandbox command).
	// The default client runs it at the start of each tick, again once a
	// minute while the tick runs, and when the proxy answers 407, and
	// retries the refused request once. Empty means $PILOT_PROXY_CMD, then
	// "proxy_cmd" in ~/.pilot/config.json (pilot-daemon's own settings),
	// unless PILOT_PROXY or config.json "proxy" turns the proxy off.
	// Ignored when HTTPClient is set.
	ProxyCommand string
	// ManifestPublicKey replaces the key that verifies the manifest and
	// every fetched repo file. Each resource's detached signature is
	// fetched from <url>.sig and checked before anything is written (see
	// signature.go). When nil, the key is resolved from the environment
	// (EnvDaemonManifestPublicKey, then EnvManifestPublicKey), then the
	// on-disk trust file (~/.pilot/skillinject.pub), then the built-in
	// DefaultManifestPublicKeyHex. Verification is never skipped.
	ManifestPublicKey ed25519.PublicKey
	// RequireSignedManifest made a verification key mandatory.
	//
	// Deprecated: signatures are always required; the field is ignored.
	RequireSignedManifest bool
}

// Run blocks running scan/reconcile ticks until ctx is cancelled. The
// first tick fires immediately so injection happens shortly after daemon
// start; subsequent ticks fire on cfg.Interval (unless mode is manual,
// in which case only the initial tick runs).
func Run(ctx context.Context, cfg Config) {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}

	home := cfg.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			slog.Warn("skillinject: home dir resolution failed, skipping startup tick", "err", err)
			return
		}
		home = h
	}

	// Run the initial tick regardless of mode. Manual mode runs exactly
	// once (the startup tick). In disabled mode that tick only removes
	// retired surfaces (see tick).
	report, err := Tick(ctx, cfg)
	logTick(report, err)

	mode := GetMode(home)
	if mode == ModeManual || mode == ModeDisabled {
		return // manual: one-shot on startup; disabled: no further ticks
	}

	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			report, err := Tick(ctx, cfg)
			logTick(report, err)
		}
	}
}

// Tick performs one scan + reconcile pass and returns a Report. Network
// failures abort the tick and return an error — there is no embedded
// fallback. Exposed for tests, one-shot use, and `pilotctl skills check`.
//
// If the user has set skill injection to disabled mode via
// `pilotctl skills disable all` (persisted in ~/.pilot/config.json),
// Tick installs nothing and makes no network request. It only removes
// retired surfaces still on disk (see tick) and returns a report with
// Disabled set.
func Tick(ctx context.Context, cfg Config) (*Report, error) {
	tickMu.Lock()
	defer tickMu.Unlock()

	return tick(ctx, cfg, false)
}

// ForceTick performs an immediate scan + reconcile pass, bypassing the
// periodic ticker (which does not run in manual mode). It is the entry
// point for `pilotctl skills check`, `pilotctl skills enable`, and the
// post-update reconcile in `pilotctl update`.
//
// It does NOT override the disabled opt-out: once the user has run
// `pilotctl skills disable all`, ForceTick is a no-op until they re-enable.
// (Enable persists mode=auto before calling in, so its reconcile still
// runs.) Manual mode is not gated here, so a forced reconcile there
// behaves exactly like Tick.
func ForceTick(ctx context.Context, cfg Config) (*Report, error) {
	tickMu.Lock()
	defer tickMu.Unlock()

	return tick(ctx, cfg, false)
}

// Plan is a read-only dry run: it performs the same scan + classification
// as ForceTick (network fetch of the manifest + entrypoint) but writes
// nothing to disk. The returned Report's Outcomes carry the State and the
// Action the next real tick WOULD take, so `pilotctl skills status` can
// preview changes without applying them.
func Plan(ctx context.Context, cfg Config) (*Report, error) {
	tickMu.Lock()
	defer tickMu.Unlock()

	return tick(ctx, cfg, true)
}

// tick is the shared implementation for Tick, ForceTick, and Plan. dryRun
// classifies without writing (Plan); otherwise it reconciles to disk.
//
// Callers hold tickMu before calling tick; do NOT acquire it here or we
// deadlock.
func tick(ctx context.Context, cfg Config, dryRun bool) (*Report, error) {
	home := cfg.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home dir: %w", err)
		}
		home = h
	}

	// Disabled is a hard opt-out: once the user runs `pilotctl skills disable all`,
	// nothing WRITES skills back — not the periodic ticker, and not a forced
	// reconcile from `pilotctl skills check`, `pilotctl update`, or an installer
	// re-run. Only the read-only dry run (Plan, behind `pilotctl skills` status)
	// still previews what a re-enable would do.
	//
	// Removing retired surfaces is still in scope: it is what the opt-out
	// asked for. Hosts that ran `disable all` on a release without the
	// retired list kept the prompt-injector plugin trusted and enabled, and
	// they are exactly the hosts that sit in disabled mode after upgrading.
	// The prune is offline (built-in list plus the cached manifest's
	// "retired" key) and skips nothing, since nothing is managed while
	// disabled.
	if !dryRun && GetMode(home) == ModeDisabled {
		return &Report{
			At:       time.Now().UTC(),
			Disabled: true,
			Outcomes: pruneRetired(collectRetiredWhileDisabled(home), false),
		}, nil
	}

	f := newFetcher(cfg)
	defer f.close()

	// Fetch and verify everything this tick uses before writing anything.
	// The manifest, the entrypoint and each file the reconcile steps below
	// read must carry a valid signature (signature.go). One that does not
	// stops the tick here: no skill copy, marker, helper, plugin file,
	// allow-list, retired-surface removal or cache file is touched, so the
	// host keeps what the last good tick wrote.
	manifest, err := f.fetchManifest(ctx)
	if err != nil {
		return nil, unchangedErr(err)
	}
	// All tools get the same entrypoint body; without it the tick stops.
	entrypointRel := entrypointPath(manifest.Entrypoint)
	skillBody, err := f.fetchRepoFile(ctx, entrypointRel)
	if err != nil {
		return nil, unchangedErr(fmt.Errorf("fetch entrypoint %s: %w", entrypointRel, err))
	}
	files, err := prefetch(ctx, f, filesForTick(manifest, home))
	if err != nil {
		return nil, unchangedErr(err)
	}
	files[entrypointRel] = fetchedFile{body: skillBody}

	// Cache the verified content: the manifest lets `pilotctl skills
	// disable all` find everything we wrote without the network. Best-effort.
	if !dryRun {
		if manifestBytes, mErr := manifestJSON(manifest); mErr == nil {
			_ = writeCache(home, manifestCacheRel, manifestBytes)
		}
		files.cache(home)
	}

	skillHash := sha256Hex(skillBody)
	skillShort := skillHash[:12]

	report := &Report{At: time.Now().UTC()}

	// Heartbeat files already reconciled this tick, by the file they
	// resolve to. Two tools can share one file, typically through a
	// symlink (~/.config/opencode/AGENTS.md -> ~/.claude/CLAUDE.md). Each
	// tool renders a different block, so reconciling the file once per tool
	// would rewrite it on every tick, each tool undoing the other. The
	// first tool in manifest order owns the file and the rest report a
	// noop row naming it.
	type hbOwner struct{ tool, hash string }
	hbOwners := map[string]hbOwner{}

	// Skill copies the regular tools reconciled this tick, by the file
	// they resolve to. A gated tool pointing at one of them is refused
	// (see reconcileGatedTool).
	skillOwners := map[string]string{}

	// (0) install host-wide helpers (e.g. ~/.pilot/bin/pilot-ask). These
	// are tool-agnostic and referenced from every tool's heartbeat
	// directive. Failure is best-effort: we record an error outcome and
	// continue with skill/marker reconciliation.
	for _, h := range manifest.Helpers {
		dst := expandHome(h.Dst, home)
		o := Outcome{Tool: h.Name, Kind: KindHelper, Path: dst}
		body, err := files.get(h.Src)
		if err != nil {
			o.Action = ActionError
			o.Err = fmt.Sprintf("fetch %s: %v", h.Src, err)
			report.Outcomes = append(report.Outcomes, o)
			continue
		}
		o.Hash = sha256Hex(body)
		var state State
		if dryRun {
			state, err = classifyHelper(dst, body, ParseFileMode(h.Mode))
		} else {
			state, err = writeHelper(dst, body, ParseFileMode(h.Mode))
		}
		o.State = state
		switch {
		case err != nil:
			o.Action = ActionError
			o.Err = err.Error()
		case state == StateAbsent:
			o.Action = ActionCreate
		case state == StateDrifted:
			o.Action = ActionRewrite
		default:
			o.Action = ActionNoop
		}
		report.Outcomes = append(report.Outcomes, o)
	}

	for _, mt := range manifest.Tools {
		rootDir := expandHome(mt.RootDir, home)
		if !dirExists(rootDir) {
			report.Skipped = append(report.Skipped, mt.Name)
			continue
		}

		// (a) skill copy
		skillPath := skillTargetPath(mt, manifest.Entrypoint, home)
		skillOwners[canonicalPath(skillPath)] = mt.Name
		state := classifySkill(skillPath, skillHash)
		action := actionFor(state)
		o := Outcome{
			Tool: mt.Name, Kind: KindSkill, Path: skillPath,
			State: state, Action: action, Hash: skillHash,
		}
		if action != ActionNoop && !dryRun {
			if err := writeFile(skillPath, skillBody); err != nil {
				o.Action = ActionError
				o.Err = err.Error()
			}
		}
		report.Outcomes = append(report.Outcomes, o)

		// (b) heartbeat marker, if this tool has a separate heartbeat file
		if mt.HeartbeatPath == "" || mt.HeartbeatTemplate == "" {
			continue
		}
		hbPath := expandHome(mt.HeartbeatPath, home)
		hbKey := canonicalPath(hbPath)
		if owner, shared := hbOwners[hbKey]; shared {
			report.Outcomes = append(report.Outcomes, Outcome{
				Tool: mt.Name, Kind: KindMarker, Path: hbPath,
				State: StateIdentical, Action: ActionNoop, Hash: owner.hash,
				Note: fmt.Sprintf("same file as the %s heartbeat (%s), which keeps a single block written for %s", owner.tool, hbKey, owner.tool),
			})
		} else {
			mo, rendered := reconcileMarker(files, mt, skillPath, skillHash, skillShort, hbPath, dryRun)
			report.Outcomes = append(report.Outcomes, mo)
			if !rendered {
				continue
			}
			hbOwners[hbKey] = hbOwner{tool: mt.Name, hash: mo.Hash}
		}

		// (c) per-tool plugin files + (d) allow-list merge.
		//
		// Today only openclaw uses this — it carries a plugin block
		// in the manifest pointing at the source files for the
		// before_prompt_build hook + the JSON paths in openclaw.json
		// where the plugin id must appear (plugins.allow,
		// plugins.entries.<id>.enabled). The plugin's own register()
		// reads the heartbeat content off disk at runtime, so once
		// these files + the allow-list entry are in place, every
		// turn of openclaw lands the pilot directive on the system
		// prompt via prependSystemContext.
		//
		// Same noop/create/rewrite/error vocabulary as skill+marker;
		// surfaces in `pilotctl skills` exactly like the other rows.
		if mt.Plugin != nil {
			report.Outcomes = append(report.Outcomes,
				reconcilePluginFiles(files, mt.Plugin, home, dryRun)...)
			if mt.Plugin.AllowList != nil {
				report.Outcomes = append(report.Outcomes,
					reconcilePluginAllowList(mt.Plugin, home, dryRun))
			}
		}
	}

	// Gated tools (gated.go): the skill copy only, and only on hosts that
	// carry the tool's marker under ~/.pilot.
	for _, gt := range manifest.GatedTools {
		o, skipped := reconcileGatedTool(gt, manifest.Entrypoint, home, skillBody, skillOwners, dryRun)
		if skipped {
			report.Skipped = append(report.Skipped, gt.Name)
			continue
		}
		report.Outcomes = append(report.Outcomes, o)
	}

	// (e) retired surfaces: marker blocks, plugins and helpers that an
	// earlier manifest installed and the current one no longer manages.
	// Runs after the active surfaces so a plugin allow-list merge above
	// and the retired-id removal here touch openclaw.json in a fixed
	// order. Only surfaces still present on disk produce an Outcome.
	report.Outcomes = append(report.Outcomes,
		pruneRetired(collectRetired(manifest, home), dryRun)...)

	return report, nil
}

// filesForTick lists the repo files the reconcile steps of tick read for
// m on this host, besides the entrypoint: each helper's source and, for
// every tool whose rootDir exists and that has a heartbeat, its template
// and the source of each plugin file it would write. It may list a file
// a tick ends up not reading (the plugin of a tool whose template fails
// to render), never one it reads: the reconcile steps read only what
// prefetch verified.
func filesForTick(m *Manifest, home string) []string {
	var out []string
	for _, h := range m.Helpers {
		out = append(out, h.Src)
	}
	for _, mt := range m.Tools {
		if !dirExists(expandHome(mt.RootDir, home)) {
			continue
		}
		if mt.HeartbeatPath == "" || mt.HeartbeatTemplate == "" {
			continue
		}
		out = append(out, mt.HeartbeatTemplate)
		if mt.Plugin == nil {
			continue
		}
		installDir := expandHome(mt.Plugin.InstallPath, home)
		for _, pf := range mt.Plugin.Files {
			if pathWithin(installDir, filepath.Join(installDir, pf.Name)) {
				out = append(out, pf.Src)
			}
		}
	}
	return out
}

// fetchedFile is one prefetched repo file: its verified body, or the error
// that kept it from being fetched.
type fetchedFile struct {
	body []byte
	err  error
}

// fetchedFiles maps repo paths to what prefetch got for them.
type fetchedFiles map[string]fetchedFile

// prefetch fetches and verifies each of names. A file that cannot be
// fetched is recorded with its error, and the reconcile step that needs it
// reports an error row as before. A file whose signature is missing or
// does not verify stops prefetch with that error, and the tick writes
// nothing.
func prefetch(ctx context.Context, f *fetcher, names []string) (fetchedFiles, error) {
	out := fetchedFiles{}
	for _, n := range names {
		name := repoPath(n)
		if _, done := out[name]; done {
			continue
		}
		body, err := f.fetchRepoFile(ctx, name)
		if errors.Is(err, ErrSignature) {
			return nil, err
		}
		out[name] = fetchedFile{body: body, err: err}
	}
	return out, nil
}

// get returns the prefetched body of repo path p. A path prefetch did not
// fetch is an error, never a fetch here, so nothing unverified is used.
func (ff fetchedFiles) get(p string) ([]byte, error) {
	r, ok := ff[repoPath(p)]
	if !ok {
		return nil, fmt.Errorf("%s was not fetched and verified before the write phase", p)
	}
	return r.body, r.err
}

// cache mirrors each verified file under ~/.pilot/skills-cache.
// Best-effort.
func (ff fetchedFiles) cache(home string) {
	for name, r := range ff {
		if r.err == nil && validRepoPath(name) {
			_ = writeCache(home, name, r.body)
		}
	}
}

// unchangedErr says, on a signature failure, that the tick changed
// nothing.
func unchangedErr(err error) error {
	if errors.Is(err, ErrSignature) {
		return fmt.Errorf("%w; nothing changed, keeping what the last good tick wrote", err)
	}
	return err
}

// reconcileMarker renders mt's (prefetched) heartbeat template, then
// classifies and (unless dryRun) writes its marker block into hbPath. The
// bool is false when the template could not be fetched or rendered; the
// Outcome is then an error row and the caller skips the rest of the tool,
// as it always has.
func reconcileMarker(files fetchedFiles, mt ManifestTool, skillPath, skillHash, skillShort, hbPath string, dryRun bool) (Outcome, bool) {
	tmplBody, err := files.get(mt.HeartbeatTemplate)
	if err != nil {
		return Outcome{
			Tool: mt.Name, Kind: KindMarker,
			Path:   hbPath,
			Action: ActionError,
			Err:    fmt.Sprintf("fetch %s: %v", mt.HeartbeatTemplate, err),
		}, false
	}

	ref, err := renderHeartbeat(tmplBody, heartbeatVars{EntrypointPath: skillPath})
	if err == nil {
		err = validateMarkerRef(ref)
	}
	if err != nil {
		return Outcome{
			Tool: mt.Name, Kind: KindMarker,
			Path:   hbPath,
			Action: ActionError, Err: err.Error(),
		}, false
	}

	// NOTE: gateway-mode Hermes ignores SOUL.md (#26596) -- this write
	// is a no-op for gateway users. Only local-daemon Hermes instances
	// pick up the file. See project_harness_plugin_models.md for context.
	if strings.HasSuffix(hbPath, "SOUL.md") {
		slog.Warn("skillinject: writing SOUL.md for Hermes; gateway-mode Hermes ignores this file (issue #26596) -- write is a no-op for gateway users", "path", hbPath)
	}
	// hash= stays the SKILL.md short hash (what older binaries
	// compare); r= also covers the rendered block, so a
	// heartbeat-template-only change is Drifted and ships.
	rShort := markerHash(skillHash, ref)
	mState := classifyMarker(hbPath, skillShort, rShort)
	mAction := actionFor(mState)
	mo := Outcome{
		Tool: mt.Name, Kind: KindMarker, Path: hbPath,
		State: mState, Action: mAction, Hash: rShort,
	}
	if mAction != ActionNoop && !dryRun {
		if err := writeMarker(hbPath, ref, skillShort, rShort); err != nil {
			mo.Action = ActionError
			mo.Err = err.Error()
		}
	}
	return mo, true
}

// reconcilePluginFiles writes each (prefetched) plugin source file. One
// Outcome per file. Fetch errors are isolated per file: a 404 on one
// source doesn't block the rest of the plugin from being reconciled. (A
// bad signature never gets here: it stops the whole tick.)
func reconcilePluginFiles(files fetchedFiles, p *ManifestPlugin, home string, dryRun bool) []Outcome {
	installDir := expandHome(p.InstallPath, home)
	out := make([]Outcome, 0, len(p.Files))
	for _, pf := range p.Files {
		dst := filepath.Join(installDir, pf.Name)
		// Only names that stay inside installDir are written.
		if !pathWithin(installDir, dst) {
			out = append(out, Outcome{
				Tool: p.ID, Kind: KindPluginFile, Path: dst,
				Action: ActionError,
				Err:    fmt.Sprintf("path traversal: %q escapes install dir", pf.Name),
			})
			continue
		}
		body, err := files.get(pf.Src)
		if err != nil {
			out = append(out, Outcome{
				Tool: p.ID, Kind: KindPluginFile, Path: dst,
				Action: ActionError,
				Err:    fmt.Sprintf("fetch %s: %v", pf.Src, err),
			})
			continue
		}
		want := sha256Hex(body)
		state := classifyPluginFile(dst, want)
		action := actionFor(state)
		o := Outcome{
			Tool: p.ID, Kind: KindPluginFile, Path: dst,
			State: state, Action: action, Hash: want,
		}
		if action != ActionNoop && !dryRun {
			if werr := writeFile(dst, body); werr != nil {
				o.Action = ActionError
				o.Err = werr.Error()
			}
		}
		out = append(out, o)
	}
	return out
}

// reconcilePluginAllowList does a JSON-merge into the tool's plugin
// config so the plugin id appears in the trust array AND its entries
// row has enabled=true. Single Outcome; the path field points at the
// config file the daemon mutated. Read-modify-write is atomic via
// .tmp + rename.
//
// Transparency note (most sensitive step — auto-trusting our own plugin).
// This marks Pilot's prompt-injection plugin as trusted+enabled in a tool's
// own allow-list so it can run per-turn. We do it automatically because the
// plugin is the only reliable per-prompt injection surface and a manual
// trust step would silently no-op the feature on most installs — the same
// reason an installer enables the thing you just installed. It is bounded
// and reversible: we only ever add OUR id (`p.ID`), we never remove or
// disable anyone else's plugin, the plugin source is open and fetched from
// the public repo, and `pilotctl skills disable all` reverses it (and a
// plugin the manifest later retires is removed again, see retired.go). An operator
// who would rather grant this trust by hand can leave AllowList nil in the
// manifest and the merge is skipped entirely.
func reconcilePluginAllowList(p *ManifestPlugin, home string, dryRun bool) Outcome {
	al := p.AllowList
	cfgPath := expandHome(al.ConfigPath, home)
	o := Outcome{
		Tool: p.ID, Kind: KindPluginAllowList, Path: cfgPath,
	}
	state := classifyPluginAllowList(cfgPath, al.AllowListJsonPath, al.EntriesJsonPath, p.ID)
	o.State = state
	o.Action = actionFor(state)
	if o.Action == ActionNoop || dryRun {
		return o
	}
	if err := mergePluginAllowList(cfgPath, al.AllowListJsonPath, al.EntriesJsonPath, p.ID); err != nil {
		o.Action = ActionError
		o.Err = err.Error()
	}
	return o
}

// skillTargetPath returns where the entrypoint SKILL.md should be written
// for one tool. Most tools follow the AgentSkills directory convention
// (<skillsDir>/<name>/SKILL.md). Tools with skillNaming="flat" use a
// single-file layout (<skillsDir>/<name>.md, e.g. OpenHands microagents).
func skillTargetPath(mt ManifestTool, entrypoint, home string) string {
	skillsDir := expandHome(mt.SkillsDir, home)
	if mt.SkillNaming == "flat" {
		return skillsDir + "/" + entrypoint + ".md"
	}
	return skillsDir + "/" + entrypoint + "/SKILL.md"
}

func logTick(r *Report, err error) {
	logTickTo(slog.Default(), r, err)
}

// logTickTo logs one tick's summary and then one line per retired-surface
// row. Those rows delete files and edit a tool's config (openclaw.json)
// without anyone asking at that moment, so each one is logged with its
// path to keep the mechanism auditable.
func logTickTo(l *slog.Logger, r *Report, err error) {
	if errors.Is(err, ErrSignature) {
		l.Warn("skillinject refused content that failed its signature check; nothing changed", "err", err)
		return
	}
	if err != nil {
		l.Warn("skillinject tick failed", "err", err)
		return
	}
	if r == nil {
		return
	}
	c := r.Counts()
	l.Info("skillinject tick",
		"disabled", r.Disabled,
		"tools_skipped", len(r.Skipped),
		"noops", c[ActionNoop],
		"creates", c[ActionCreate],
		"rewrites", c[ActionRewrite],
		"removes", c[ActionRemove],
		"errors", c[ActionError],
	)
	for _, o := range r.Outcomes {
		if o.State != StateRetired {
			continue
		}
		attrs := []any{"tool", o.Tool, "kind", o.Kind, "path", o.Path, "action", o.Action}
		if o.Note != "" {
			attrs = append(attrs, "note", o.Note)
		}
		if o.Err != "" {
			l.Warn("skillinject retired surface", append(attrs, "err", o.Err)...)
			continue
		}
		l.Info("skillinject retired surface", attrs...)
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
