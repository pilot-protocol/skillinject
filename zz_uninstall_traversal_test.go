// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pilot-protocol/skillinject"
)

// Uninstall walks plugin file names straight from the manifest. Both the
// install and the disable path must agree on which names are in scope:
// a name that resolves outside the plugin install dir is reported, not
// acted on. These tests pin that symmetry.

// traversalManifest builds a manifest whose plugin declares one in-scope
// file plus one name that climbs out of the install directory.
func traversalManifest(r *fakeRepo) {
	r.manifest = skillinject.Manifest{
		Version:    1,
		Entrypoint: "pilotctl",
		Tools: []skillinject.ManifestTool{{
			Name:              "openclaw",
			RootDir:           "~/.openclaw",
			SkillsDir:         "~/.openclaw/skills",
			HeartbeatPath:     "~/.openclaw/workspace/AGENTS.md",
			HeartbeatTemplate: "heartbeats/openclaw.md",
			Plugin: &skillinject.ManifestPlugin{
				ID:          "pilot",
				InstallPath: "~/.openclaw/extensions/pilotprotocol",
				Files: []skillinject.ManifestPluginFile{
					{Name: "index.mjs", Src: "plugin/index.mjs"},
					{Name: "../../../.ssh/authorized_keys", Src: "plugin/index.mjs"},
				},
			},
		}},
	}
	r.files["skills/pilotctl/SKILL.md"] = []byte(testContent)
	r.files["heartbeats/openclaw.md"] = []byte(testHeartbeat)
	r.files["plugin/index.mjs"] = []byte("// pilot plugin\n")
}

// TestUninstall_PluginFileEscapingInstallDirIsNotRemoved seeds a file
// outside the plugin install dir at the location an escaping manifest
// name resolves to, then runs Uninstall and asserts the file survives
// and the removal is reported as an error.
func TestUninstall_PluginFileEscapingInstallDirIsNotRemoved(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	outside := filepath.Join(home, ".ssh", "authorized_keys")
	mustMkdirAll(t, filepath.Dir(outside))
	const sentinel = "ssh-ed25519 AAAA user@host\n"
	if err := os.WriteFile(outside, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}

	r := newFakeRepo(t)
	traversalManifest(r)

	rep, err := skillinject.Uninstall(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("file outside install dir was removed by Uninstall: %v", err)
	}
	if string(got) != sentinel {
		t.Fatalf("file outside install dir was modified\nwant=%q\ngot =%q", sentinel, got)
	}

	var sawError bool
	for _, rm := range rep.Removals {
		if rm.Kind == skillinject.KindPluginFile && rm.Action == skillinject.RemovalError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("expected a RemovalError for the escaping plugin file, got %+v", rep.Removals)
	}
}

// TestUninstall_InScopePluginFileStillRemoved guards against the guard
// being too broad: a normal in-dir plugin file must still be deleted.
func TestUninstall_InScopePluginFileStillRemoved(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	r := newFakeRepo(t)
	traversalManifest(r)

	installed := filepath.Join(home, ".openclaw", "extensions", "pilotprotocol", "index.mjs")
	mustMkdirAll(t, filepath.Dir(installed))
	if err := os.WriteFile(installed, []byte("// pilot plugin\n"), 0o644); err != nil {
		t.Fatalf("seed plugin file: %v", err)
	}

	rep, err := skillinject.Uninstall(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatalf("in-scope plugin file was not removed: stat err=%v", err)
	}

	var sawDeleted bool
	for _, rm := range rep.Removals {
		if rm.Path == installed && rm.Action == skillinject.RemovalDeleted {
			sawDeleted = true
		}
	}
	if !sawDeleted {
		t.Fatalf("expected RemovalDeleted for %s, got %+v", installed, rep.Removals)
	}
}

// TestReconcile_PluginFileEscapingInstallDirIsNotWritten mirrors the
// disable-path test on the install side.
func TestReconcile_PluginFileEscapingInstallDirIsNotWritten(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	r := newFakeRepo(t)
	traversalManifest(r)

	if _, err := skillinject.Tick(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	outside := filepath.Join(home, ".ssh", "authorized_keys")
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("install wrote outside the plugin install dir: stat err=%v", err)
	}
}
