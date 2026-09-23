// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilot-protocol/skillinject"
)

// currentOpenClaw mirrors today's pilot-skills manifest row for OpenClaw:
// heartbeat in workspace/AGENTS.md, no singular plugin block.
func currentOpenClaw() []skillinject.ManifestTool {
	return []skillinject.ManifestTool{{
		Name:              "openclaw",
		RootDir:           "~/.openclaw",
		SkillsDir:         "~/.openclaw/skills",
		HeartbeatPath:     "~/.openclaw/workspace/AGENTS.md",
		HeartbeatTemplate: "heartbeats/openclaw.md",
	}}
}

// staleOpenClawHeartbeat is workspace/HEARTBEAT.md as older installs left
// it: OpenClaw's own boilerplate plus a block from the retired manifest.
const staleOpenClawHeartbeat = "# HEARTBEAT.md\n\n# Keep this file empty (or with only comments) to skip heartbeat API calls.\n\n" +
	"<!-- pilot:begin v=1 hash=9f90cfe03bc3\n     Inserted by pilot-daemon. Remove with: pilotctl skills disable\n-->\n" +
	"stale directive: ~436 agents\n<!-- pilot:end -->\n"

// openClawConfig is an openclaw.json with the retired prompt-injector
// trusted and enabled next to plugins that are not ours to touch. The
// chat id is larger than 2^53 (a float64 round-trip would corrupt it) and
// the note has characters encoding/json escapes by default.
const openClawConfig = `{
  "channels": {
    "telegram": {
      "chatId": 12345678901234567891
    }
  },
  "note": "a <b> & c",
  "plugins": {
    "allow": [
      "pilotprotocol-prompt-injector",
      "pilotprotocol-webhook-receiver",
      "user-plugin"
    ],
    "entries": {
      "pilotprotocol-prompt-injector": {
        "enabled": true
      },
      "pilotprotocol-webhook-receiver": {
        "enabled": true
      },
      "telegram": {
        "enabled": true
      }
    }
  }
}
`

type openClawHost struct {
	home, heartbeat, pluginDir, webhookDir, config string
}

// seedStaleOpenClaw lays out an OpenClaw install carrying every retired
// Pilot surface from DOCS-09.
func seedStaleOpenClaw(t *testing.T) openClawHost {
	t.Helper()
	home := t.TempDir()
	h := openClawHost{
		home:       home,
		heartbeat:  filepath.Join(home, ".openclaw", "workspace", "HEARTBEAT.md"),
		pluginDir:  filepath.Join(home, ".openclaw", "extensions", "pilotprotocol-prompt-injector"),
		webhookDir: filepath.Join(home, ".openclaw", "extensions", "pilotprotocol-webhook-receiver"),
		config:     filepath.Join(home, ".openclaw", "openclaw.json"),
	}
	mustMkdirAll(t, filepath.Dir(h.heartbeat))
	mustMkdirAll(t, h.pluginDir)
	mustMkdirAll(t, h.webhookDir)
	mustWriteFile(t, h.heartbeat, staleOpenClawHeartbeat, 0o644)
	mustWriteFile(t, filepath.Join(h.pluginDir, "openclaw.plugin.json"), `{"id":"pilotprotocol-prompt-injector"}`, 0o644)
	mustWriteFile(t, filepath.Join(h.pluginDir, "index.mjs"), "// retired\n", 0o644)
	mustWriteFile(t, filepath.Join(h.webhookDir, "index.mjs"), "// not retired\n", 0o644)
	mustWriteFile(t, h.config, openClawConfig, 0o600)
	return h
}

func mustWriteFile(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func retiredOutcomes(rep *skillinject.Report) []skillinject.Outcome {
	var out []skillinject.Outcome
	for _, o := range rep.Outcomes {
		if o.State == skillinject.StateRetired {
			out = append(out, o)
		}
	}
	return out
}

func assertRetiredCleanedUp(t *testing.T, h openClawHost) {
	t.Helper()
	// Stale block gone, user's file and text kept.
	hb := mustRead(t, h.heartbeat)
	if strings.Contains(hb, "pilot:begin") || strings.Contains(hb, "stale directive") {
		t.Errorf("stale block still in HEARTBEAT.md:\n%s", hb)
	}
	if !strings.Contains(hb, "# Keep this file empty") {
		t.Errorf("user text lost from HEARTBEAT.md:\n%s", hb)
	}
	// Retired plugin gone; the plugin that is not retired untouched.
	mustNotExist(t, h.pluginDir)
	mustExist(t, filepath.Join(h.webhookDir, "index.mjs"))

	cfg := mustRead(t, h.config)
	if strings.Contains(cfg, "pilotprotocol-prompt-injector") {
		t.Errorf("retired id still in openclaw.json:\n%s", cfg)
	}
	for _, want := range []string{
		`"pilotprotocol-webhook-receiver"`, `"user-plugin"`, `"telegram"`,
		`12345678901234567891`, // exact digits, no float64 round-trip
		`"a <b> & c"`,          // not HTML-escaped
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("openclaw.json lost %s:\n%s", want, cfg)
		}
	}
	st, err := os.Stat(h.config)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("openclaw.json mode = %o, want 600 (it can hold credentials)", st.Mode().Perm())
	}
	if _, err := os.Stat(h.config + skillinject.BackupSuffix); err == nil {
		t.Errorf("prune should not create a %s snapshot", skillinject.BackupSuffix)
	}
}

// DOCS-09: with today's manifest (OpenClaw heartbeat in AGENTS.md, no
// prompt-injector), a tick removes the stale HEARTBEAT.md block and the
// retired plugin, including its openclaw.json trust entry, and the next
// tick has nothing left to do.
func TestTick_PrunesRetiredOpenClawSurfaces(t *testing.T) {
	t.Parallel()
	h := seedStaleOpenClaw(t)
	r := newFakeRepo(t)
	r.withTools(currentOpenClaw())
	cfg := r.cfg(h.home)

	rep, err := skillinject.Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	got := map[string]skillinject.Outcome{}
	for _, o := range retiredOutcomes(rep) {
		got[o.Path] = o
	}
	for _, p := range []string{
		h.heartbeat, h.config,
		filepath.Join(h.pluginDir, "openclaw.plugin.json"),
		filepath.Join(h.pluginDir, "index.mjs"),
	} {
		o, ok := got[p]
		if !ok {
			t.Errorf("no retired outcome for %s; got %+v", p, rep.Outcomes)
			continue
		}
		if o.Action != skillinject.ActionRemove {
			t.Errorf("%s: action %s (err %q), want remove", p, o.Action, o.Err)
		}
	}
	if len(got) != 4 {
		t.Errorf("want 4 retired outcomes, got %d: %+v", len(got), got)
	}
	assertRetiredCleanedUp(t, h)

	// The active heartbeat went to AGENTS.md as usual.
	if !strings.Contains(mustRead(t, filepath.Join(h.home, ".openclaw", "workspace", "AGENTS.md")), "pilot:begin") {
		t.Error("active AGENTS.md block not written")
	}

	// Settled: no retired rows, nothing rewritten.
	cfgBytes := mustRead(t, h.config)
	rep, err = skillinject.Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Tick #2: %v", err)
	}
	if rr := retiredOutcomes(rep); len(rr) != 0 {
		t.Fatalf("second tick still pruning: %+v", rr)
	}
	if c := rep.Counts(); c[skillinject.ActionNoop] != len(rep.Outcomes) {
		t.Fatalf("second tick not all noop: %+v", rep.Outcomes)
	}
	if mustRead(t, h.config) != cfgBytes {
		t.Fatal("openclaw.json rewritten on a settled tick")
	}
}

// The dry run (pilotctl skills status) previews the prune and changes
// nothing on disk.
func TestPlan_ReportsRetiredWithoutTouchingDisk(t *testing.T) {
	t.Parallel()
	h := seedStaleOpenClaw(t)
	r := newFakeRepo(t)
	r.withTools(currentOpenClaw())

	rep, err := skillinject.Plan(context.Background(), r.cfg(h.home))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	rr := retiredOutcomes(rep)
	if len(rr) != 4 {
		t.Fatalf("want 4 retired outcomes, got %+v", rr)
	}
	for _, o := range rr {
		if o.Action != skillinject.ActionRemove {
			t.Errorf("%s: action %s, want remove", o.Path, o.Action)
		}
	}
	if mustRead(t, h.heartbeat) != staleOpenClawHeartbeat {
		t.Error("Plan changed HEARTBEAT.md")
	}
	if mustRead(t, h.config) != openClawConfig {
		t.Error("Plan changed openclaw.json")
	}
	mustExist(t, filepath.Join(h.pluginDir, "index.mjs"))
}

// openClawJSON5Config is openclaw.json as OpenClaw also accepts it: JSON5
// with comments and trailing commas. Strict encoding/json cannot parse it,
// and it is never rewritten.
const openClawJSON5Config = `{
  // my telegram bot
  "channels": {"telegram": {"botToken": "123:abc"}},
  "plugins": {
    "allow": ["pilotprotocol-prompt-injector", "user-plugin",],
    "entries": {
      "pilotprotocol-prompt-injector": {"enabled": true},
    },
  },
}
`

// If openclaw.json cannot be parsed (JSON5, or just broken), the id cannot
// be taken out of it, and the files cannot be deleted without leaving
// OpenClaw trusting a plugin with nothing on disk. The tick replaces the
// plugin's index.mjs with a no-op instead and reports that once. Later
// ticks are quiet: no error row on every tick. When the config parses
// again, the plugin is removed as usual.
func TestTick_RetiredPluginNeutralizedWhenConfigUnparseable(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"json5":  openClawJSON5Config,
		"broken": "{ this is not json",
	} {
		body := body
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := seedStaleOpenClaw(t)
			mustWriteFile(t, h.config, body, 0o600)
			index := filepath.Join(h.pluginDir, "index.mjs")
			r := newFakeRepo(t)
			r.withTools(currentOpenClaw())
			cfg := r.cfg(h.home)

			// The dry run previews the rewrite and changes nothing.
			rep, err := skillinject.Plan(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if o := outcomeFor(rep, index); o == nil || o.State != skillinject.StateRetired || o.Action != skillinject.ActionRewrite {
				t.Fatalf("Plan: want retired/rewrite for index.mjs, got %+v", o)
			}
			if mustRead(t, index) != "// retired\n" {
				t.Fatal("Plan rewrote index.mjs")
			}

			rep, err = skillinject.Tick(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Tick: %v", err)
			}
			if c := rep.Counts(); c[skillinject.ActionError] != 0 {
				t.Fatalf("want no error rows, got %+v", rep.Outcomes)
			}
			o := outcomeFor(rep, index)
			if o == nil || o.State != skillinject.StateRetired || o.Action != skillinject.ActionRewrite {
				t.Fatalf("want retired/rewrite for index.mjs, got %+v", o)
			}
			if !strings.Contains(o.Note, "no-op") || !strings.Contains(o.Note, "refusing to edit") {
				t.Errorf("note does not explain the neutralization: %q", o.Note)
			}
			stub := mustRead(t, index)
			if !strings.Contains(stub, "register() {}") || strings.Contains(stub, "before_prompt_build") {
				t.Fatalf("index.mjs is not the no-op stub:\n%s", stub)
			}
			if !strings.Contains(stub, `id: "pilotprotocol-prompt-injector"`) {
				t.Fatalf("stub lost the plugin id OpenClaw trusts:\n%s", stub)
			}
			// openclaw.plugin.json stays so the trusted id still resolves.
			mustExist(t, filepath.Join(h.pluginDir, "openclaw.plugin.json"))
			if mustRead(t, h.config) != body {
				t.Fatal("unparseable config was modified")
			}
			// The marker strip does not depend on the config and still happens.
			if strings.Contains(mustRead(t, h.heartbeat), "pilot:begin") {
				t.Error("stale HEARTBEAT.md block not stripped")
			}

			// Reported once: the next tick has nothing to say about the plugin.
			rep, err = skillinject.Tick(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Tick #2: %v", err)
			}
			if rr := retiredOutcomes(rep); len(rr) != 0 {
				t.Fatalf("second tick still reports the retired plugin: %+v", rr)
			}
			if mustRead(t, index) != stub {
				t.Fatal("stub rewritten on the second tick")
			}

			// The user makes the config strict JSON: the id is removed and
			// the plugin, stub included, is deleted.
			mustWriteFile(t, h.config, openClawConfig, 0o600)
			if _, err := skillinject.Tick(context.Background(), cfg); err != nil {
				t.Fatalf("Tick #3: %v", err)
			}
			assertRetiredCleanedUp(t, h)
		})
	}
}

// A retired plugin without a known stub (manifest-declared) still reports
// an error and keeps its files when the config cannot be parsed.
func TestTick_RetiredPluginWithoutStubKeptWhenConfigUnparseable(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	dir := filepath.Join(home, ".tool", "ext", "pilot-old")
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, "index.mjs"), "// old\n", 0o644)
	cfgPath := filepath.Join(home, ".tool", "cfg.json")
	mustWriteFile(t, cfgPath, "{ // json5\n \"a\": [\"pilot-old\",],}\n", 0o644)

	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	r.manifest.Retired = &skillinject.ManifestRetired{Plugins: []skillinject.ManifestPlugin{{
		ID: "pilot-old", InstallPath: "~/.tool/ext/pilot-old",
		Files:     []skillinject.ManifestPluginFile{{Name: "index.mjs"}},
		AllowList: &skillinject.ManifestPluginAllowList{ConfigPath: "~/.tool/cfg.json", AllowListJsonPath: "a", EntriesJsonPath: "e"},
	}}}
	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	o := outcomeFor(rep, cfgPath)
	if o == nil || o.Action != skillinject.ActionError || !strings.Contains(o.Err, "leaving the retired plugin's files in place") {
		t.Fatalf("want an error outcome for the config, got %+v", o)
	}
	if mustRead(t, filepath.Join(dir, "index.mjs")) != "// old\n" {
		t.Fatal("plugin without a stub was modified")
	}
}

// `pilotctl skills disable all` on a host whose openclaw.json is JSON5
// neutralizes the retired plugin too, and says so.
func TestUninstall_NeutralizesRetiredPluginWithJSON5Config(t *testing.T) {
	t.Parallel()
	h := seedStaleOpenClaw(t)
	mustWriteFile(t, h.config, openClawJSON5Config, 0o600)
	r := newFakeRepo(t)
	r.withTools(currentOpenClaw())
	rep, err := skillinject.Uninstall(context.Background(), r.cfg(h.home))
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	index := filepath.Join(h.pluginDir, "index.mjs")
	var got *skillinject.Removal
	for i := range rep.Removals {
		if rep.Removals[i].Path == index {
			got = &rep.Removals[i]
		}
	}
	if got == nil || got.Action != skillinject.RemovalNeutralized || got.Note == "" {
		t.Fatalf("want a neutralized removal with a note for index.mjs, got %+v", got)
	}
	if c := rep.Counts(); c[skillinject.RemovalError] != 0 {
		t.Errorf("errors during uninstall: %+v", rep.Removals)
	}
	if !strings.Contains(mustRead(t, index), "register() {}") {
		t.Fatal("index.mjs not neutralized")
	}
	if mustRead(t, h.config) != openClawJSON5Config {
		t.Fatal("JSON5 config was modified")
	}
}

func outcomeFor(rep *skillinject.Report, path string) *skillinject.Outcome {
	for i := range rep.Outcomes {
		if rep.Outcomes[i].Path == path {
			return &rep.Outcomes[i]
		}
	}
	return nil
}

// A path the current manifest still manages is never treated as retired,
// so a manifest that (re)adopts HEARTBEAT.md is not fought by the prune.
func TestTick_ActiveHeartbeatNeverPruned(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".openclaw"))
	hb := filepath.Join(home, ".openclaw", "workspace", "HEARTBEAT.md")

	r := newFakeRepo(t)
	tools := currentOpenClaw()
	tools[0].HeartbeatPath = "~/.openclaw/workspace/HEARTBEAT.md"
	r.withTools(tools)
	cfg := r.cfg(home)

	for i := 0; i < 3; i++ {
		rep, err := skillinject.Tick(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Tick #%d: %v", i, err)
		}
		if rr := retiredOutcomes(rep); len(rr) != 0 {
			t.Fatalf("tick %d pruned an active path: %+v", i, rr)
		}
		if !strings.Contains(mustRead(t, hb), "pilot:begin") {
			t.Fatalf("tick %d: active block missing", i)
		}
		if i > 0 {
			if c := rep.Counts(); c[skillinject.ActionNoop] != len(rep.Outcomes) {
				t.Fatalf("tick %d not settled: %+v", i, rep.Outcomes)
			}
		}
	}
}

// pilot-skills can retire paths through the manifest. Entries outside the
// safety rules (helpers outside ~/.pilot, relative paths, a plugin dir not
// named after its id) are ignored.
func TestTick_ManifestDeclaredRetired(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	oldHB := filepath.Join(home, ".config", "oldtool", "RULES.md")
	oldHelper := filepath.Join(home, ".pilot", "bin", "old-helper")
	outside := filepath.Join(home, ".local", "bin", "not-ours")
	misnamed := filepath.Join(home, ".oldtool", "extensions", "somebody-else")
	for _, d := range []string{filepath.Dir(oldHB), filepath.Dir(oldHelper), filepath.Dir(outside), misnamed} {
		mustMkdirAll(t, d)
	}
	mustWriteFile(t, oldHB, "keep me\n\n<!-- pilot:begin v=1 hash=abc\n-->\nold\n<!-- pilot:end -->\n", 0o644)
	mustWriteFile(t, oldHelper, "#!/bin/sh\n", 0o755)
	mustWriteFile(t, outside, "#!/bin/sh\n", 0o755)
	mustWriteFile(t, filepath.Join(misnamed, "index.mjs"), "x", 0o644)

	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	r.manifest.Retired = &skillinject.ManifestRetired{
		Markers: []skillinject.RetiredMarker{
			{Tool: "oldtool", Path: "~/.config/oldtool/RULES.md"},
			{Tool: "rel", Path: "relative/RULES.md"},
		},
		Helpers: []skillinject.ManifestHelper{
			{Name: "old-helper", Dst: "~/.pilot/bin/old-helper"},
			{Name: "not-ours", Dst: "~/.local/bin/not-ours"},
		},
		Plugins: []skillinject.ManifestPlugin{{
			ID:          "pilot-old",
			InstallPath: "~/.oldtool/extensions/somebody-else",
			Files:       []skillinject.ManifestPluginFile{{Name: "index.mjs"}},
		}},
	}

	if _, err := skillinject.Tick(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := mustRead(t, oldHB); got != "keep me\n\n" {
		t.Errorf("retired marker not stripped cleanly: %q", got)
	}
	mustNotExist(t, oldHelper)
	mustExist(t, outside)
	mustExist(t, filepath.Join(misnamed, "index.mjs"))
}

// The built-in retired pilot-ask helper is deleted; a directory at that
// path is not ours and is left alone.
func TestTick_RetiredHelperOnlyRemovesFiles(t *testing.T) {
	t.Parallel()
	for _, asDir := range []bool{false, true} {
		home := t.TempDir()
		mustMkdirAll(t, filepath.Join(home, ".claude"))
		p := filepath.Join(home, ".pilot", "bin", "pilot-ask")
		if asDir {
			mustMkdirAll(t, filepath.Join(p, "keep"))
		} else {
			mustMkdirAll(t, filepath.Dir(p))
			mustWriteFile(t, p, "#!/bin/sh\n", 0o755)
		}
		r := newFakeRepo(t)
		r.withTools(claudeOnly())
		rep, err := skillinject.Tick(context.Background(), r.cfg(home))
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if asDir {
			mustExist(t, filepath.Join(p, "keep"))
			if rr := retiredOutcomes(rep); len(rr) != 0 {
				t.Errorf("directory reported as retired: %+v", rr)
			}
		} else {
			mustNotExist(t, p)
			mustExist(t, filepath.Dir(p)) // ~/.pilot/bin itself stays
		}
	}
}

// `pilotctl skills disable all` removes retired surfaces too, even though
// the current manifest no longer lists them.
func TestUninstall_RemovesRetiredSurfaces(t *testing.T) {
	t.Parallel()
	h := seedStaleOpenClaw(t)
	r := newFakeRepo(t)
	r.withTools(currentOpenClaw())
	cfg := r.cfg(h.home)
	if _, err := skillinject.Tick(context.Background(), cfg); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// Simulate an older release putting the stale surfaces back.
	h2 := h
	mustWriteFile(t, h2.heartbeat, staleOpenClawHeartbeat, 0o644)
	mustMkdirAll(t, h2.pluginDir)
	mustWriteFile(t, filepath.Join(h2.pluginDir, "index.mjs"), "// retired\n", 0o644)
	mustWriteFile(t, h2.config, openClawConfig, 0o600)

	rep, err := skillinject.Uninstall(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	acts := map[string]skillinject.RemovalKind{}
	for _, x := range rep.Removals {
		acts[x.Path] = x.Action
	}
	if acts[h.heartbeat] != skillinject.RemovalStripped {
		t.Errorf("HEARTBEAT.md: %q, want stripped", acts[h.heartbeat])
	}
	if acts[h.config] != skillinject.RemovalMerged {
		t.Errorf("openclaw.json: %q, want merged", acts[h.config])
	}
	if acts[filepath.Join(h.pluginDir, "index.mjs")] != skillinject.RemovalDeleted {
		t.Errorf("plugin index.mjs: %q, want deleted", acts[filepath.Join(h.pluginDir, "index.mjs")])
	}
	if c := rep.Counts(); c[skillinject.RemovalError] != 0 {
		t.Errorf("errors during uninstall: %+v", rep.Removals)
	}
	assertRetiredCleanedUp(t, h)
	if strings.Contains(mustRead(t, filepath.Join(h.home, ".openclaw", "workspace", "AGENTS.md")), "pilot:begin") {
		t.Error("active AGENTS.md block not stripped")
	}
	// The emptied <skillsDir>/pilotctl/ directory goes too; skillsDir stays.
	mustNotExist(t, filepath.Join(h.home, ".openclaw", "skills", "pilotctl"))
	mustExist(t, filepath.Join(h.home, ".openclaw", "skills"))
}

// Flat-layout tools write <skillsDir>/<entrypoint>.md straight into a
// directory that is not ours; Uninstall must leave that directory even
// when it ends up empty.
func TestUninstall_FlatLayoutKeepsSkillsDir(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".openhands"))
	r := newFakeRepo(t)
	r.withTools([]skillinject.ManifestTool{{
		Name: "openhands", RootDir: "~/.openhands", SkillsDir: "~/.openhands/pilotctl",
		SkillNaming: "flat", SelfHeartbeat: true,
	}})
	cfg := r.cfg(home)
	if _, err := skillinject.Tick(context.Background(), cfg); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	mustExist(t, filepath.Join(home, ".openhands", "pilotctl", "pilotctl.md"))
	if _, err := skillinject.Uninstall(context.Background(), cfg); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	mustNotExist(t, filepath.Join(home, ".openhands", "pilotctl", "pilotctl.md"))
	// Even though the skills dir happens to be named like the entrypoint.
	mustExist(t, filepath.Join(home, ".openhands", "pilotctl"))
}
