// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !no_skillinject
// +build !no_skillinject

// Targets the hardest-to-reach disk error branches by leveraging
// permission denial (chmod 0500), un-stat-able paths, and config-file
// targets that are not regular files.
//
//   - mergePluginAllowList: parent-dir mkdir failure surfaces error
//     (config path under a non-writable ancestor that's a regular file)
//   - mergePluginAllowList: writeFileAtomic backup failure (bakPath
//     points into a non-writable dir while live file is in a writable
//     sibling — we wedge by chmod 0500 on the parent)
//   - mergePluginAllowList: read plugin config returns non-IsNotExist
//     error (config path is a directory, not a file)
//   - mergePluginAllowList: writeFileAtomic for the live config swap
//     fails (parent chmod 0500)
//   - mergePluginAllowList: post-swap verifyOnDiskResult rollback path
//     (covered by directly invoking verifyOnDiskResult + writeFileAtomic
//     in a unit-level integration that mirrors the rollback machinery)
//   - writeFile: mkdir failure (parent is a regular file)
//   - writeMarker: ReadFile non-IsNotExist error (path is a dir)
//   - SetEnabled: mkdir failure when parent is a file
//   - writeCache: mkdir failure when parent is a file
//   - get(): http.NewRequestWithContext fails on a control-character URL
//
// We skip permission-based tests when running as root since chmod can't
// restrict root.

package skillinject

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission-based test requires non-root euid")
	}
}

// writeFile: parent is a REGULAR FILE → MkdirAll returns ENOTDIR. We
// pick a path whose dirname is an existing regular file, which makes
// os.MkdirAll fail with "not a directory" without needing chmod tricks.
func TestWriteFile_MkdirFailsWhenParentIsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create a regular file, then ask writeFile to put a child under it.
	parentAsFile := filepath.Join(dir, "iam-a-file")
	if err := os.WriteFile(parentAsFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := writeFile(filepath.Join(parentAsFile, "child"), []byte("body"))
	if err == nil {
		t.Fatal("expected mkdir error when parent is a regular file")
	}
}

// writeMarker: passing a path whose dirname is a regular file causes
// the post-read mkdir to fail (and is well-defined cross-platform).
func TestWriteMarker_MkdirFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parentAsFile := filepath.Join(dir, "x")
	if err := os.WriteFile(parentAsFile, []byte(""), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Path is under a regular file → ReadFile returns ENOTDIR (a
	// non-IsNotExist error), so writeMarker returns immediately.
	err := writeMarker(filepath.Join(parentAsFile, "AGENT.md"), "ref", "abc123")
	if err == nil {
		t.Fatal("expected error reading or making path under a file")
	}
}

// mergePluginAllowList: parent of config path is a regular file →
// mkdir fails inside merge. Note: the function first tries to ReadFile
// the configPath, which will return ENOTDIR (path traversal hit a
// non-dir); since that's not IsNotExist, merge returns
// "read plugin config" error early.
func TestMergePluginAllowList_ReadPluginConfigErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parentAsFile := filepath.Join(dir, "iam-a-file")
	if err := os.WriteFile(parentAsFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfgPath := filepath.Join(parentAsFile, "openclaw.json")
	err := mergePluginAllowList(cfgPath, "plugins.allow", "plugins.entries", "pilot")
	if err == nil {
		t.Fatal("expected error reading config path under a file")
	}
	if !strings.Contains(err.Error(), "read plugin config") {
		t.Errorf("error not wrapped as expected: %v", err)
	}
}

// mergePluginAllowList: config exists but parent dir becomes read-only
// AFTER the read. Hard to do without monkey patching; instead, we
// exercise the writeFileAtomic-fails branch by making the temp file
// path uncreatable. We chmod the parent dir to 0o500 (read+exec only)
// — file writes inside will fail, the backup write fails first.
func TestMergePluginAllowList_BackupWriteFailsWhenDirReadOnly(t *testing.T) {
	t.Parallel()
	skipIfRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based test not applicable on windows")
	}
	dir := t.TempDir()
	// Seed config inside `dir`.
	cfgPath := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(cfgPath, []byte(`{"x": 1}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Make the directory read-only (no write/create).
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := mergePluginAllowList(cfgPath, "plugins.allow", "plugins.entries", "pilot")
	if err == nil {
		t.Fatal("expected merge error when parent dir is read-only")
	}
	// Should mention either backup or write — both are reasonable failure points.
	msg := err.Error()
	if !strings.Contains(msg, "backup") && !strings.Contains(msg, "write plugin config") {
		t.Errorf("error not about backup/write: %v", err)
	}
}

// SetEnabled: parent of ~/.pilot is a regular file → mkdir fails.
func TestSetEnabled_MkdirFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Put a regular file at the slot where the ~/.pilot dir would live.
	pilotSlot := filepath.Join(dir, ".pilot")
	if err := os.WriteFile(pilotSlot, []byte("nope"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Now SetEnabled wants to create ~/.pilot/ but it's a file.
	// os.MkdirAll on an existing regular file returns an error.
	err := SetEnabled(dir, false)
	if err == nil {
		t.Fatal("expected SetEnabled to fail when ~/.pilot is a regular file")
	}
}

// writeCache: parent of the cache slot is a regular file → mkdir fails
// inside writeCache. Verifies the err-return branch.
func TestWriteCache_MkdirFails(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// Block the cache root by putting a regular file at .pilot.
	if err := os.WriteFile(filepath.Join(home, ".pilot"), []byte(""), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := writeCache(home, "some/relative.json", []byte("x"))
	if err == nil {
		t.Fatal("expected mkdir error in writeCache")
	}
}

// get(): URL with embedded control character makes
// http.NewRequestWithContext fail before any IO happens. Covers the
// `if err != nil { return nil, err }` branch in get().
func TestGet_NewRequestFails(t *testing.T) {
	t.Parallel()
	f := newFetcher(Config{})
	// A control-char (0x7f DEL) in the URL is rejected by net/url
	// inside NewRequestWithContext.
	_, err := f.get(context.Background(), "http://example.com/\x7fbad")
	if err == nil {
		t.Fatal("expected NewRequestWithContext error")
	}
}

// reconcilePluginAllowList: when merge fails on a malformed config,
// already covered. Here we cover the symmetric case where merge fails
// AT the disk level (parent read-only) and the error surfaces through
// the Outcome. This exercises the line that records the err.Error()
// into Outcome.Err.
func TestReconcilePluginAllowList_DiskErrorSurfacesAsOutcome(t *testing.T) {
	t.Parallel()
	skipIfRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based test not applicable on windows")
	}
	home := t.TempDir()
	openclawDir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(openclawDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(openclawDir, "openclaw.json")
	// Seed in a Drifted state so the merge path is taken.
	if err := os.WriteFile(cfgPath, []byte(`{"unrelated": true}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Lock the dir so the backup + swap fail.
	if err := os.Chmod(openclawDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(openclawDir, 0o700) })

	p := &ManifestPlugin{
		ID: "pilot",
		AllowList: &ManifestPluginAllowList{
			ConfigPath:        "~/.openclaw/openclaw.json",
			AllowListJsonPath: "plugins.allow",
			EntriesJsonPath:   "plugins.entries",
		},
	}
	out := reconcilePluginAllowList(p, home, false)
	if out.Action != ActionError {
		t.Errorf("action = %v; want error", out.Action)
	}
	if out.Err == "" {
		t.Errorf("expected non-empty Err: %+v", out)
	}
}

// Direct unit on rollback machinery: writeFileAtomic + verifyOnDiskResult
// together — this exercises the same combo used in the rollback branch
// of mergePluginAllowList (writeFileAtomic from in-memory originalBytes).
// We seed a valid file, run verify (passes), then overwrite with garbage
// and verify (fails), then writeFileAtomic the original bytes back
// (recovers the file), then verify (passes). Mirrors the rollback step
// in mergePluginAllowList without monkey-patching.
func TestRollbackMachinery_RestoreFromInMemorySnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")
	originalBytes := []byte(`{
  "plugins": {
    "allow": ["pilot"],
    "entries": { "pilot": { "enabled": true } }
  }
}
`)
	if err := os.WriteFile(cfgPath, originalBytes, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	originalTopKeys := map[string]struct{}{"plugins": {}}
	// Step 1: verify passes.
	if err := verifyOnDiskResult(cfgPath, originalTopKeys, "pilot", "plugins.allow", "plugins.entries"); err != nil {
		t.Fatalf("baseline verify: %v", err)
	}
	// Step 2: corrupt; verify fails.
	if err := os.WriteFile(cfgPath, []byte("scrambled garbage"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if err := verifyOnDiskResult(cfgPath, originalTopKeys, "pilot", "plugins.allow", "plugins.entries"); err == nil {
		t.Fatal("verify should fail on corrupted bytes")
	}
	// Step 3: rollback via writeFileAtomic (same call merge() uses).
	if err := writeFileAtomic(cfgPath, originalBytes, 0o644); err != nil {
		t.Fatalf("rollback writeFileAtomic: %v", err)
	}
	// Step 4: post-rollback verify passes again.
	if err := verifyOnDiskResult(cfgPath, originalTopKeys, "pilot", "plugins.allow", "plugins.entries"); err != nil {
		t.Errorf("post-rollback verify: %v", err)
	}
	got, _ := os.ReadFile(cfgPath)
	if string(got) != string(originalBytes) {
		t.Errorf("rollback file content differs from original")
	}
}

// Round-out reconcilePluginFiles: helper-fetch returns 404 → error
// outcome (one of two cov0 lines in reconcilePluginFiles, the other
// being the unreachable writeFile failure).
func TestReconcilePluginFiles_FetchErrorSurfaces(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always 404 — every fetchRepoFile call errors out.
		http.NotFound(w, r)
	}))
	defer srv.Close()
	f := newFetcher(Config{RepoBaseURL: srv.URL + "/"})

	p := &ManifestPlugin{
		ID:          "pilot",
		InstallPath: "~/.openclaw/extensions/pilot",
		Files: []ManifestPluginFile{
			{Name: "openclaw.plugin.json", Src: "plugin/openclaw.plugin.json"},
		},
	}
	outs := reconcilePluginFiles(f, context.Background(), p, home, false)
	if len(outs) != 1 {
		t.Fatalf("expected 1 outcome; got %d", len(outs))
	}
	if outs[0].Action != ActionError {
		t.Errorf("action = %v; want error on 404 fetch", outs[0].Action)
	}
	if outs[0].Err == "" {
		t.Errorf("expected non-empty Err")
	}
}

// Tick: heartbeat template fetch fails (404) → marker outcome carries
// ActionError, doesn't block other tools.
func TestTick_HeartbeatTemplateFetchFails(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		switch path {
		case "inject-manifest.json":
			_, _ = w.Write([]byte(`{"version":1,"entrypoint":"pilotctl","tools":[
  {"name":"claude-code","rootDir":"~/.claude","skillsDir":"~/.claude/skills",
   "heartbeatPath":"~/.claude/CLAUDE.md","heartbeatTemplate":"heartbeats/MISSING.md"}
]}`))
		case "skills/pilotctl/SKILL.md":
			_, _ = w.Write([]byte("# skill body"))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{
		Home:        home,
		ManifestURL: srv.URL + "/inject-manifest.json",
		RepoBaseURL: srv.URL + "/",
	}
	rep, err := Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// Should have one marker error outcome (the heartbeat fetch failed).
	var markerErr bool
	for _, o := range rep.Outcomes {
		if o.Kind == KindMarker && o.Action == ActionError {
			markerErr = true
		}
	}
	if !markerErr {
		t.Errorf("expected marker error outcome on failed heartbeat fetch: %+v", rep.Outcomes)
	}
}

// Tick: entrypoint SKILL.md fetch fails → Tick returns an error.
func TestTick_EntrypointSkillFetchFails(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "inject-manifest.json") {
			_, _ = w.Write([]byte(`{"version":1,"entrypoint":"pilotctl","tools":[
  {"name":"claude-code","rootDir":"~/.claude","skillsDir":"~/.claude/skills"}
]}`))
			return
		}
		// Every other fetch (incl. skills/pilotctl/SKILL.md) 404s.
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{
		Home:        home,
		ManifestURL: srv.URL + "/inject-manifest.json",
		RepoBaseURL: srv.URL + "/",
	}
	_, err := Tick(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Tick error when entrypoint SKILL.md 404s")
	}
	if !strings.Contains(err.Error(), "fetch entrypoint") {
		t.Errorf("error not wrapped: %v", err)
	}
}

// Tick with a manifest that points heartbeatTemplate at a template
// containing an Execute-time error (referencing an unknown field).
// Covers the renderHeartbeat error branch inside Tick.
func TestTick_HeartbeatTemplateRenderError(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		switch path {
		case "inject-manifest.json":
			_, _ = w.Write([]byte(`{"version":1,"entrypoint":"pilotctl","tools":[
  {"name":"claude-code","rootDir":"~/.claude","skillsDir":"~/.claude/skills",
   "heartbeatPath":"~/.claude/CLAUDE.md","heartbeatTemplate":"heartbeats/bad.md"}
]}`))
		case "skills/pilotctl/SKILL.md":
			_, _ = w.Write([]byte("# skill"))
		case "heartbeats/bad.md":
			// Field doesn't exist on heartbeatVars → Execute error.
			_, _ = w.Write([]byte(`{{.NonExistent}}`))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{
		Home:        home,
		ManifestURL: srv.URL + "/inject-manifest.json",
		RepoBaseURL: srv.URL + "/",
	}
	rep, err := Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	var sawErr bool
	for _, o := range rep.Outcomes {
		if o.Kind == KindMarker && o.Action == ActionError {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("expected marker render error outcome: %+v", rep.Outcomes)
	}
}
