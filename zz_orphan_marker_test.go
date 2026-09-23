// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilot-protocol/skillinject"
)

// The user deleted the end line of a block and wrote notes after it. The
// next tick finds no complete block and appends a fresh one. From then on
// the orphaned begin must not be paired with the fresh block's end: a
// rewrite (upgrade, SKILL.md or heartbeat change) or `disable all` would
// otherwise delete everything in between, notes included.
func TestTick_OrphanedBeginNeverSwallowsUserText(t *testing.T) {
	t.Parallel()
	for name, orphan := range map[string]string{
		// As a released binary wrote it (hash= only).
		"released block": "<!-- pilot:begin v=1 hash=5504ad8a7718\n     Inserted by pilot-daemon. Remove with: pilotctl skills disable\n-->\nold directive\n",
		// As this release writes it.
		"current block": "", // filled from the first tick below
	} {
		orphan := orphan
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			mustMkdirAll(t, filepath.Join(home, ".claude"))
			hb := filepath.Join(home, ".claude", "CLAUDE.md")
			r := newFakeRepo(t)
			r.withTools(claudeOnly())
			cfg := r.cfg(home)

			if orphan == "" {
				if _, err := skillinject.Tick(context.Background(), cfg); err != nil {
					t.Fatal(err)
				}
				orphan = strings.TrimSuffix(mustRead(t, hb), "<!-- pilot:end -->\n")
			}
			const notes = "\n## MY PRECIOUS NOTES\n\nkeep me\n"
			mustWriteFile(t, hb, "# rules\n\n"+orphan+notes, 0o644)

			// No complete block: a fresh one is appended below the notes.
			rep, err := skillinject.Tick(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if a := markerOutcome(t, rep).Action; a != skillinject.ActionCreate {
				t.Fatalf("first tick: %s, want create", a)
			}
			// Settled, notes intact.
			rep, err = skillinject.Tick(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if a := markerOutcome(t, rep).Action; a != skillinject.ActionNoop {
				t.Fatalf("second tick: %s, want noop", a)
			}
			// A heartbeat change rewrites the fresh block only.
			r.files["heartbeats/claude-code.md"] = []byte("v2 {{.EntrypointPath}}")
			rep, err = skillinject.Tick(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if a := markerOutcome(t, rep).Action; a != skillinject.ActionRewrite {
				t.Fatalf("heartbeat change: %s, want rewrite", a)
			}
			got := mustRead(t, hb)
			if !strings.Contains(got, notes) || !strings.HasPrefix(got, "# rules\n\n"+orphan) || !strings.Contains(got, "v2 ") {
				t.Fatalf("rewrite lost user text:\n%s", got)
			}
			// disable all strips only the complete block.
			if _, err := skillinject.Uninstall(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			got = mustRead(t, hb)
			if !strings.Contains(got, notes) || strings.Contains(got, "v2 ") {
				t.Fatalf("Uninstall lost user text or kept the block:\n%s", got)
			}
		})
	}
}

// The review's reproduction: a released install whose end line the user
// deleted, then the released binary appended a fresh block. The upgrade
// tick rewrites only that block.
func TestTick_UpgradeKeepsNotesAfterOrphanedBegin(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	hb := filepath.Join(home, ".claude", "CLAUDE.md")
	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	skillBody := string(r.files["skills/pilotctl/SKILL.md"])
	ref := strings.ReplaceAll(testHeartbeat, "{{.EntrypointPath}}", filepath.Join(home, ".claude", "skills", "pilotctl", "SKILL.md"))

	releasedTick(t, hb, skillBody, ref)
	cur := mustRead(t, hb)
	cur = strings.Replace(cur, "<!-- pilot:end -->\n", "", 1) + "\n## MY PRECIOUS NOTES\n"
	mustWriteFile(t, hb, cur, 0o644)
	if !releasedTick(t, hb, skillBody, ref) {
		t.Fatal("released binary should append a fresh block")
	}
	if releasedTick(t, hb, skillBody, ref) {
		t.Fatal("released binary should then be settled")
	}

	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatal(err)
	}
	if a := markerOutcome(t, rep).Action; a != skillinject.ActionRewrite {
		t.Fatalf("upgrade tick: %s, want rewrite", a)
	}
	got := mustRead(t, hb)
	if !strings.Contains(got, "## MY PRECIOUS NOTES") {
		t.Fatalf("upgrade tick deleted the user's notes:\n%s", got)
	}
	if !strings.Contains(got, "disable all") {
		t.Fatalf("fresh block not rewritten:\n%s", got)
	}
}
