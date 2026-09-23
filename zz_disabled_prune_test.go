// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pilot-protocol/skillinject"
)

// offlineCfg points at an address nothing listens on. A disabled tick must
// not fetch, so any network access would surface as an error.
func offlineCfg(home string) skillinject.Config {
	return skillinject.Config{
		Home:        home,
		ManifestURL: "http://127.0.0.1:1/inject-manifest.json",
		RepoBaseURL: "http://127.0.0.1:1/",
	}
}

// A host that ran `pilotctl skills disable all` on a release without the
// retired list still has the prompt-injector trusted and enabled, the stale
// HEARTBEAT.md and AGENT.md blocks, and pilot-ask. After upgrading it sits
// in disabled mode. Tick and ForceTick must still remove those surfaces,
// offline, and report them.
func TestTick_DisabledHostPrunesRetiredSurfaces(t *testing.T) {
	t.Parallel()
	for name, run := range map[string]func(context.Context, skillinject.Config) (*skillinject.Report, error){
		"Tick":      skillinject.Tick,
		"ForceTick": skillinject.ForceTick,
	} {
		run := run
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := seedStaleOpenClaw(t)
			pico := filepath.Join(h.home, ".picoclaw", "workspace", "AGENT.md")
			mustMkdirAll(t, filepath.Dir(pico))
			mustWriteFile(t, pico, "# my agent\n\n<!-- pilot:begin v=1 hash=aaaaaaaaaaaa -->\nold\n<!-- pilot:end -->\n\nmy notes\n", 0o644)
			ask := filepath.Join(h.home, ".pilot", "bin", "pilot-ask")
			mustMkdirAll(t, filepath.Dir(ask))
			mustWriteFile(t, ask, "#!/bin/sh\n", 0o755)
			if err := skillinject.SetMode(h.home, skillinject.ModeDisabled); err != nil {
				t.Fatal(err)
			}

			rep, err := run(context.Background(), offlineCfg(h.home))
			if err != nil {
				t.Fatalf("disabled tick: %v", err)
			}
			if !rep.Disabled {
				t.Fatalf("report not marked disabled: %+v", rep)
			}
			if c := rep.Counts(); c[skillinject.ActionRemove] != 6 || len(rep.Outcomes) != 6 {
				t.Fatalf("want 6 remove rows (2 markers, config, 2 plugin files, helper), got %+v", rep.Outcomes)
			}
			for _, o := range rep.Outcomes {
				if o.State != skillinject.StateRetired {
					t.Errorf("non-retired row in a disabled tick: %+v", o)
				}
			}
			assertRetiredCleanedUp(t, h)
			mustNotExist(t, ask)
			if got := mustRead(t, pico); got != "# my agent\n\nmy notes\n" {
				t.Errorf("AGENT.md not stripped cleanly: %q", got)
			}
			// Nothing was installed: disabled still means no skills.
			mustNotExist(t, filepath.Join(h.home, ".openclaw", "skills"))
			mustNotExist(t, filepath.Join(h.home, ".openclaw", "workspace", "AGENTS.md"))

			// Settled: the next disabled tick has nothing to do.
			rep, err = run(context.Background(), offlineCfg(h.home))
			if err != nil {
				t.Fatalf("second disabled tick: %v", err)
			}
			if len(rep.Outcomes) != 0 {
				t.Fatalf("second disabled tick not empty: %+v", rep.Outcomes)
			}
		})
	}
}

// The disabled prune also honours the "retired" key of the manifest the
// last tick cached, without the network.
func TestTick_DisabledHostUsesCachedManifestRetired(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	old := filepath.Join(home, ".pilot", "bin", "old-helper")
	mustMkdirAll(t, filepath.Dir(old))
	mustWriteFile(t, old, "#!/bin/sh\n", 0o755)
	rules := filepath.Join(home, ".oldtool", "RULES.md")
	mustMkdirAll(t, filepath.Dir(rules))
	mustWriteFile(t, rules, "mine\n\n<!-- pilot:begin v=1 hash=abc -->\nold\n<!-- pilot:end -->\n", 0o644)

	cached, err := json.Marshal(skillinject.Manifest{
		Version: 1, Entrypoint: "pilotctl",
		// An active tool listing the same file does not protect it while
		// disabled: nothing is managed then.
		Tools: []skillinject.ManifestTool{{Name: "oldtool", RootDir: "~/.oldtool", HeartbeatPath: "~/.oldtool/RULES.md"}},
		Retired: &skillinject.ManifestRetired{
			Helpers: []skillinject.ManifestHelper{{Name: "old-helper", Dst: "~/.pilot/bin/old-helper"}},
			Markers: []skillinject.RetiredMarker{{Tool: "oldtool", Path: "~/.oldtool/RULES.md"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(home, ".pilot", "skills-cache", "inject-manifest.json")
	mustMkdirAll(t, filepath.Dir(cache))
	mustWriteFile(t, cache, string(cached), 0o644)
	if err := skillinject.SetMode(home, skillinject.ModeDisabled); err != nil {
		t.Fatal(err)
	}

	rep, err := skillinject.Tick(context.Background(), offlineCfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if c := rep.Counts(); c[skillinject.ActionRemove] != 2 {
		t.Fatalf("want 2 removals, got %+v", rep.Outcomes)
	}
	mustNotExist(t, old)
	if got := mustRead(t, rules); got != "mine\n\n" {
		t.Errorf("RULES.md not stripped: %q", got)
	}
}

// A corrupt cached manifest falls back to the built-in list; the tick
// neither fails nor reaches the network.
func TestTick_DisabledHostWithCorruptCache(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cache := filepath.Join(home, ".pilot", "skills-cache", "inject-manifest.json")
	mustMkdirAll(t, filepath.Dir(cache))
	mustWriteFile(t, cache, "{ nope", 0o644)
	ask := filepath.Join(home, ".pilot", "bin", "pilot-ask")
	mustMkdirAll(t, filepath.Dir(ask))
	mustWriteFile(t, ask, "#!/bin/sh\n", 0o755)
	if err := skillinject.SetMode(home, skillinject.ModeDisabled); err != nil {
		t.Fatal(err)
	}
	rep, err := skillinject.Tick(context.Background(), offlineCfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(rep.Outcomes) != 1 || rep.Outcomes[0].Path != ask || rep.Outcomes[0].Action != skillinject.ActionRemove {
		t.Fatalf("want one removal of pilot-ask, got %+v", rep.Outcomes)
	}
	mustNotExist(t, ask)
}

// A disabled host never gets its active blocks touched by the prune: only
// the retired list is acted on.
func TestTick_DisabledHostLeavesActiveFilesAlone(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	claude := filepath.Join(home, ".claude", "CLAUDE.md")
	mustMkdirAll(t, filepath.Dir(claude))
	body := "# rules\n\n<!-- pilot:begin v=1 hash=abc -->\nx\n<!-- pilot:end -->\n"
	mustWriteFile(t, claude, body, 0o644)
	if err := skillinject.SetMode(home, skillinject.ModeDisabled); err != nil {
		t.Fatal(err)
	}
	rep, err := skillinject.Tick(context.Background(), offlineCfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(rep.Outcomes) != 0 {
		t.Fatalf("want no rows, got %+v", rep.Outcomes)
	}
	if mustRead(t, claude) != body {
		t.Fatal("disabled tick touched an active heartbeat file")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatal("disabled tick installed a skill")
	}
}
