// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pilot-protocol/skillinject"
)

// releasedMarkerRE and releasedTick reproduce the marker handling of the
// released binary (v0.2.4-beta.4, origin/main before this
// change): hash= is sha256(SKILL.md)[:12], and the rewrite goes through
// regexp.ReplaceAllString. That binary can still tick the same home after
// an update: the daemon is not restarted, the manual-mode post-update tick
// runs in the old pilotctl, or a binary is pinned.
var releasedMarkerRE = regexp.MustCompile(`(?s)<!-- pilot:begin v=1 hash=([0-9a-f]+)[^>]*?-->.*?<!-- pilot:end -->\n*`)

// releasedTick runs the released binary's marker step on path and reports
// whether it wrote.
func releasedTick(t *testing.T, path, skillBody, ref string) bool {
	t.Helper()
	sum := sha256.Sum256([]byte(skillBody))
	short := hex.EncodeToString(sum[:])[:12]
	cur, err := os.ReadFile(path)
	if err == nil {
		if m := releasedMarkerRE.FindStringSubmatch(string(cur)); m != nil && m[1] == short {
			return false // Identical
		}
	}
	block := fmt.Sprintf("<!-- pilot:begin v=1 hash=%s\n     Inserted by pilot-daemon. Remove with: pilotctl skills disable\n-->\n%s\n<!-- pilot:end -->\n", short, ref)
	var next string
	switch {
	case err != nil:
		next = block
	case releasedMarkerRE.MatchString(string(cur)):
		next = releasedMarkerRE.ReplaceAllString(string(cur), block)
	default:
		next = strings.TrimRight(string(cur), "\n") + "\n\n" + block
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		t.Fatal(err)
	}
	return true
}

// With the released binary and this one ticking the same home, the block
// settles: the released binary reads hash= (still the SKILL.md hash), finds
// it current and leaves the block alone, so it never re-garbles "$5
// budget" and neither binary rewrites the other's block on every tick.
func TestTick_CoexistsWithReleasedBinary(t *testing.T) {
	t.Parallel()
	for _, releasedFirst := range []bool{false, true} {
		releasedFirst := releasedFirst
		t.Run(fmt.Sprintf("releasedFirst=%v", releasedFirst), func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			mustMkdirAll(t, filepath.Join(home, ".claude"))
			hb := filepath.Join(home, ".claude", "CLAUDE.md")
			mustWriteFile(t, hb, "# my rules\n", 0o644)

			r := newFakeRepo(t)
			r.withTools(claudeOnly())
			r.files["heartbeats/claude-code.md"] = []byte(dollarHeartbeat)
			cfg := r.cfg(home)
			skillBody := string(r.files["skills/pilotctl/SKILL.md"])
			ref := dollarRendered(home)

			newTick := func() skillinject.Action {
				t.Helper()
				rep, err := skillinject.Tick(context.Background(), cfg)
				if err != nil {
					t.Fatalf("Tick: %v", err)
				}
				return markerOutcome(t, rep).Action
			}

			if releasedFirst {
				// Upgrade from a released install whose block an earlier
				// rewrite garbled; this binary rewrites it once.
				releasedTick(t, hb, skillBody, strings.ReplaceAll(ref, "$5", ""))
				if a := newTick(); a != skillinject.ActionRewrite {
					t.Fatalf("upgrade tick: %s, want rewrite", a)
				}
			} else if a := newTick(); a != skillinject.ActionCreate {
				t.Fatalf("first tick: %s, want create", a)
			}
			settled := mustRead(t, hb)
			if !strings.Contains(settled, "**$5 budget**") {
				t.Fatalf("block not rendered literally:\n%s", settled)
			}

			for i := 0; i < 3; i++ {
				if releasedTick(t, hb, skillBody, ref) {
					t.Fatalf("round %d: the released binary rewrote the block:\n%s", i, mustRead(t, hb))
				}
				if a := newTick(); a != skillinject.ActionNoop {
					t.Fatalf("round %d: this binary %s, want noop", i, a)
				}
			}
			if got := mustRead(t, hb); got != settled {
				t.Fatalf("file changed while both binaries ticked:\n%s", got)
			}
			if !strings.HasPrefix(settled, "# my rules\n") || strings.Count(settled, "<!-- pilot:begin") != 1 {
				t.Fatalf("user text or block count wrong:\n%s", settled)
			}
		})
	}
}

// When SKILL.md changes, both binaries see drift. Whichever ticks first
// writes; the other follows at most once, and then both are quiet again.
func TestTick_CoexistsWithReleasedBinaryAcrossSkillChange(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	hb := filepath.Join(home, ".claude", "CLAUDE.md")

	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	r.files["heartbeats/claude-code.md"] = []byte(dollarHeartbeat)
	cfg := r.cfg(home)
	if _, err := skillinject.Tick(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	r.setSkillBody([]byte(testContent + "\nchanged\n"))
	skillBody := string(r.files["skills/pilotctl/SKILL.md"])
	// The released binary ticks first; its ReplaceAllString rewrite
	// garbles the block, as it does on hosts today.
	if !releasedTick(t, hb, skillBody, dollarRendered(home)) {
		t.Fatal("released binary should rewrite after a SKILL.md change")
	}
	if got := mustRead(t, hb); !strings.Contains(got, "** budget**") {
		t.Fatalf("emulated released rewrite did not garble as the real one does:\n%s", got)
	}
	rep, err := skillinject.Tick(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a := markerOutcome(t, rep).Action; a != skillinject.ActionRewrite {
		t.Fatalf("this binary: %s, want one rewrite (r= missing)", a)
	}
	settled := mustRead(t, hb)
	for i := 0; i < 2; i++ {
		if releasedTick(t, hb, skillBody, dollarRendered(home)) {
			t.Fatalf("round %d: released binary rewrote again", i)
		}
		rep, err := skillinject.Tick(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if a := markerOutcome(t, rep).Action; a != skillinject.ActionNoop {
			t.Fatalf("round %d: %s, want noop", i, a)
		}
	}
	if got := mustRead(t, hb); got != settled || !strings.Contains(got, "**$5 budget**") {
		t.Fatalf("not settled with literal text:\n%s", got)
	}
}

// The header this release writes: hash= first and unchanged (the SKILL.md
// short hash, which released binaries compare), then r=.
func TestTick_MarkerHeaderKeepsSkillHashFirst(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude"))
	r := newFakeRepo(t)
	r.withTools(claudeOnly())
	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(testContent))
	skillShort := hex.EncodeToString(sum[:])[:12]
	want := "<!-- pilot:begin v=1 hash=" + skillShort + " r=" + markerOutcome(t, rep).Hash + "\n"
	if got := mustRead(t, filepath.Join(home, ".claude", "CLAUDE.md")); !strings.HasPrefix(got, want) {
		t.Fatalf("header = %q, want prefix %q", strings.SplitN(got, "\n", 2)[0], want)
	}
	m := releasedMarkerRE.FindStringSubmatch(mustRead(t, filepath.Join(home, ".claude", "CLAUDE.md")))
	if m == nil || m[1] != skillShort {
		t.Fatalf("released regex reads hash=%v, want %s", m, skillShort)
	}
}
