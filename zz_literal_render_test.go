// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilot-protocol/skillinject"
)

// dollarHeartbeat carries every `$` shape regexp.Expand would rewrite
// ($N, ${name}, $$, $name) plus ones it leaves alone ($(...)). The
// heartbeat templates in pilot-skills use "$5 budget" and "$(ls ...)".
const dollarHeartbeat = "Cost: metered against a per-user **$5 budget**. $1 ${x} $$ $HOME $(ls -1t ~/.pilot/inbox | head -1) \\$0\nskill: {{.EntrypointPath}}\n"

func dollarRendered(home string) string {
	return strings.ReplaceAll(dollarHeartbeat, "{{.EntrypointPath}}",
		filepath.Join(home, ".claude", "skills", "pilotctl", "SKILL.md"))
}

func markerOutcome(t *testing.T, rep *skillinject.Report) skillinject.Outcome {
	t.Helper()
	for _, o := range rep.Outcomes {
		if o.Kind == skillinject.KindMarker && o.State != skillinject.StateRetired {
			return o
		}
	}
	t.Fatalf("no marker outcome in report: %+v", rep.Outcomes)
	return skillinject.Outcome{}
}

// A heartbeat containing `$` must reach the file byte-for-byte on the
// first insert AND on every later rewrite. Before the fix the rewrite
// went through regexp.ReplaceAllString and "$5 budget" became " budget".
func TestTick_HeartbeatDollarSignsSurviveRewrite(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	hb := filepath.Join(home, ".claude", "CLAUDE.md")

	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	r.files["heartbeats/claude-code.md"] = []byte(dollarHeartbeat)

	if _, err := skillinject.Tick(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Tick #1: %v", err)
	}
	if got := mustRead(t, hb); !strings.Contains(got, dollarRendered(home)) {
		t.Fatalf("first insert altered the heartbeat:\n%s", got)
	}

	// Heartbeat-only edit: forces the in-place rewrite path.
	r.files["heartbeats/claude-code.md"] = []byte(dollarHeartbeat + "v2 $5\n")
	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick #2: %v", err)
	}
	if o := markerOutcome(t, rep); o.Action != skillinject.ActionRewrite {
		t.Fatalf("marker action = %s, want rewrite", o.Action)
	}
	got := mustRead(t, hb)
	if !strings.Contains(got, dollarRendered(home)+"v2 $5\n") {
		t.Fatalf("rewrite altered the heartbeat (regex group expansion?):\n%s", got)
	}
	if strings.Contains(got, "** budget**") {
		t.Fatalf("found the garbled '** budget**' string:\n%s", got)
	}
	if c := strings.Count(got, "<!-- pilot:begin v=1 hash="); c != 1 {
		t.Fatalf("want exactly 1 marker block, got %d", c)
	}
}

// Changing only the heartbeat template (SKILL.md unchanged) must re-render
// the block, once. Before the fix the marker hash was the SKILL.md hash,
// so template-only fixes never shipped.
func TestTick_HeartbeatOnlyChangeShipsOnce(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	hb := filepath.Join(home, ".claude", "CLAUDE.md")

	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	cfg := r.cfg(home)

	if _, err := skillinject.Tick(context.Background(), cfg); err != nil {
		t.Fatalf("Tick #1: %v", err)
	}
	r.files["heartbeats/claude-code.md"] = []byte("Corrected heartbeat. See {{.EntrypointPath}}.")

	rep, err := skillinject.Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Tick #2: %v", err)
	}
	c := rep.Counts()
	if c[skillinject.ActionRewrite] != 1 || c[skillinject.ActionNoop] != 1 {
		t.Fatalf("want 1 rewrite (marker) + 1 noop (skill), got %+v", c)
	}
	if got := mustRead(t, hb); !strings.Contains(got, "Corrected heartbeat.") {
		t.Fatalf("heartbeat-only change did not ship:\n%s", got)
	}

	// And it settles: no rewrite loop.
	m := mustMtime(t, hb)
	rep, err = skillinject.Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Tick #3: %v", err)
	}
	if c := rep.Counts(); c[skillinject.ActionNoop] != 2 || len(rep.Outcomes) != 2 {
		t.Fatalf("want 2 noops on the settled tick, got %+v (%+v)", c, rep.Outcomes)
	}
	if mustMtime(t, hb) != m {
		t.Fatal("heartbeat rewritten on a settled tick")
	}
}

// The dry run sees heartbeat-only drift too, and writes nothing.
func TestPlan_ReportsHeartbeatOnlyDrift(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	hb := filepath.Join(home, ".claude", "CLAUDE.md")

	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	cfg := r.cfg(home)
	if _, err := skillinject.Tick(context.Background(), cfg); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	before := mustRead(t, hb)

	r.files["heartbeats/claude-code.md"] = []byte("New wording. {{.EntrypointPath}}")
	rep, err := skillinject.Plan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	o := markerOutcome(t, rep)
	if o.State != skillinject.StateDrifted || o.Action != skillinject.ActionRewrite {
		t.Fatalf("Plan marker outcome = %s/%s, want drifted/rewrite", o.State, o.Action)
	}
	if mustRead(t, hb) != before {
		t.Fatal("Plan wrote to disk")
	}
}

// legacyBlock is a marker block exactly as releases before this change
// wrote it: hash = first 12 hex chars of sha256(SKILL.md), the old
// disclosure line, and the `$`-garbled body.
func legacyBlock(skillBody string) string {
	sum := sha256.Sum256([]byte(skillBody))
	return "<!-- pilot:begin v=1 hash=" + hex.EncodeToString(sum[:])[:12] + "\n" +
		"     Inserted by pilot-daemon. Remove with: pilotctl skills disable\n" +
		"-->\nmetered against a per-user ** budget** (garbled)\n<!-- pilot:end -->\n"
}

// A node upgrading from an older release has blocks with the old
// SKILL.md-only hash and the old disclosure line. The first tick must
// rewrite each block in place exactly once — no second block appended,
// user content untouched — and later ticks must be no-ops.
func TestTick_UpgradeRewritesLegacyBlockOnce(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"multi-line legacy block":  legacyBlock(testContent),
		"single-line legacy block": "<!-- pilot:begin v=1 hash=5504ad8a7718 -->\nold body\n<!-- pilot:end -->\n",
	}
	for name, block := range cases {
		block := block
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			mustMkdirAll(t, filepath.Join(home, ".claude"))
			hb := filepath.Join(home, ".claude", "CLAUDE.md")
			above := "# My rules\n\nCost is $5 — keep it.\n\n"
			below := "\n## After\n\nmine ${x}\n"
			if err := os.WriteFile(hb, []byte(above+block+below), 0o644); err != nil {
				t.Fatal(err)
			}

			r := newFakeRepo(t)
			r.withTools(claudeOnly())
			r.files["heartbeats/claude-code.md"] = []byte(dollarHeartbeat)
			cfg := r.cfg(home)

			rep, err := skillinject.Tick(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Tick #1: %v", err)
			}
			if o := markerOutcome(t, rep); o.Action != skillinject.ActionRewrite {
				t.Fatalf("legacy block: action = %s, want rewrite", o.Action)
			}
			got := mustRead(t, hb)
			if c := strings.Count(got, "<!-- pilot:begin"); c != 1 {
				t.Fatalf("want 1 block after upgrade, got %d:\n%s", c, got)
			}
			if !strings.HasPrefix(got, above) {
				t.Errorf("content above the block changed:\n%s", got)
			}
			if !strings.Contains(got, "\n## After\n\nmine ${x}\n") {
				t.Errorf("content below the block changed:\n%s", got)
			}
			if !strings.Contains(got, dollarRendered(home)) {
				t.Errorf("block body not re-rendered literally:\n%s", got)
			}
			if !strings.Contains(got, "Remove with: pilotctl skills disable all\n") {
				t.Errorf("disclosure line not updated:\n%s", got)
			}
			if strings.Contains(got, "garbled") || strings.Contains(got, "old body") {
				t.Errorf("legacy body survived:\n%s", got)
			}

			m := mustMtime(t, hb)
			for i := 0; i < 2; i++ {
				rep, err = skillinject.Tick(context.Background(), cfg)
				if err != nil {
					t.Fatalf("settled Tick: %v", err)
				}
				if o := markerOutcome(t, rep); o.Action != skillinject.ActionNoop {
					t.Fatalf("settled tick %d: marker action = %s, want noop", i, o.Action)
				}
			}
			if mustMtime(t, hb) != m {
				t.Fatal("heartbeat rewritten after the one-time upgrade rewrite")
			}
		})
	}
}

// Two marker blocks in one file (e.g. a hand-merged dotfile) collapse to
// one on the next tick even when the first already carries the current
// hash, and the file then stays put.
func TestTick_CollapsesDuplicateMarkerBlocks(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	hb := filepath.Join(home, ".claude", "CLAUDE.md")

	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	cfg := r.cfg(home)
	if _, err := skillinject.Tick(context.Background(), cfg); err != nil {
		t.Fatalf("Tick #1: %v", err)
	}
	current := mustRead(t, hb)
	dup := "# top\n\n" + current + "\n# middle\n\n" + current + "\n# bottom\n"
	if err := os.WriteFile(hb, []byte(dup), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := skillinject.Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Tick #2: %v", err)
	}
	if o := markerOutcome(t, rep); o.State != skillinject.StateDrifted || o.Action != skillinject.ActionRewrite {
		t.Fatalf("duplicate blocks: outcome %s/%s, want drifted/rewrite", o.State, o.Action)
	}
	got := mustRead(t, hb)
	if c := strings.Count(got, "<!-- pilot:begin"); c != 1 {
		t.Fatalf("want 1 block, got %d:\n%s", c, got)
	}
	for _, want := range []string{"# top", "# middle", "# bottom"} {
		if !strings.Contains(got, want) {
			t.Errorf("user text %q lost:\n%s", want, got)
		}
	}
	if strings.Index(got, "<!-- pilot:begin") > strings.Index(got, "# middle") {
		t.Errorf("surviving block should be the first one:\n%s", got)
	}

	rep, err = skillinject.Tick(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Tick #3: %v", err)
	}
	if o := markerOutcome(t, rep); o.Action != skillinject.ActionNoop {
		t.Fatalf("after collapse: action %s, want noop", o.Action)
	}
}

// A template that renders a marker delimiter would split the block on the
// next rewrite; the tick refuses it and leaves the file alone.
func TestTick_RejectsHeartbeatContainingMarkerDelimiter(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{
		"docs: the block ends at <!-- pilot:end --> ok",
		"docs: <!-- pilot:begin v=1 hash=abc --> nested",
	} {
		home := t.TempDir()
		mustMkdirAll(t, filepath.Join(home, ".claude"))
		hb := filepath.Join(home, ".claude", "CLAUDE.md")
		if err := os.WriteFile(hb, []byte("# mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		r := newFakeRepo(t)
		r.withTools(claudeOnly())
		r.files["heartbeats/claude-code.md"] = []byte(tmpl)

		rep, err := skillinject.Tick(context.Background(), r.cfg(home))
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
		o := markerOutcome(t, rep)
		if o.Action != skillinject.ActionError || !strings.Contains(o.Err, "marker delimiter") {
			t.Fatalf("template %q: outcome %s err=%q, want error", tmpl, o.Action, o.Err)
		}
		if got := mustRead(t, hb); got != "# mine\n" {
			t.Fatalf("file modified despite rejected template:\n%s", got)
		}
	}
}

// Item 39: the removal command printed in every block must be one that
// works as written.
func TestTick_MarkerDisclosureNamesDisableAll(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	if _, err := skillinject.Tick(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	got := mustRead(t, filepath.Join(home, ".claude", "CLAUDE.md"))
	if !strings.Contains(got, "\n     Inserted by pilot-daemon. Remove with: pilotctl skills disable all\n-->\n") {
		t.Fatalf("disclosure line missing or wrong:\n%s", got)
	}

	// And disable still finds and strips the new-format block.
	if _, err := skillinject.Uninstall(context.Background(), r.cfg(home)); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if got := mustRead(t, filepath.Join(home, ".claude", "CLAUDE.md")); strings.Contains(got, "pilot:begin") {
		t.Fatalf("Uninstall left the block:\n%s", got)
	}
}
