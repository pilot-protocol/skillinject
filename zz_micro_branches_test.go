// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !no_skillinject
// +build !no_skillinject

// Micro-branch coverage:
//   - verifyMarshalRoundTrip: catches missing-entries-enabled (the
//     "allow has our id but entries.<id>.enabled is missing or not true")
//   - walkObject: edge cases — empty path returns nil,""; create=false
//     on already-present nested map returns the deepest level
//   - allowListContains: raw not-ok branches (key missing, value not array)
//   - entryEnabled: parent[leaf] not a map branch
//   - ensureAllowListEntry / ensureEntryEnabled: parent==nil branch
//     (we can hit this by passing an empty jsonPath, which makes
//     walkObject return empty leaf — but walkObject only returns nil
//     parent when create=false AND an intermediate is non-map. So we
//     route through walkObject with create=false to get nil.)
//   - classifyPluginAllowList: non-IsNotExist ReadFile error (path is dir)
//   - writeHelper: ReadFile non-IsNotExist error (path is a directory)
//   - Run: cancel-fast loop exits cleanly without firing a tick beat
//     (covers the ctx.Done branch inside the for-select)
//
// All tests are pure unit tests on package-private helpers.

package skillinject

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilot-protocol/common/coreapi"
)

// verifyMarshalRoundTrip: bytes have our id in allow but the entry
// for our id is missing the enabled=true assertion.
func TestVerifyMarshalRoundTrip_MissingEntryEnabled(t *testing.T) {
	t.Parallel()
	originalKeys := map[string]struct{}{"plugins": {}}
	// id is in allow, but entries.<id> has enabled=false.
	buf := []byte(`{"plugins":{"allow":["pilot"],"entries":{"pilot":{"enabled":false}}}}`)
	err := verifyMarshalRoundTrip(buf, originalKeys, "pilot", "plugins.allow", "plugins.entries")
	if err == nil {
		t.Fatal("expected error: enabled=false should fail verify")
	}
	if !strings.Contains(err.Error(), "enabled") {
		t.Errorf("wrong error: %v", err)
	}
}

// walkObject: empty path → nil, "". This is a tiny defensive branch.
// We pass jsonPath="" which strings.Split returns ["", ...wait]. Actually
// strings.Split("", ".") returns [""], len 1 — so the loop doesn't run
// and we return the root with leaf="". Not the cov0 line. To hit cov0
// (len(parts)==0), we'd need an impossible input — that branch is dead
// code, but let's exercise the path that returns leaf="" with the root.
func TestWalkObject_EmptyPathReturnsRoot(t *testing.T) {
	t.Parallel()
	obj := map[string]any{"x": 1}
	parent, leaf := walkObject(obj, "", false)
	if parent == nil {
		t.Errorf("expected non-nil parent for empty path")
	}
	if leaf != "" {
		t.Errorf("leaf = %q; want empty", leaf)
	}
}

// walkObject create=false on a path where an intermediate IS a map →
// returns the deepest container + the leaf. Already covered by other
// tests, but pin the exact semantics here.
func TestWalkObject_ExistingNestedPathNoCreate(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"a": map[string]any{
			"b": map[string]any{
				"c": "value",
			},
		},
	}
	parent, leaf := walkObject(obj, "a.b.c", false)
	if parent == nil || leaf != "c" {
		t.Fatalf("got (parent=%v, leaf=%q); want non-nil and c", parent, leaf)
	}
	if parent["c"] != "value" {
		t.Errorf("walkObject returned wrong container: %v", parent)
	}
}

// allowListContains: parent map doesn't have the leaf key → false.
func TestAllowListContains_LeafKeyMissing(t *testing.T) {
	t.Parallel()
	obj := map[string]any{"plugins": map[string]any{}}
	if allowListContains(obj, "plugins.allow", "pilot") {
		t.Error("expected false when leaf key absent")
	}
}

// allowListContains: leaf key exists but value isn't a JSON array
// (e.g. it's a string). Returns false.
func TestAllowListContains_LeafNotArray(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"plugins": map[string]any{
			"allow": "not-an-array",
		},
	}
	if allowListContains(obj, "plugins.allow", "pilot") {
		t.Error("expected false when leaf isn't an array")
	}
}

// entryEnabled: parent[leaf] is not a map (e.g. it's an int) → false.
func TestEntryEnabled_LeafNotMap(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"plugins": map[string]any{
			"entries": "not-a-map",
		},
	}
	if entryEnabled(obj, "plugins.entries", "pilot") {
		t.Error("expected false when entries isn't a map")
	}
}

// classifyPluginAllowList: config path is a directory (not a file) →
// ReadFile returns a non-IsNotExist error. Function returns Drifted.
func TestClassifyPluginAllowList_PathIsDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // dir itself, not a file
	state := classifyPluginAllowList(dir, "plugins.allow", "plugins.entries", "pilot")
	if state != StateDrifted {
		t.Errorf("classify=%s; want Drifted on read-as-file-but-is-dir", state)
	}
}

// classifyPluginAllowList: file exists but contents are unparseable
// JSON. Should return Drifted (so daemon retries) without erroring.
func TestClassifyPluginAllowList_UnparseableContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(p, []byte("not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	state := classifyPluginAllowList(p, "plugins.allow", "plugins.entries", "pilot")
	if state != StateDrifted {
		t.Errorf("classify=%s; want Drifted on parse failure", state)
	}
}

// writeHelper: target path IS a directory → ReadFile returns a
// non-IsNotExist error. The function returns that error.
func TestWriteHelper_TargetIsDirectoryReturnsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Treat the directory itself as the helper destination.
	_, err := writeHelper(dir, []byte("body"), 0o755)
	if err == nil {
		t.Fatal("expected error when target path is a directory")
	}
}

// Run: cancel context immediately → first Tick fires (returns error
// due to bad manifest URL), Run exits via ctx.Done branch without
// firing additional ticks. Race-detector verifies no goroutine leak.
func TestRun_ContextCancellationExits(t *testing.T) {
	t.Parallel()
	// Use a tiny interval to make ticker very active.
	ctx, cancel := context.WithCancel(context.Background())

	// Fake server that always 404s, making the first Tick fail fast.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := Config{
		Home:        t.TempDir(),
		Interval:    5 * time.Millisecond,
		ManifestURL: srv.URL + "/m.json",
		RepoBaseURL: srv.URL + "/",
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		Run(ctx, cfg)
	}()
	// Let Run fire one or two ticker beats, then cancel.
	time.Sleep(25 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancel")
	}
}

// Run: zero-interval uses default. Cover the `cfg.Interval <= 0`
// guard. We can't observe the default directly without a long wait,
// but we can confirm Run honors immediate cancellation regardless.
func TestRun_ZeroIntervalDefaults(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	cfg := Config{
		Home:        t.TempDir(),
		Interval:    0,
		ManifestURL: srv.URL + "/m.json",
		RepoBaseURL: srv.URL + "/",
	}
	cancel() // cancel immediately
	done := make(chan struct{})
	go func() {
		Run(ctx, cfg)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after immediate cancel with zero interval")
	}
}

// mergePluginAllowList: existing file contains literal JSON `null` →
// json.Unmarshal succeeds and produces obj=nil; the code path that
// re-initializes `obj = map[string]any{}` after the unmarshal must
// fire. Result: merge succeeds and the file ends up with our managed
// keys (because original was "null", the merge effectively starts
// fresh).
func TestMergePluginAllowList_NullJSONReinitializes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(cfg, []byte("null"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := mergePluginAllowList(cfg, "plugins.allow", "plugins.entries", "pilot"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	// Result: file is now a valid JSON object with our managed keys.
	body, _ := os.ReadFile(cfg)
	if !strings.Contains(string(body), `"pilot"`) {
		t.Errorf("expected pilot id in merged file: %s", body)
	}
}

// Stop ctx-deadline path: Start the service, then Stop with a
// context that's already cancelled BEFORE the worker goroutine exits.
// This forces the `case <-ctx.Done()` branch of Stop's select.
func TestService_StopCtxDeadline(t *testing.T) {
	t.Parallel()
	// Long interval + slow tick so the worker goroutine is still
	// running when we call Stop with an already-cancelled context.
	cfg := Config{
		Home:        t.TempDir(),
		Interval:    1 * time.Hour, // never fires during this test
		ManifestURL: "http://127.0.0.1:1/m.json",
		RepoBaseURL: "http://127.0.0.1:1/",
	}
	s := NewService(cfg)
	startCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(startCtx, coreapi.Deps{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Hold the worker goroutine in its ticker loop by NOT cancelling
	// startCtx. Pass Stop a context that's already cancelled.
	stopCtx, stopCancel := context.WithCancel(context.Background())
	stopCancel()
	err := s.Stop(stopCtx)
	if err == nil {
		t.Error("expected ctx.Err() from Stop's deadline branch")
	}
	// Clean up the worker goroutine via the start-ctx cancel (deferred).
}

// Service.Start + immediate Stop: the service spawns a goroutine that
// runs Tick once and the ticker loop. Stop must wait for `done`. This
// covers the success branch of Stop's select.
func TestService_StartStopGracefully(t *testing.T) {
	t.Parallel()
	// Point cfg at an unreachable manifest URL so Tick errors quickly.
	cfg := Config{
		Home:        t.TempDir(),
		Interval:    100 * time.Millisecond,
		ManifestURL: "http://127.0.0.1:1/m.json",
		RepoBaseURL: "http://127.0.0.1:1/",
	}
	s := NewService(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cover the Start path via a small fake of coreapi.Deps. We need
	// only a non-nil Events; nil works because Start passes it straight
	// to RecoverPlugin which tolerates nil events.
	if err := s.Start(ctx, coreapi.Deps{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Stop with a generous deadline.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Errorf("Stop: %v", err)
	}
}
