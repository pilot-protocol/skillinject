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

// museEntrypoint is the entrypoint body served by the gated-tool tests:
// canonical pilot-skills frontmatter (folded description, extra keys).
const museEntrypoint = "---\nname: pilotctl\ndescription: >\n  Entrypoint for Pilot Protocol.\n  Load it for live data.\ntags:\n  - pilot-protocol\nlicense: AGPL-3.0\n---\n\n# pilotctl\n\nbody.\n"

// museWant is museEntrypoint in the Muse format.
const museWant = "---\nname: \"pilotctl\"\ndescription: \"Entrypoint for Pilot Protocol. Load it for live data.\"\n---\n\n# pilotctl\n\nbody.\n"

// museGated is the pilot-skills "gatedTools" row for Meta Muse.
func museGated() skillinject.ManifestGatedTool {
	return skillinject.ManifestGatedTool{
		Name:          "muse",
		RootDir:       "~/workspace/skills",
		SkillsDir:     "~/workspace/skills",
		RequireMarker: "~/.pilot/targets/muse",
		SkillFormat:   skillinject.SkillFormatMuse,
	}
}

// newGatedRepo serves claudeOnly() plus the given gated rows.
func newGatedRepo(t *testing.T, gated ...skillinject.ManifestGatedTool) *fakeRepo {
	t.Helper()
	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	r.setSkillBody([]byte(museEntrypoint))
	r.manifest.GatedTools = gated
	return r
}

func museSkillPath(home string) string {
	return filepath.Join(home, "workspace", "skills", "pilotctl", "SKILL.md")
}

func markMuse(t *testing.T, home string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(home, ".pilot", "targets"))
	mustWriteFile(t, filepath.Join(home, ".pilot", "targets", "muse"), "", 0o644)
}

func toolOutcome(rep *skillinject.Report, tool string) (skillinject.Outcome, bool) {
	for _, o := range rep.Outcomes {
		if o.Tool == tool {
			return o, true
		}
	}
	return skillinject.Outcome{}, false
}

func TestGated_NoMarker_DoesNothing(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	r := newGatedRepo(t, museGated())

	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if _, ok := toolOutcome(rep, "muse"); ok {
		t.Errorf("muse produced an outcome without its marker: %+v", rep.Outcomes)
	}
	if !contains(rep.Skipped, "muse") {
		t.Errorf("muse should be skipped, Skipped=%v", rep.Skipped)
	}
	entries, _ := os.ReadDir(filepath.Join(home, "workspace", "skills"))
	if len(entries) != 0 {
		t.Errorf("~/workspace/skills was written without the marker: %v", entries)
	}
	// The regular tools are unaffected.
	mustExist(t, filepath.Join(home, ".claude", "skills", "pilotctl", "SKILL.md"))
}

func TestGated_Marker_WritesMuseFormatAndIsIdempotent(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
	markMuse(t, home)
	r := newGatedRepo(t, museGated())

	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	o, ok := toolOutcome(rep, "muse")
	if !ok || o.Action != skillinject.ActionCreate || o.Kind != skillinject.KindSkill || o.Path != museSkillPath(home) {
		t.Fatalf("first tick muse outcome = %+v (found %v)", o, ok)
	}
	if got := mustRead(t, museSkillPath(home)); got != museWant {
		t.Errorf("muse SKILL.md =\n%q\nwant\n%q", got, museWant)
	}
	fi, err := os.Stat(museSkillPath(home))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("muse SKILL.md mode = %v, %v; want 0644", fi.Mode().Perm(), err)
	}

	// Later ticks are noops: the hash is taken over the rewritten bytes.
	for i := 0; i < 3; i++ {
		rep, err = skillinject.Tick(context.Background(), r.cfg(home))
		if err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
		if o, _ := toolOutcome(rep, "muse"); o.Action != skillinject.ActionNoop || o.State != skillinject.StateIdentical {
			t.Fatalf("tick %d muse outcome = %+v, want identical/noop", i, o)
		}
	}
	// No temp files are left next to the skill.
	entries, _ := os.ReadDir(filepath.Dir(museSkillPath(home)))
	if len(entries) != 1 {
		t.Errorf("skill dir holds %d entries, want only SKILL.md", len(entries))
	}

	// A new upstream SKILL.md is shipped on the next tick.
	r.setSkillBody([]byte(strings.Replace(museEntrypoint, "body.", "new body.", 1)))
	rep, err = skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if o, _ := toolOutcome(rep, "muse"); o.Action != skillinject.ActionRewrite {
		t.Errorf("after upstream change muse outcome = %+v, want rewrite", o)
	}
	if got := mustRead(t, museSkillPath(home)); !strings.HasSuffix(got, "new body.\n") {
		t.Errorf("rewrite did not ship the new body: %q", got)
	}

	// A local edit is drift and is put back.
	mustWriteFile(t, museSkillPath(home), "edited", 0o644)
	rep, _ = skillinject.Tick(context.Background(), r.cfg(home))
	if o, _ := toolOutcome(rep, "muse"); o.State != skillinject.StateDrifted || o.Action != skillinject.ActionRewrite {
		t.Errorf("after local edit muse outcome = %+v, want drifted/rewrite", o)
	}
}

func TestGated_MarkerWithoutRootDir_Skipped(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	markMuse(t, home)
	r := newGatedRepo(t, museGated())

	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !contains(rep.Skipped, "muse") {
		t.Errorf("muse should be skipped without ~/workspace/skills, Skipped=%v", rep.Skipped)
	}
	mustNotExist(t, filepath.Join(home, "workspace"))
}

func TestGated_PlanWritesNothing(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
	markMuse(t, home)
	r := newGatedRepo(t, museGated())

	rep, err := skillinject.Plan(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if o, _ := toolOutcome(rep, "muse"); o.State != skillinject.StateAbsent || o.Action != skillinject.ActionCreate {
		t.Errorf("plan muse outcome = %+v, want absent/create", o)
	}
	mustNotExist(t, filepath.Join(home, "workspace", "skills", "pilotctl"))
}

func TestGated_RejectsUnsafeRows(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*skillinject.ManifestGatedTool){
		"marker outside ~/.pilot":   func(g *skillinject.ManifestGatedTool) { g.RequireMarker = "~/workspace/skills" },
		"marker escapes ~/.pilot":   func(g *skillinject.ManifestGatedTool) { g.RequireMarker = "~/.pilot/../workspace/x" },
		"marker is ~/.pilot":        func(g *skillinject.ManifestGatedTool) { g.RequireMarker = "~/.pilot" },
		"marker absolute elsewhere": func(g *skillinject.ManifestGatedTool) { g.RequireMarker = "/tmp/muse" },
		"no marker":                 func(g *skillinject.ManifestGatedTool) { g.RequireMarker = "" },
		"rootDir outside home":      func(g *skillinject.ManifestGatedTool) { g.RootDir = "/etc"; g.SkillsDir = "/etc" },
		"rootDir is home":           func(g *skillinject.ManifestGatedTool) { g.RootDir = "~"; g.SkillsDir = "~/workspace/skills" },
		"skillsDir outside rootDir": func(g *skillinject.ManifestGatedTool) { g.SkillsDir = "~/.claude/skills" },
		"skillsDir escapes rootDir": func(g *skillinject.ManifestGatedTool) { g.SkillsDir = "~/workspace/skills/../../.ssh" },
		"unknown skillFormat":       func(g *skillinject.ManifestGatedTool) { g.SkillFormat = "yaml2" },
		"bad name":                  func(g *skillinject.ManifestGatedTool) { g.Name = "../muse" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
			markMuse(t, home)
			g := museGated()
			mutate(&g)
			r := newGatedRepo(t, g)

			rep, err := skillinject.Tick(context.Background(), r.cfg(home))
			if err != nil {
				t.Fatalf("Tick: %v", err)
			}
			o, ok := toolOutcome(rep, g.Name)
			if !ok || o.Action != skillinject.ActionError || o.Err == "" {
				t.Fatalf("outcome = %+v (found %v), want an error row", o, ok)
			}
			mustNotExist(t, museSkillPath(home))
			// The regular tool rows are unaffected by a bad gated row.
			if c, ok := toolOutcome(rep, "claude-code"); ok && c.Action == skillinject.ActionError {
				t.Errorf("claude-code errored: %+v", c)
			}
		})
	}
}

func TestGated_BadEntrypointRefused(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
	markMuse(t, home)
	r := newGatedRepo(t, museGated())
	r.manifest.Entrypoint = "../../.ssh"
	r.files["skills/../../.ssh/SKILL.md"] = []byte(museEntrypoint)

	rep, _ := skillinject.Tick(context.Background(), r.cfg(home))
	if rep != nil {
		if o, ok := toolOutcome(rep, "muse"); ok && o.Action != skillinject.ActionError {
			t.Errorf("muse outcome with a traversal entrypoint = %+v, want error", o)
		}
	}
	mustNotExist(t, filepath.Join(home, ".ssh", "SKILL.md"))
}

func TestGated_SymlinkedSkillDirRefused(t *testing.T) {
	t.Parallel()
	for name, inside := range map[string]bool{"outside rootDir": false, "inside rootDir": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			skills := filepath.Join(home, "workspace", "skills")
			mustMkdirAll(t, skills)
			markMuse(t, home)
			target := filepath.Join(home, "elsewhere")
			if inside {
				target = filepath.Join(skills, "other")
			}
			mustMkdirAll(t, target)
			if err := os.Symlink(target, filepath.Join(skills, "pilotctl")); err != nil {
				t.Fatal(err)
			}
			r := newGatedRepo(t, museGated())

			rep, err := skillinject.Tick(context.Background(), r.cfg(home))
			if err != nil {
				t.Fatalf("Tick: %v", err)
			}
			if o, _ := toolOutcome(rep, "muse"); o.Action != skillinject.ActionError || !strings.Contains(o.Err, "symlink") {
				t.Errorf("muse outcome = %+v, want a symlink error", o)
			}
			mustNotExist(t, filepath.Join(target, "SKILL.md"))

			// Uninstall leaves the link and its target alone as well.
			mustWriteFile(t, filepath.Join(target, "SKILL.md"), "theirs", 0o644)
			rr, err := skillinject.Uninstall(context.Background(), r.cfg(home))
			if err != nil {
				t.Fatalf("Uninstall: %v", err)
			}
			for _, x := range rr.Removals {
				if x.Tool == "muse" && x.Action != skillinject.RemovalError {
					t.Errorf("muse removal = %+v, want error", x)
				}
			}
			if got := mustRead(t, filepath.Join(target, "SKILL.md")); got != "theirs" {
				t.Errorf("link target was changed: %q", got)
			}
		})
	}
}

func TestGated_SymlinkedSkillFileRefused(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, "workspace", "skills", "pilotctl")
	mustMkdirAll(t, dir)
	markMuse(t, home)
	victim := filepath.Join(home, "victim.md")
	mustWriteFile(t, victim, "keep", 0o644)
	if err := os.Symlink(victim, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	r := newGatedRepo(t, museGated())

	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if o, _ := toolOutcome(rep, "muse"); o.Action != skillinject.ActionError {
		t.Errorf("muse outcome = %+v, want error", o)
	}
	if got := mustRead(t, victim); got != "keep" {
		t.Errorf("symlink target overwritten: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "SKILL.md")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v %v", fi, err)
	}
}

// A link planted at the temp name older code used (<file>.tmp) is not
// followed: gated writes use a random O_EXCL name.
func TestGated_PlantedTempLinkNotFollowed(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, "workspace", "skills", "pilotctl")
	mustMkdirAll(t, dir)
	markMuse(t, home)
	victim := filepath.Join(home, "victim")
	mustWriteFile(t, victim, "keep", 0o644)
	if err := os.Symlink(victim, filepath.Join(dir, "SKILL.md.tmp")); err != nil {
		t.Fatal(err)
	}
	r := newGatedRepo(t, museGated())

	if _, err := skillinject.Tick(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := mustRead(t, victim); got != "keep" {
		t.Errorf("planted temp link was followed: %q", got)
	}
	if got := mustRead(t, museSkillPath(home)); got != museWant {
		t.Errorf("muse SKILL.md = %q", got)
	}
}

func TestGated_SameFileAsRegularToolRefused(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	markMuse(t, home)
	// A gated row whose skill file is claude-code's skill copy would
	// rewrite it with different bytes on every tick.
	g := museGated()
	g.RootDir, g.SkillsDir = "~/.claude", "~/.claude/skills"
	r := newGatedRepo(t, g)

	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if o, _ := toolOutcome(rep, "muse"); o.Action != skillinject.ActionError || !strings.Contains(o.Err, "claude-code") {
		t.Errorf("muse outcome = %+v, want an error naming claude-code", o)
	}
	if got := mustRead(t, filepath.Join(home, ".claude", "skills", "pilotctl", "SKILL.md")); got != museEntrypoint {
		t.Errorf("claude-code skill copy was changed: %q", got)
	}
}

func TestGated_FlatNaming(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
	markMuse(t, home)
	g := museGated()
	g.SkillNaming = "flat"
	r := newGatedRepo(t, g)

	if _, err := skillinject.Tick(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	flat := filepath.Join(home, "workspace", "skills", "pilotctl.md")
	if got := mustRead(t, flat); got != museWant {
		t.Errorf("flat skill = %q", got)
	}
	rr, err := skillinject.Uninstall(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	mustNotExist(t, flat)
	mustExist(t, filepath.Join(home, "workspace", "skills"))
	_ = rr
}

func TestGated_Uninstall(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
	markMuse(t, home)
	r := newGatedRepo(t, museGated())
	if _, err := skillinject.Tick(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	mustExist(t, museSkillPath(home))

	rr, err := skillinject.Uninstall(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	var got []skillinject.Removal
	for _, x := range rr.Removals {
		if x.Tool == "muse" {
			got = append(got, x)
		}
	}
	if len(got) != 1 || got[0].Action != skillinject.RemovalDeleted || got[0].Path != museSkillPath(home) {
		t.Errorf("muse removals = %+v", got)
	}
	mustNotExist(t, museSkillPath(home))
	mustNotExist(t, filepath.Dir(museSkillPath(home))) // empty entrypoint dir pruned
	mustExist(t, filepath.Join(home, "workspace", "skills"))

	// A second uninstall is a noop.
	rr, _ = skillinject.Uninstall(context.Background(), r.cfg(home))
	for _, x := range rr.Removals {
		if x.Tool == "muse" && x.Action != skillinject.RemovalNoop {
			t.Errorf("second uninstall muse removal = %+v, want noop", x)
		}
	}
}

func TestGated_UninstallKeepsOtherFilesInSkillDir(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
	markMuse(t, home)
	r := newGatedRepo(t, museGated())
	if _, err := skillinject.Tick(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	extra := filepath.Join(filepath.Dir(museSkillPath(home)), "notes.md")
	mustWriteFile(t, extra, "mine", 0o644)
	if _, err := skillinject.Uninstall(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	mustNotExist(t, museSkillPath(home))
	mustExist(t, extra)
}

// Without the marker, Uninstall touches nothing under rootDir either: a
// ~/workspace/skills/pilotctl/SKILL.md on a host that never opted in is
// not ours.
func TestGated_UninstallWithoutMarkerLeavesFiles(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Dir(museSkillPath(home)))
	mustWriteFile(t, museSkillPath(home), "someone else's skill", 0o644)
	r := newGatedRepo(t, museGated())

	rr, err := skillinject.Uninstall(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	for _, x := range rr.Removals {
		if x.Tool == "muse" {
			t.Errorf("muse removal without marker: %+v", x)
		}
	}
	if got := mustRead(t, museSkillPath(home)); got != "someone else's skill" {
		t.Errorf("file changed: %q", got)
	}
}

// Uninstall works offline from the cached manifest, which keeps the
// gatedTools key.
func TestGated_UninstallOfflineFromCache(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
	markMuse(t, home)
	r := newGatedRepo(t, museGated())
	if _, err := skillinject.Tick(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	cfg := r.cfg(home)
	r.srv.Close()

	rr, err := skillinject.Uninstall(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !rr.ManifestOffline {
		t.Error("expected the cached manifest to be used")
	}
	mustNotExist(t, museSkillPath(home))
}

// The disable flow (Uninstall, then disabled mode) leaves the file gone:
// disabled ticks do not reinstall it.
func TestGated_DisabledModeDoesNotReinstall(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
	markMuse(t, home)
	r := newGatedRepo(t, museGated())
	if _, err := skillinject.Tick(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if _, err := skillinject.Uninstall(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if err := skillinject.SetMode(home, skillinject.ModeDisabled); err != nil {
		t.Fatal(err)
	}
	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil || !rep.Disabled {
		t.Fatalf("Tick in disabled mode: %+v, %v", rep, err)
	}
	mustNotExist(t, museSkillPath(home))
}

func TestGated_WriteErrorIsReported(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	skills := filepath.Join(home, "workspace", "skills")
	mustMkdirAll(t, skills)
	markMuse(t, home)
	if err := os.Chmod(skills, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skills, 0o755) })
	r := newGatedRepo(t, museGated())

	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if o, _ := toolOutcome(rep, "muse"); o.Action != skillinject.ActionError || o.Err == "" {
		t.Errorf("muse outcome = %+v, want a write error", o)
	}
}

func TestGated_UninstallEdgeCases(t *testing.T) {
	t.Parallel()
	t.Run("not a regular file", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		mustMkdirAll(t, museSkillPath(home)) // a directory named SKILL.md
		markMuse(t, home)
		r := newGatedRepo(t, museGated())
		rr, err := skillinject.Uninstall(context.Background(), r.cfg(home))
		if err != nil {
			t.Fatalf("Uninstall: %v", err)
		}
		for _, x := range rr.Removals {
			if x.Tool == "muse" && x.Action != skillinject.RemovalError {
				t.Errorf("muse removal = %+v, want error", x)
			}
		}
		mustExist(t, museSkillPath(home))
	})
	t.Run("rootDir missing", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		markMuse(t, home)
		r := newGatedRepo(t, museGated())
		rr, err := skillinject.Uninstall(context.Background(), r.cfg(home))
		if err != nil {
			t.Fatalf("Uninstall: %v", err)
		}
		for _, x := range rr.Removals {
			if x.Tool == "muse" && x.Action != skillinject.RemovalNoop {
				t.Errorf("muse removal = %+v, want noop", x)
			}
		}
	})
	t.Run("invalid rows", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
		markMuse(t, home)
		badMarker, badRoot := museGated(), museGated()
		badMarker.Name, badMarker.RequireMarker = "bad-marker", "~/workspace/m"
		badRoot.Name, badRoot.RootDir = "bad-root", "/etc"
		r := newGatedRepo(t, badMarker, badRoot)
		rr, err := skillinject.Uninstall(context.Background(), r.cfg(home))
		if err != nil {
			t.Fatalf("Uninstall: %v", err)
		}
		n := 0
		for _, x := range rr.Removals {
			if x.Tool == "bad-marker" || x.Tool == "bad-root" {
				n++
				if x.Action != skillinject.RemovalError {
					t.Errorf("%s removal = %+v, want error", x.Tool, x)
				}
			}
		}
		if n != 2 {
			t.Errorf("got %d rows for the invalid gated tools, want 2", n)
		}
	})
}
