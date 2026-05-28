// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !no_skillinject
// +build !no_skillinject

// Additional uninstall.go coverage:
//   - loadManifestForUninstall offline / cached-manifest fallback paths
//   - loadManifestForUninstall total failure (network unreachable + no cache)
//   - removePluginAllowListEntry path matrix:
//       * missing config file with orphan .pilot-bak cleanup
//       * .pilot-bak restore preferred path (RemovalRestored)
//       * inverse-merge path (RemovalMerged)
//       * inverse-merge over an id we never added (RemovalNoop + orphan bak cleanup)
//       * unparseable live config (RemovalError, refuses to write)
//   - stripMarkerFile noop when no marker present
//   - removeOwnedFile noop on already-missing path
//   - Uninstall error surfacing when loadManifestForUninstall fails completely
//
// All file I/O uses t.TempDir().

package skillinject

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadManifestForUninstall: network OK → returns (manifest, offline=false).
// Spin up a tiny fake repo locally, point Config at it, ensure the
// "happy" branch is taken (no cache touched).
func TestLoadManifestForUninstall_NetworkPath(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "inject-manifest.json") {
			m := Manifest{
				Version:    1,
				Entrypoint: "pilotctl",
				Tools: []ManifestTool{{
					Name: "claude-code", RootDir: "~/.claude", SkillsDir: "~/.claude/skills",
				}},
			}
			_ = json.NewEncoder(w).Encode(m)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := Config{
		Home:        home,
		ManifestURL: srv.URL + "/inject-manifest.json",
		RepoBaseURL: srv.URL + "/",
	}
	m, offline, err := loadManifestForUninstall(context.Background(), cfg, home)
	if err != nil {
		t.Fatalf("loadManifestForUninstall: %v", err)
	}
	if offline {
		t.Errorf("offline=true; want false when network reachable")
	}
	if m == nil || m.Entrypoint != "pilotctl" {
		t.Errorf("unexpected manifest: %+v", m)
	}
}

// loadManifestForUninstall: network DOWN, cached manifest on disk →
// returns (manifest, offline=true).
func TestLoadManifestForUninstall_CachedFallback(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	// Seed cached manifest.
	cacheRel := filepath.Join(cacheDir(home), manifestCacheRel)
	if err := os.MkdirAll(filepath.Dir(cacheRel), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	m := Manifest{Version: 1, Entrypoint: "pilotctl", Tools: []ManifestTool{{Name: "x"}}}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(cacheRel, b, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	cfg := Config{
		Home:        home,
		ManifestURL: "http://127.0.0.1:1/inject-manifest.json", // unreachable
		RepoBaseURL: "http://127.0.0.1:1/",
	}
	got, offline, err := loadManifestForUninstall(context.Background(), cfg, home)
	if err != nil {
		t.Fatalf("loadManifestForUninstall: %v", err)
	}
	if !offline {
		t.Errorf("offline=false; want true (used cached fallback)")
	}
	if got == nil || got.Entrypoint != "pilotctl" {
		t.Errorf("manifest not restored from cache: %+v", got)
	}
}

// loadManifestForUninstall: network DOWN, cache MISSING → returns error.
func TestLoadManifestForUninstall_NoCache(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfg := Config{
		Home:        home,
		ManifestURL: "http://127.0.0.1:1/inject-manifest.json",
		RepoBaseURL: "http://127.0.0.1:1/",
	}
	_, _, err := loadManifestForUninstall(context.Background(), cfg, home)
	if err == nil {
		t.Fatal("expected error when network is down and no cache")
	}
	if !strings.Contains(err.Error(), "no cached manifest") {
		t.Errorf("error doesn't mention missing cache: %v", err)
	}
}

// loadManifestForUninstall: cached file is unparseable garbage → error.
func TestLoadManifestForUninstall_CachedGarbage(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cacheRel := filepath.Join(cacheDir(home), manifestCacheRel)
	if err := os.MkdirAll(filepath.Dir(cacheRel), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.WriteFile(cacheRel, []byte("not json {{{"), 0o644); err != nil {
		t.Fatalf("seed garbage: %v", err)
	}
	cfg := Config{
		Home:        home,
		ManifestURL: "http://127.0.0.1:1/inject-manifest.json",
		RepoBaseURL: "http://127.0.0.1:1/",
	}
	_, _, err := loadManifestForUninstall(context.Background(), cfg, home)
	if err == nil || !strings.Contains(err.Error(), "parse cached manifest") {
		t.Errorf("expected parse error; got %v", err)
	}
}

// Uninstall surfaces an error when loadManifestForUninstall fails
// completely (network unreachable + no cache). Empty report returned.
func TestUninstall_PropagatesManifestLoadFailure(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfg := Config{
		Home:        home,
		ManifestURL: "http://127.0.0.1:1/inject-manifest.json",
		RepoBaseURL: "http://127.0.0.1:1/",
	}
	rep, err := Uninstall(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when manifest unfetchable + uncached")
	}
	if rep == nil {
		t.Fatal("expected non-nil empty report alongside error")
	}
	if len(rep.Removals) != 0 {
		t.Errorf("expected no removals on early failure; got %d", len(rep.Removals))
	}
}

// removePluginAllowListEntry: config file gone, no .pilot-bak → noop.
func TestRemovePluginAllowListEntry_ConfigMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")

	p := &ManifestPlugin{
		ID: "pilot",
		AllowList: &ManifestPluginAllowList{
			ConfigPath:        cfgPath,
			AllowListJsonPath: "plugins.allow",
			EntriesJsonPath:   "plugins.entries",
		},
	}
	r := removePluginAllowListEntry(p, cfgPath)
	if r.Action != RemovalNoop {
		t.Errorf("action = %v, want noop; %+v", r.Action, r)
	}
}

// removePluginAllowListEntry: config gone but orphan .pilot-bak present
// → still noop, AND the orphan backup is cleaned up.
func TestRemovePluginAllowListEntry_OrphanBakCleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(cfgPath+BackupSuffix, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed orphan bak: %v", err)
	}

	p := &ManifestPlugin{
		ID: "pilot",
		AllowList: &ManifestPluginAllowList{
			ConfigPath:        cfgPath,
			AllowListJsonPath: "plugins.allow",
			EntriesJsonPath:   "plugins.entries",
		},
	}
	r := removePluginAllowListEntry(p, cfgPath)
	if r.Action != RemovalNoop {
		t.Errorf("action = %v, want noop", r.Action)
	}
	if _, err := os.Stat(cfgPath + BackupSuffix); err == nil {
		t.Errorf("orphan .pilot-bak should have been removed")
	}
}

// removePluginAllowListEntry: .pilot-bak is the pre-install snapshot and
// the live file has our id merged in → RESTORE from backup, return
// RemovalRestored, delete the .pilot-bak.
func TestRemovePluginAllowListEntry_RestoreFromBackup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")

	// Backup: original config without pilot.
	bak := map[string]any{
		"gateway": map[string]any{"mode": "auto"},
		"plugins": map[string]any{
			"allow":   []any{"other-plugin"},
			"entries": map[string]any{"other-plugin": map[string]any{"enabled": true}},
		},
	}
	bakBytes, _ := json.MarshalIndent(bak, "", "  ")
	bakBytes = append(bakBytes, '\n')
	if err := os.WriteFile(cfgPath+BackupSuffix, bakBytes, 0o600); err != nil {
		t.Fatalf("seed bak: %v", err)
	}

	// Live: config WITH pilot merged in.
	live := map[string]any{
		"gateway": map[string]any{"mode": "auto"},
		"plugins": map[string]any{
			"allow": []any{"other-plugin", "pilot"},
			"entries": map[string]any{
				"other-plugin": map[string]any{"enabled": true},
				"pilot":        map[string]any{"enabled": true},
			},
		},
	}
	liveBytes, _ := json.MarshalIndent(live, "", "  ")
	if err := os.WriteFile(cfgPath, liveBytes, 0o644); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	p := &ManifestPlugin{
		ID: "pilot",
		AllowList: &ManifestPluginAllowList{
			ConfigPath:        cfgPath,
			AllowListJsonPath: "plugins.allow",
			EntriesJsonPath:   "plugins.entries",
		},
	}
	r := removePluginAllowListEntry(p, cfgPath)
	if r.Action != RemovalRestored {
		t.Errorf("action = %v, want RemovalRestored; %+v", r.Action, r)
	}
	// Live file should now equal the backup bytes byte-for-byte.
	got, _ := os.ReadFile(cfgPath)
	if string(got) != string(bakBytes) {
		t.Errorf("file content not byte-restored:\nwant=%q\ngot =%q", bakBytes, got)
	}
	// Backup should have been removed after restore.
	if _, err := os.Stat(cfgPath + BackupSuffix); err == nil {
		t.Errorf(".pilot-bak should be removed after restore")
	}
}

// removePluginAllowListEntry: .pilot-bak somehow itself contains our id
// (invalid as a "pre-install" snapshot) → restore is skipped, falls
// through to inverse-merge path. End state: id removed, action=Merged.
func TestRemovePluginAllowListEntry_BakContainsOurIdFallsToInverseMerge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")

	// Bogus "backup" that actually contains pilot.
	bogus := map[string]any{
		"plugins": map[string]any{
			"allow":   []any{"pilot"},
			"entries": map[string]any{"pilot": map[string]any{"enabled": true}},
		},
	}
	bb, _ := json.MarshalIndent(bogus, "", "  ")
	if err := os.WriteFile(cfgPath+BackupSuffix, bb, 0o600); err != nil {
		t.Fatalf("seed bogus bak: %v", err)
	}
	// Live file with pilot.
	live := map[string]any{
		"plugins": map[string]any{
			"allow":   []any{"user-x", "pilot"},
			"entries": map[string]any{"user-x": map[string]any{"enabled": true}, "pilot": map[string]any{"enabled": true}},
		},
	}
	lb, _ := json.MarshalIndent(live, "", "  ")
	if err := os.WriteFile(cfgPath, lb, 0o644); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	p := &ManifestPlugin{
		ID: "pilot",
		AllowList: &ManifestPluginAllowList{
			ConfigPath:        cfgPath,
			AllowListJsonPath: "plugins.allow",
			EntriesJsonPath:   "plugins.entries",
		},
	}
	r := removePluginAllowListEntry(p, cfgPath)
	if r.Action != RemovalMerged {
		t.Errorf("action = %v, want RemovalMerged (inverse-merge fallback)", r.Action)
	}
	// User entry preserved.
	got, _ := os.ReadFile(cfgPath)
	var obj map[string]any
	_ = json.Unmarshal(got, &obj)
	plugins := obj["plugins"].(map[string]any)
	entries := plugins["entries"].(map[string]any)
	if _, ok := entries["pilot"]; ok {
		t.Errorf("pilot entry not removed: %+v", entries)
	}
	if _, ok := entries["user-x"]; !ok {
		t.Errorf("user-x entry lost: %+v", entries)
	}
}

// removePluginAllowListEntry: config parseable, no backup, our id not
// present → noop (nothing to remove). Pre-existing orphan .pilot-bak
// (if any) gets cleaned up too.
func TestRemovePluginAllowListEntry_NoIdPresentNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")
	live := map[string]any{
		"plugins": map[string]any{
			"allow":   []any{"user-a"},
			"entries": map[string]any{"user-a": map[string]any{"enabled": true}},
		},
	}
	lb, _ := json.MarshalIndent(live, "", "  ")
	if err := os.WriteFile(cfgPath, lb, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Orphan backup that is UNPARSEABLE — the restore preference check
	// then bails out (json.Unmarshal returns error), forcing the path
	// through the inverse-merge logic, which finds no id and noops.
	if err := os.WriteFile(cfgPath+BackupSuffix, []byte("garbage{{"), 0o600); err != nil {
		t.Fatalf("seed orphan bak: %v", err)
	}

	p := &ManifestPlugin{
		ID: "pilot",
		AllowList: &ManifestPluginAllowList{
			ConfigPath:        cfgPath,
			AllowListJsonPath: "plugins.allow",
			EntriesJsonPath:   "plugins.entries",
		},
	}
	r := removePluginAllowListEntry(p, cfgPath)
	if r.Action != RemovalNoop {
		t.Errorf("action = %v, want noop (id not present)", r.Action)
	}
	if _, err := os.Stat(cfgPath + BackupSuffix); err == nil {
		t.Errorf("orphan .pilot-bak should have been removed")
	}
}

// removePluginAllowListEntry: live config is malformed JSON → RemovalError;
// the file must NOT be overwritten.
func TestRemovePluginAllowListEntry_MalformedConfigRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")
	garbage := []byte("not json { ]")
	if err := os.WriteFile(cfgPath, garbage, 0o644); err != nil {
		t.Fatalf("seed garbage: %v", err)
	}

	p := &ManifestPlugin{
		ID: "pilot",
		AllowList: &ManifestPluginAllowList{
			ConfigPath:        cfgPath,
			AllowListJsonPath: "plugins.allow",
			EntriesJsonPath:   "plugins.entries",
		},
	}
	r := removePluginAllowListEntry(p, cfgPath)
	if r.Action != RemovalError {
		t.Errorf("action = %v, want RemovalError on malformed config", r.Action)
	}
	got, _ := os.ReadFile(cfgPath)
	if string(got) != string(garbage) {
		t.Errorf("malformed config was overwritten:\nwant=%q\ngot =%q", garbage, got)
	}
}

// stripMarkerFile: file exists but contains no marker block → noop,
// content untouched.
func TestStripMarkerFile_NoMarkerNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "AGENT.md")
	content := []byte("# Plain file\n\nno marker here.\n")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := stripMarkerFile("my-tool", p)
	if r.Action != RemovalNoop {
		t.Errorf("action = %v, want noop", r.Action)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(content) {
		t.Errorf("file mutated despite noop: %q", got)
	}
}

// stripMarkerFile: file missing → noop with no error.
func TestStripMarkerFile_MissingFileNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := stripMarkerFile("my-tool", filepath.Join(dir, "does-not-exist.md"))
	if r.Action != RemovalNoop {
		t.Errorf("action = %v, want noop on missing file", r.Action)
	}
}

// removeOwnedFile: file already missing → noop.
func TestRemoveOwnedFile_MissingNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := removeOwnedFile("tool", KindHelper, filepath.Join(dir, "gone"))
	if r.Action != RemovalNoop {
		t.Errorf("action = %v, want noop", r.Action)
	}
}

// removeOwnedFile: file exists, removable → deleted.
func TestRemoveOwnedFile_Deletes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := removeOwnedFile("tool", KindHelper, p)
	if r.Action != RemovalDeleted {
		t.Errorf("action = %v, want deleted", r.Action)
	}
	if _, err := os.Stat(p); err == nil {
		t.Errorf("file should be gone")
	}
}

// stripMarkerFile: path is a directory (not a file) → ReadFile returns
// a non-IsNotExist error. Reports RemovalError.
func TestStripMarkerFile_PathIsDirectoryErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // directory itself
	r := stripMarkerFile("tool", dir)
	if r.Action != RemovalError {
		t.Errorf("action = %v, want RemovalError on dir-as-path", r.Action)
	}
	if r.Err == "" {
		t.Errorf("expected non-empty Err on error")
	}
}

// removeOwnedFile: path is a non-empty directory → os.Remove returns
// an error. Reports RemovalError.
func TestRemoveOwnedFile_NonEmptyDirErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create child file so os.Remove(dir) fails with "directory not empty".
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := removeOwnedFile("tool", KindHelper, dir)
	if r.Action != RemovalError {
		t.Errorf("action = %v, want RemovalError on non-empty dir", r.Action)
	}
	if r.Err == "" {
		t.Errorf("expected non-empty Err")
	}
}

// removePluginAllowListEntry: cfgPath is a directory (non-IsNotExist
// read error) → RemovalError.
func TestRemovePluginAllowListEntry_PathIsDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &ManifestPlugin{
		ID: "pilot",
		AllowList: &ManifestPluginAllowList{
			ConfigPath:        dir, // directory, not a file
			AllowListJsonPath: "plugins.allow",
			EntriesJsonPath:   "plugins.entries",
		},
	}
	r := removePluginAllowListEntry(p, dir)
	if r.Action != RemovalError {
		t.Errorf("action = %v, want RemovalError on dir-as-config", r.Action)
	}
}

// Uninstall: openclaw end-to-end "restore from backup" path.
// Seed a live openclaw.json + a pre-install .pilot-bak, run Uninstall,
// verify the live file is restored byte-for-byte AND the report carries
// a RemovalRestored row.
func TestUninstall_OpenClawRestoredFromBackup(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	openclawDir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(openclawDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	configPath := filepath.Join(openclawDir, "openclaw.json")

	// Pre-install snapshot (the .pilot-bak): NO pilot id.
	bak := map[string]any{
		"plugins": map[string]any{
			"allow":   []any{"user-a"},
			"entries": map[string]any{"user-a": map[string]any{"enabled": true}},
		},
		"user_setting": "preserve-me",
	}
	bakBytes, _ := json.MarshalIndent(bak, "", "  ")
	bakBytes = append(bakBytes, '\n')
	if err := os.WriteFile(configPath+BackupSuffix, bakBytes, 0o600); err != nil {
		t.Fatalf("seed bak: %v", err)
	}
	// Live: has pilot merged in.
	live := map[string]any{
		"plugins": map[string]any{
			"allow":   []any{"user-a", "pilot"},
			"entries": map[string]any{"user-a": map[string]any{"enabled": true}, "pilot": map[string]any{"enabled": true}},
		},
		"user_setting": "preserve-me",
	}
	liveBytes, _ := json.MarshalIndent(live, "", "  ")
	if err := os.WriteFile(configPath, liveBytes, 0o644); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	// Seed a minimal cached manifest so Uninstall can run with no network.
	m := Manifest{
		Version:    1,
		Entrypoint: "pilotctl",
		Tools: []ManifestTool{{
			Name: "openclaw", RootDir: "~/.openclaw", SkillsDir: "~/.openclaw/skills",
			Plugin: &ManifestPlugin{
				ID:          "pilot",
				InstallPath: "~/.openclaw/extensions/pilot",
				AllowList: &ManifestPluginAllowList{
					ConfigPath:        "~/.openclaw/openclaw.json",
					AllowListJsonPath: "plugins.allow",
					EntriesJsonPath:   "plugins.entries",
				},
			},
		}},
	}
	cacheRel := filepath.Join(cacheDir(home), manifestCacheRel)
	if err := os.MkdirAll(filepath.Dir(cacheRel), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	mb, _ := json.Marshal(m)
	if err := os.WriteFile(cacheRel, mb, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	cfg := Config{
		Home:        home,
		ManifestURL: "http://127.0.0.1:1/inject-manifest.json", // unreachable
		RepoBaseURL: "http://127.0.0.1:1/",
	}
	rep, err := Uninstall(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !rep.ManifestOffline {
		t.Errorf("ManifestOffline=false; want true (cache used)")
	}
	var sawRestored bool
	for _, rm := range rep.Removals {
		if rm.Action == RemovalRestored && rm.Kind == KindPluginAllowList {
			sawRestored = true
		}
	}
	if !sawRestored {
		t.Errorf("expected a RemovalRestored row in report: %+v", rep.Removals)
	}
	got, _ := os.ReadFile(configPath)
	if string(got) != string(bakBytes) {
		t.Errorf("live file not byte-restored from backup:\nwant=%q\ngot =%q", bakBytes, got)
	}
}
