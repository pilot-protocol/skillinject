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
//   - Optional Ed25519 verification (Config.ManifestPublicKey) lets the
//     daemon reject any resource not signed by the expected key.
//   - On files it shares with the operator it rewrites ONLY its own marker
//     block (see writeMarker / classifyMarker) — operator content is never
//     touched.
//   - It is opt-out at any time: GetMode (config.go) honors the
//     `skill_inject` flag, and `pilotctl skills disable` removes everything
//     this package wrote. Default-on so fresh installs work without a step.
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
	// HTTPClient overrides the HTTP client used for fetching.
	HTTPClient *http.Client
	// ManifestPublicKey, when set, enables Ed25519 detached-signature
	// verification on manifest + all fetched repo files. The daemon
	// fetches <url>.sig alongside each resource and verifies before
	// accepting. Nil (default) preserves the pre-verification behavior.
	ManifestPublicKey ed25519.PublicKey
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
	// once (the startup tick).
	report, err := Tick(ctx, cfg)
	logTick(report, err)

	mode := GetMode(home)
	if mode == ModeManual || mode == ModeDisabled {
		return // manual: one-shot on startup; disabled: no ticks
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
// `pilotctl skills disable` (persisted in ~/.pilot/config.json),
// Tick returns an empty report without touching disk or the network.
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
// `pilotctl skills disable`, ForceTick is a no-op until they re-enable.
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

	// Disabled is a hard opt-out: once the user runs `pilotctl skills disable`,
	// nothing WRITES skills back — not the periodic ticker, and not a forced
	// reconcile from `pilotctl skills check`, `pilotctl update`, or an installer
	// re-run. Only the read-only dry run (Plan, behind `pilotctl skills` status)
	// still previews what a re-enable would do.
	if !dryRun && GetMode(home) == ModeDisabled {
		return &Report{At: time.Now().UTC(), Disabled: true}, nil
	}

	f := newFetcher(cfg)

	manifest, err := f.fetchManifest(ctx)
	if err != nil {
		return nil, err
	}
	// Cache the manifest so `pilotctl skills disable` can find everything
	// we wrote without depending on the network. Best-effort.
	if manifestBytes, mErr := manifestJSON(manifest); mErr == nil && !dryRun {
		_ = writeCache(home, manifestCacheRel, manifestBytes)
	}

	// Fetch entrypoint SKILL.md once; all tools get the same body.
	entrypointRel := fmt.Sprintf("skills/%s/SKILL.md", manifest.Entrypoint)
	skillBody, err := f.fetchRepoFile(ctx, entrypointRel)
	if err != nil {
		return nil, fmt.Errorf("fetch entrypoint %s: %w", entrypointRel, err)
	}
	if !dryRun {
		_ = writeCache(home, entrypointRel, skillBody)
	}

	skillHash := sha256Hex(skillBody)
	skillShort := skillHash[:12]

	report := &Report{At: time.Now().UTC()}

	// (0) install host-wide helpers (e.g. ~/.pilot/bin/pilot-ask). These
	// are tool-agnostic and referenced from every tool's heartbeat
	// directive. Failure is best-effort: we record an error outcome and
	// continue with skill/marker reconciliation.
	for _, h := range manifest.Helpers {
		dst := expandHome(h.Dst, home)
		o := Outcome{Tool: h.Name, Kind: KindHelper, Path: dst}
		body, err := f.fetchRepoFile(ctx, h.Src)
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
		tmplBody, err := f.fetchRepoFile(ctx, mt.HeartbeatTemplate)
		if err != nil {
			report.Outcomes = append(report.Outcomes, Outcome{
				Tool: mt.Name, Kind: KindMarker,
				Path:   expandHome(mt.HeartbeatPath, home),
				Action: ActionError,
				Err:    fmt.Sprintf("fetch %s: %v", mt.HeartbeatTemplate, err),
			})
			continue
		}
		if !dryRun {
			_ = writeCache(home, mt.HeartbeatTemplate, tmplBody)
		}

		ref, err := renderHeartbeat(tmplBody, heartbeatVars{EntrypointPath: skillPath})
		if err != nil {
			report.Outcomes = append(report.Outcomes, Outcome{
				Tool: mt.Name, Kind: KindMarker,
				Path:   expandHome(mt.HeartbeatPath, home),
				Action: ActionError, Err: err.Error(),
			})
			continue
		}

		hbPath := expandHome(mt.HeartbeatPath, home)
		// NOTE: gateway-mode Hermes ignores SOUL.md (#26596) -- this write
		// is a no-op for gateway users. Only local-daemon Hermes instances
		// pick up the file. See project_harness_plugin_models.md for context.
		if strings.HasSuffix(hbPath, "SOUL.md") {
			slog.Warn("skillinject: writing SOUL.md for Hermes; gateway-mode Hermes ignores this file (issue #26596) -- write is a no-op for gateway users", "path", hbPath)
		}
		mState := classifyMarker(hbPath, skillShort)
		mAction := actionFor(mState)
		mo := Outcome{
			Tool: mt.Name, Kind: KindMarker, Path: hbPath,
			State: mState, Action: mAction, Hash: skillShort,
		}
		if mAction != ActionNoop && !dryRun {
			if err := writeMarker(hbPath, ref, skillShort); err != nil {
				mo.Action = ActionError
				mo.Err = err.Error()
			}
		}
		report.Outcomes = append(report.Outcomes, mo)

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
				reconcilePluginFiles(f, ctx, mt.Plugin, home, dryRun)...)
			if mt.Plugin.AllowList != nil {
				report.Outcomes = append(report.Outcomes,
					reconcilePluginAllowList(mt.Plugin, home, dryRun))
			}
		}
	}

	return report, nil
}

// reconcilePluginFiles fetches and writes each plugin source file. One
// Outcome per file. Errors are isolated per file: a 404 on one source
// doesn't block the rest of the plugin from being reconciled.
func reconcilePluginFiles(f *fetcher, ctx context.Context, p *ManifestPlugin, home string, dryRun bool) []Outcome {
	installDir := expandHome(p.InstallPath, home)
	out := make([]Outcome, 0, len(p.Files))
	for _, pf := range p.Files {
		dst := filepath.Join(installDir, pf.Name)
		// Reject path-traversal in pf.Name (e.g. "../../.ssh/authorized_keys")
		if clean := filepath.Clean(dst); !strings.HasPrefix(clean, filepath.Clean(installDir)+string(os.PathSeparator)) && clean != filepath.Clean(installDir) {
			out = append(out, Outcome{
				Tool: p.ID, Kind: KindPluginFile, Path: dst,
				Action: ActionError,
				Err:    fmt.Sprintf("path traversal: %q escapes install dir", pf.Name),
			})
			continue
		}
		body, err := f.fetchRepoFile(ctx, pf.Src)
		if err != nil {
			out = append(out, Outcome{
				Tool: p.ID, Kind: KindPluginFile, Path: dst,
				Action: ActionError,
				Err:    fmt.Sprintf("fetch %s: %v", pf.Src, err),
			})
			continue
		}
		_ = writeCache(home, pf.Src, body)
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
// the public repo, and `pilotctl skills disable` reverses it. An operator
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
	if err != nil {
		slog.Warn("skillinject tick failed", "err", err)
		return
	}
	if r == nil {
		return
	}
	c := r.Counts()
	slog.Info("skillinject tick",
		"tools_skipped", len(r.Skipped),
		"noops", c[ActionNoop],
		"creates", c[ActionCreate],
		"rewrites", c[ActionRewrite],
		"errors", c[ActionError],
	)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
