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

// claudeAndOpenCode is two tools whose heartbeat templates render
// different blocks (each names its own skill path).
func claudeAndOpenCode() []skillinject.ManifestTool {
	return []skillinject.ManifestTool{
		{Name: "claude-code", RootDir: "~/.claude", SkillsDir: "~/.claude/skills",
			HeartbeatPath: "~/.claude/CLAUDE.md", HeartbeatTemplate: "heartbeats/claude-code.md"},
		{Name: "opencode", RootDir: "~/.config/opencode", SkillsDir: "~/.config/opencode/skills",
			HeartbeatPath: "~/.config/opencode/AGENTS.md", HeartbeatTemplate: "heartbeats/opencode.md"},
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func assertSymlink(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s was replaced by a regular file (mode %v)", p, fi.Mode())
	}
}

// A common single-source-of-truth setup: opencode's AGENTS.md is a symlink
// to ~/.claude/CLAUDE.md. Each tool renders a different block, so writing
// the shared file once per tool would replace the link with a copy (the
// atomic rename) and rewrite the file on every tick. The file is
// reconciled once, for the first tool; the link survives and later edits
// to CLAUDE.md still reach OpenCode.
func TestTick_SharedSymlinkedHeartbeatReconciledOnce(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".config", "opencode"))
	claude := filepath.Join(home, ".claude", "CLAUDE.md")
	link := filepath.Join(home, ".config", "opencode", "AGENTS.md")
	mustWriteFile(t, claude, "# my rules\n", 0o644)
	mustSymlink(t, "../../.claude/CLAUDE.md", link)

	r := newFakeRepo(t)
	r.withTools(claudeAndOpenCode())
	cfg := r.cfg(home)

	// The dry run shows the one write it would do.
	rep, err := skillinject.Plan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if o := outcomeFor(rep, link); o == nil || o.Action != skillinject.ActionNoop || !strings.Contains(o.Note, "claude-code") {
		t.Fatalf("Plan: opencode row = %+v, want a noop naming claude-code", o)
	}

	rep, err = skillinject.Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if o := outcomeFor(rep, claude); o == nil || o.Action != skillinject.ActionCreate {
		t.Fatalf("claude-code row = %+v, want create", o)
	}
	o := outcomeFor(rep, link)
	if o == nil || o.Action != skillinject.ActionNoop || o.State != skillinject.StateIdentical || !strings.Contains(o.Note, "claude-code") {
		t.Fatalf("opencode row = %+v, want identical/noop naming claude-code", o)
	}
	assertSymlink(t, link)
	got := mustRead(t, claude)
	if c := strings.Count(got, "<!-- pilot:begin"); c != 1 {
		t.Fatalf("want 1 block in the shared file, got %d:\n%s", c, got)
	}

	// Settled: nothing rewritten on later ticks.
	m := mustMtime(t, claude)
	for i := 0; i < 2; i++ {
		rep, err = skillinject.Tick(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if c := rep.Counts(); c[skillinject.ActionNoop] != len(rep.Outcomes) {
			t.Fatalf("settled tick %d not all noop: %+v", i, rep.Outcomes)
		}
	}
	if mustMtime(t, claude) != m {
		t.Fatal("shared heartbeat rewritten on a settled tick")
	}
	assertSymlink(t, link)

	// The user's later edits still reach OpenCode through the link.
	f, err := os.OpenFile(claude, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\nNEW RULE\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if !strings.Contains(mustRead(t, link), "NEW RULE") {
		t.Fatal("edit to CLAUDE.md does not show in opencode AGENTS.md")
	}

	// disable all strips the one block through the link and keeps it.
	if _, err := skillinject.Uninstall(context.Background(), cfg); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	assertSymlink(t, link)
	if got := mustRead(t, claude); strings.Contains(got, "pilot:begin") || !strings.Contains(got, "# my rules") || !strings.Contains(got, "NEW RULE") {
		t.Fatalf("Uninstall did not strip cleanly:\n%s", got)
	}
}

// The link can point the other way (CLAUDE.md -> opencode's AGENTS.md);
// the first tool in manifest order still owns the file.
func TestTick_SharedHeartbeatEitherDirection(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	mustMkdirAll(t, filepath.Join(home, ".config", "opencode"))
	claude := filepath.Join(home, ".claude", "CLAUDE.md")
	agents := filepath.Join(home, ".config", "opencode", "AGENTS.md")
	mustWriteFile(t, agents, "# shared\n", 0o644)
	mustSymlink(t, agents, claude)

	r := newFakeRepo(t)
	r.withTools(claudeAndOpenCode())
	cfg := r.cfg(home)
	for i := 0; i < 3; i++ {
		rep, err := skillinject.Tick(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if i > 0 {
			if c := rep.Counts(); c[skillinject.ActionNoop] != len(rep.Outcomes) {
				t.Fatalf("tick %d not settled: %+v", i, rep.Outcomes)
			}
		}
	}
	assertSymlink(t, claude)
	if c := strings.Count(mustRead(t, agents), "<!-- pilot:begin"); c != 1 {
		t.Fatalf("want 1 block, got %d", c)
	}
}

// A heartbeat file that is a symlink into a dotfiles repo is edited at its
// target: a rewrite (here a heartbeat-only change) keeps the link and the
// file's mode.
func TestTick_SymlinkedHeartbeatRewrittenAtTarget(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	target := filepath.Join(home, "dotfiles", "CLAUDE.md")
	mustMkdirAll(t, filepath.Dir(target))
	mustWriteFile(t, target, "# mine\n", 0o600)
	link := filepath.Join(home, ".claude", "CLAUDE.md")
	mustSymlink(t, target, link)

	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	cfg := r.cfg(home)
	if _, err := skillinject.Tick(context.Background(), cfg); err != nil {
		t.Fatalf("Tick #1: %v", err)
	}
	r.files["heartbeats/claude-code.md"] = []byte("v2 $5 budget. {{.EntrypointPath}}")
	rep, err := skillinject.Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Tick #2: %v", err)
	}
	if o := outcomeFor(rep, link); o == nil || o.Action != skillinject.ActionRewrite {
		t.Fatalf("want a rewrite, got %+v", o)
	}
	assertSymlink(t, link)
	got := mustRead(t, target)
	if !strings.HasPrefix(got, "# mine\n") || !strings.Contains(got, "v2 $5 budget.") {
		t.Fatalf("target not updated in place:\n%s", got)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o600 {
		t.Errorf("target mode = %o, want 600", st.Mode().Perm())
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

// A dangling heartbeat symlink is reported, not replaced by a regular file.
func TestTick_DanglingHeartbeatSymlinkIsAnError(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	link := filepath.Join(home, ".claude", "CLAUDE.md")
	mustSymlink(t, filepath.Join(home, "gone", "CLAUDE.md"), link)

	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	o := outcomeFor(rep, link)
	if o == nil || o.Action != skillinject.ActionError || !strings.Contains(o.Err, "refusing to replace the link") {
		t.Fatalf("want an error row for the dangling link, got %+v", o)
	}
	assertSymlink(t, link)
}

// A retired heartbeat path that links to an active one is the same file;
// the prune must not strip the block the active tool just wrote (a
// write/strip loop).
func TestTick_RetiredMarkerLinkedToActiveIsNotPruned(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ws := filepath.Join(home, ".openclaw", "workspace")
	mustMkdirAll(t, ws)
	agents := filepath.Join(ws, "AGENTS.md")
	mustWriteFile(t, agents, "# agents\n", 0o644)
	mustSymlink(t, "AGENTS.md", filepath.Join(ws, "HEARTBEAT.md")) // built-in retired path

	r := newFakeRepo(t)
	r.withTools(currentOpenClaw())
	cfg := r.cfg(home)
	for i := 0; i < 3; i++ {
		rep, err := skillinject.Tick(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Tick #%d: %v", i, err)
		}
		if rr := retiredOutcomes(rep); len(rr) != 0 {
			t.Fatalf("tick %d pruned a path linked to the active heartbeat: %+v", i, rr)
		}
		if i > 0 {
			if c := rep.Counts(); c[skillinject.ActionNoop] != len(rep.Outcomes) {
				t.Fatalf("tick %d not settled: %+v", i, rep.Outcomes)
			}
		}
	}
	if !strings.Contains(mustRead(t, agents), "pilot:begin") {
		t.Fatal("active block missing")
	}
}
