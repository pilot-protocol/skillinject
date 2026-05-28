// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !no_skillinject
// +build !no_skillinject

// Sweep over small uncovered branches across:
//   - manifest.go: fetchManifest validation errors (version, missing
//     entrypoint, no tools); fetcher repoBase auto-slash suffix;
//     renderHeartbeat execute-time error; writeCache mkdir error.
//   - state.go: classifyPluginFile happy paths (identical & drifted).
//   - skillinject.go: logTick nil-report + error paths; reconcile
//     branch where allowlist merge surfaces an error via Outcome.
//   - skillinject.go: Tick disabled-by-config short-circuit returns
//     a Disabled report without any disk/network IO.
//   - skillinject.go: skillTargetPath for "flat" SkillNaming.
//   - reconcile.go: writeFile + writeHelper mkdir/rename happy paths
//     are already covered, so cover the noop-when-identical-and-mode-ok
//     and write the rare case where helper exists with wrong mode but
//     identical content (state=Drifted, mode rewritten).
//   - config.go: SetEnabled handles a pre-existing config that's
//     unparseable garbage (json.Unmarshal error is swallowed; new
//     write succeeds).
//   - plugin_allowlist.go: walkObject with empty path; verifyOnDiskResult
//     when file disappeared between write and read-back.
//
// Every test isolates to t.TempDir() and uses no network.

package skillinject

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fetchManifest: HTTP 200 but body is a manifest with version != 1.
func TestFetchManifest_RejectsWrongVersion(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version": 99, "entrypoint": "x", "tools": [{}]}`))
	}))
	defer srv.Close()
	f := newFetcher(Config{ManifestURL: srv.URL + "/m.json"})
	_, err := f.fetchManifest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unsupported manifest version") {
		t.Errorf("expected version error; got %v", err)
	}
}

// fetchManifest: missing entrypoint.
func TestFetchManifest_RejectsMissingEntrypoint(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version": 1, "tools": [{"name":"x"}]}`))
	}))
	defer srv.Close()
	f := newFetcher(Config{ManifestURL: srv.URL + "/m.json"})
	_, err := f.fetchManifest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "entrypoint") {
		t.Errorf("expected entrypoint error; got %v", err)
	}
}

// fetchManifest: no tools.
func TestFetchManifest_RejectsEmptyTools(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version": 1, "entrypoint":"x", "tools": []}`))
	}))
	defer srv.Close()
	f := newFetcher(Config{ManifestURL: srv.URL + "/m.json"})
	_, err := f.fetchManifest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no tools") {
		t.Errorf("expected no-tools error; got %v", err)
	}
}

// fetchManifest: HTTP non-200 → wrapped error from get().
func TestFetchManifest_HTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	f := newFetcher(Config{ManifestURL: srv.URL + "/m.json"})
	_, err := f.fetchManifest(context.Background())
	if err == nil {
		t.Fatal("expected HTTP error")
	}
}

// newFetcher: RepoBaseURL without trailing slash gets one appended.
func TestNewFetcher_RepoBaseGetsSlash(t *testing.T) {
	t.Parallel()
	f := newFetcher(Config{RepoBaseURL: "https://example.org/repo"})
	if !strings.HasSuffix(f.repoBase, "/") {
		t.Errorf("repoBase = %q; want trailing /", f.repoBase)
	}
}

// renderHeartbeat: template with a field that doesn't exist on
// heartbeatVars triggers Execute error.
func TestRenderHeartbeat_ExecuteError(t *testing.T) {
	t.Parallel()
	// {{.DoesNotExist}} fails at Execute time because the field is
	// missing on heartbeatVars.
	_, err := renderHeartbeat([]byte(`{{.DoesNotExist}}`), heartbeatVars{EntrypointPath: "/x"})
	if err == nil {
		t.Fatal("expected execute error on unknown field")
	}
	if !strings.Contains(err.Error(), "render heartbeat template") {
		t.Errorf("error not wrapped properly: %v", err)
	}
}

// classifyPluginFile happy paths: identical hash, drifted hash.
func TestClassifyPluginFile_AllStates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")

	// Absent.
	if got := classifyPluginFile(p, "anyhash"); got != StateAbsent {
		t.Errorf("missing file classify=%s; want Absent", got)
	}
	// Identical.
	body := []byte("hello")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	wantHash := sha256Hex(body)
	if got := classifyPluginFile(p, wantHash); got != StateIdentical {
		t.Errorf("matching content classify=%s; want Identical", got)
	}
	// Drifted.
	if got := classifyPluginFile(p, "deadbeef"); got != StateDrifted {
		t.Errorf("different hash classify=%s; want Drifted", got)
	}
}

// logTick: nil report short-circuits; logTick(nil, nil) must not panic.
func TestLogTick_NilReportIsSafe(t *testing.T) {
	t.Parallel()
	logTick(nil, nil) // covers `if r == nil { return }`
}

// logTick: error path logs via slog and returns; no panic.
func TestLogTick_ErrorIsSafe(t *testing.T) {
	t.Parallel()
	logTick(nil, context.Canceled)
}

// logTick: non-nil report goes through the Info branch.
func TestLogTick_NonNilReportLogs(t *testing.T) {
	t.Parallel()
	r := &Report{
		Outcomes: []Outcome{
			{Action: ActionNoop}, {Action: ActionCreate}, {Action: ActionRewrite}, {Action: ActionError},
		},
		Skipped: []string{"a", "b"},
	}
	logTick(r, nil)
}

// Tick: opt-out flag set → returns Disabled report, no IO performed.
func TestTick_DisabledReturnsEarly(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := SetEnabled(home, false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	cfg := Config{
		Home: home,
		// Bogus URL — would error if we got past the IsEnabled gate.
		ManifestURL: "http://127.0.0.1:1/m.json",
		RepoBaseURL: "http://127.0.0.1:1/",
	}
	rep, err := Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("expected no error on disabled tick, got %v", err)
	}
	if rep == nil || !rep.Disabled {
		t.Errorf("expected Disabled=true; got %+v", rep)
	}
	if len(rep.Outcomes) != 0 || len(rep.Skipped) != 0 {
		t.Errorf("expected empty report on disabled tick: %+v", rep)
	}
}

// skillTargetPath: "flat" SkillNaming yields a single-file path.
func TestSkillTargetPath_FlatNaming(t *testing.T) {
	t.Parallel()
	mt := ManifestTool{
		Name:        "openhands",
		SkillsDir:   "~/.openhands/microagents",
		SkillNaming: "flat",
	}
	got := skillTargetPath(mt, "pilotctl", "/home/u")
	want := "/home/u/.openhands/microagents/pilotctl.md"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
	// Verify the default "directory" naming still hits the SKILL.md path.
	mt2 := ManifestTool{Name: "claude", SkillsDir: "~/.claude/skills"}
	if got := skillTargetPath(mt2, "pilotctl", "/h"); !strings.HasSuffix(got, "/pilotctl/SKILL.md") {
		t.Errorf("default naming got %q; want SKILL.md suffix", got)
	}
}

// writeHelper: identical content + correct mode → noop, no rewrite.
func TestWriteHelper_IdenticalContentAndModeNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "h")
	content := []byte("body")
	if err := os.WriteFile(p, content, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, err := writeHelper(p, content, 0o755)
	if err != nil {
		t.Fatalf("writeHelper: %v", err)
	}
	if st != StateIdentical {
		t.Errorf("state = %s; want Identical", st)
	}
}

// writeHelper: identical content but wrong mode → Drifted; rewrite restores mode.
func TestWriteHelper_ModeDriftTriggersRewrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "h")
	content := []byte("body")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, err := writeHelper(p, content, 0o755)
	if err != nil {
		t.Fatalf("writeHelper: %v", err)
	}
	if st != StateDrifted {
		t.Errorf("state = %s; want Drifted", st)
	}
	finfo, _ := os.Stat(p)
	if finfo.Mode().Perm() != 0o755 {
		t.Errorf("mode after rewrite = %o; want 0755", finfo.Mode().Perm())
	}
}

// SetEnabled: existing config is unparseable garbage; SetEnabled
// silently overwrites with a fresh map (preserves nothing — the
// json.Unmarshal error is swallowed by design).
func TestSetEnabled_HandlesPreviouslyGarbageConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".pilot")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("seed garbage: %v", err)
	}
	if err := SetEnabled(home, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if IsEnabled(home) {
		t.Errorf("after SetEnabled(false) over garbage, IsEnabled=true; want false")
	}
}

// verifyOnDiskResult: file disappears between write and read → error.
// (Exercises the err-branch in verifyOnDiskResult.)
func TestVerifyOnDiskResult_MissingFileErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "does-not-exist.json")
	err := verifyOnDiskResult(p, map[string]struct{}{}, "pilot", "plugins.allow", "plugins.entries")
	if err == nil {
		t.Fatal("expected error reading missing file")
	}
	if !strings.Contains(err.Error(), "read-back") {
		t.Errorf("error not wrapped properly: %v", err)
	}
}

// reconcilePluginAllowList: noop branch — file already in identical state.
// Verifies that the function early-returns with Action=Noop and never
// calls mergePluginAllowList. This covers the rare cov0 line:
// `if o.Action == ActionNoop { return o }`.
func TestReconcilePluginAllowList_IdenticalNoop(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	openclawDir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(openclawDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(openclawDir, "openclaw.json")
	// Seed in the desired state.
	body := []byte(`{
  "plugins": {
    "allow": ["pilot"],
    "entries": { "pilot": { "enabled": true } }
  }
}
`)
	if err := os.WriteFile(cfgPath, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := &ManifestPlugin{
		ID: "pilot",
		AllowList: &ManifestPluginAllowList{
			ConfigPath:        "~/.openclaw/openclaw.json",
			AllowListJsonPath: "plugins.allow",
			EntriesJsonPath:   "plugins.entries",
		},
	}
	out := reconcilePluginAllowList(p, home)
	if out.Action != ActionNoop {
		t.Errorf("action = %v; want noop on identical state (got outcome %+v)", out.Action, out)
	}
	// File must be byte-identical (no write happened).
	got, _ := os.ReadFile(cfgPath)
	if string(got) != string(body) {
		t.Errorf("file was mutated despite noop classification:\nwant=%q\ngot =%q", body, got)
	}
}

// reconcilePluginAllowList: merge failure surfaces as Outcome.Action=Error
// with Err populated. Trigger by writing malformed JSON into the config
// at a state Drift will mis-classify as needing rewrite (note:
// classifyPluginAllowList returns Drifted on parse failure, then merge
// itself refuses to overwrite and returns an error — caller records it).
func TestReconcilePluginAllowList_MergeErrorSurfaced(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	openclawDir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(openclawDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(openclawDir, "openclaw.json")
	if err := os.WriteFile(cfgPath, []byte("not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := &ManifestPlugin{
		ID: "pilot",
		AllowList: &ManifestPluginAllowList{
			ConfigPath:        "~/.openclaw/openclaw.json",
			AllowListJsonPath: "plugins.allow",
			EntriesJsonPath:   "plugins.entries",
		},
	}
	out := reconcilePluginAllowList(p, home)
	if out.Action != ActionError {
		t.Errorf("action = %v; want error", out.Action)
	}
	if out.Err == "" {
		t.Errorf("expected non-empty Err on merge failure: %+v", out)
	}
}
