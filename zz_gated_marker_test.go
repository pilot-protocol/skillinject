// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pilot-protocol/skillinject"
)

// canonicalMuseCopy is what muse/install.sh writes with
// PILOT_MUSE_FRONTMATTER=0: the SKILL.md as published.
const canonicalMuseCopy = museEntrypoint

// treeUnder lists every path under dir, relative to it, sorted.
func treeUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); rel != "." {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// A marker that does not name ~/workspace/skills and the muse format
// turns nothing on. Each case starts from a ~/workspace/skills that holds
// an unrelated skill and a pilotctl copy that is not the daemon's; a tick,
// Plan and Uninstall must leave both alone and report muse as skipped.
func TestGated_MarkerMustAttestTarget(t *testing.T) {
	t.Parallel()
	other := func(home string) string { return filepath.Join(home, ".agentx", "skills") }
	cases := map[string]func(t *testing.T, home string){
		// The finding: the installer ran with MUSE_SKILLS_DIR pointing at
		// another agent's folder.
		"another skills dir": func(t *testing.T, home string) {
			writeMarker(t, home, "skills_dir="+other(home)+"\nskill_format=muse\n")
		},
		"another skills dir, tilde": func(t *testing.T, home string) {
			writeMarker(t, home, "skills_dir=~/.agentx/skills\nskill_format=muse\n")
		},
		// PILOT_MUSE_FRONTMATTER=0: the installer kept the canonical copy.
		"canonical format": func(t *testing.T, home string) {
			writeMarker(t, home, "skills_dir="+filepath.Join(home, "workspace", "skills")+"\nskill_format=canonical\n")
		},
		"bare touch": func(t *testing.T, home string) {
			writeMarker(t, home, "")
		},
		"only comments": func(t *testing.T, home string) {
			writeMarker(t, home, "# muse\n\n")
		},
		"no skill_format": func(t *testing.T, home string) {
			writeMarker(t, home, "skills_dir="+filepath.Join(home, "workspace", "skills")+"\n")
		},
		"no skills_dir": func(t *testing.T, home string) {
			writeMarker(t, home, "skill_format=muse\n")
		},
		"relative skills_dir": func(t *testing.T, home string) {
			writeMarker(t, home, "skills_dir=workspace/skills\nskill_format=muse\n")
		},
		"parent of skills_dir": func(t *testing.T, home string) {
			writeMarker(t, home, "skills_dir="+filepath.Join(home, "workspace")+"\nskill_format=muse\n")
		},
		"line without =": func(t *testing.T, home string) {
			writeMarker(t, home, "skills_dir="+filepath.Join(home, "workspace", "skills")+"\nskill_format=muse\nmuse\n")
		},
		"key set twice": func(t *testing.T, home string) {
			writeMarker(t, home, "skills_dir="+other(home)+"\nskills_dir="+filepath.Join(home, "workspace", "skills")+"\nskill_format=muse\n")
		},
		"larger than 4 KiB": func(t *testing.T, home string) {
			writeMarker(t, home, "skills_dir="+filepath.Join(home, "workspace", "skills")+"\nskill_format=muse\n#"+strings.Repeat("x", 4096)+"\n")
		},
		"marker is a symlink": func(t *testing.T, home string) {
			decl := filepath.Join(home, "decl")
			mustWriteFile(t, decl, "skills_dir="+filepath.Join(home, "workspace", "skills")+"\nskill_format=muse\n", 0o644)
			mustMkdirAll(t, filepath.Join(home, ".pilot", "targets"))
			if err := os.Symlink(decl, filepath.Join(home, ".pilot", "targets", "muse")); err != nil {
				t.Fatal(err)
			}
		},
		"marker is a directory": func(t *testing.T, home string) {
			mustMkdirAll(t, filepath.Join(home, ".pilot", "targets", "muse"))
		},
	}
	for name, mark := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			ws := filepath.Join(home, "workspace")
			mustMkdirAll(t, filepath.Join(ws, "skills", "their-skill"))
			mustWriteFile(t, filepath.Join(ws, "skills", "their-skill", "SKILL.md"), "theirs", 0o644)
			mustMkdirAll(t, filepath.Dir(museSkillPath(home)))
			mustWriteFile(t, museSkillPath(home), canonicalMuseCopy, 0o644)
			mustMkdirAll(t, other(home))
			mark(t, home)
			before := treeUnder(t, ws)
			r := newGatedRepo(t, museGated())

			for i := 0; i < 2; i++ {
				rep, err := skillinject.Tick(context.Background(), r.cfg(home))
				if err != nil {
					t.Fatalf("Tick: %v", err)
				}
				if o, ok := toolOutcome(rep, "muse"); ok {
					t.Fatalf("muse acted on a marker that does not attest it: %+v", o)
				}
				if !contains(rep.Skipped, "muse") {
					t.Errorf("muse should be skipped, Skipped=%v", rep.Skipped)
				}
			}
			rep, err := skillinject.Plan(context.Background(), r.cfg(home))
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if o, ok := toolOutcome(rep, "muse"); ok {
				t.Errorf("Plan planned muse: %+v", o)
			}
			rr, err := skillinject.Uninstall(context.Background(), r.cfg(home))
			if err != nil {
				t.Fatalf("Uninstall: %v", err)
			}
			for _, x := range rr.Removals {
				if x.Tool == "muse" {
					t.Errorf("Uninstall acted on muse: %+v", x)
				}
			}
			if got := treeUnder(t, ws); strings.Join(got, ",") != strings.Join(before, ",") {
				t.Errorf("~/workspace changed: %v, was %v", got, before)
			}
			if got := mustRead(t, museSkillPath(home)); got != canonicalMuseCopy {
				t.Errorf("the pilotctl copy was rewritten: %q", got)
			}
			if entries, _ := os.ReadDir(other(home)); len(entries) != 0 {
				t.Errorf("the marker's own skills_dir was written: %v", entries)
			}
		})
	}
}

// The spellings a correct marker may use: "~/", a trailing slash,
// surrounding blanks, CRLF, comments, unknown keys, and a path that
// reaches ~/workspace/skills through a symlink.
func TestGated_MarkerAttestsTarget(t *testing.T) {
	t.Parallel()
	cases := map[string]func(home string) string{
		"absolute": func(home string) string {
			return "skills_dir=" + filepath.Join(home, "workspace", "skills") + "\nskill_format=muse\n"
		},
		"tilde":      func(string) string { return "skills_dir=~/workspace/skills\nskill_format=muse" },
		"trailing /": func(string) string { return "skills_dir=~/workspace/skills/\nskill_format=muse\n" },
		"blanks":     func(string) string { return "  skills_dir =  ~/workspace/skills  \n\tskill_format= muse\n" },
		"crlf":       func(string) string { return "skills_dir=~/workspace/skills\r\nskill_format=muse\r\n" },
		"unknown keys": func(string) string {
			return "# by muse/install.sh\nversion=2\nskill_format=muse\ninstalled_by=muse/install.sh\nskills_dir=~/workspace/skills\n"
		},
		"through link": func(string) string { return "skills_dir=~/ws-link/skills\nskill_format=muse\n" },
		"real via link": func(home string) string {
			return "skills_dir=" + filepath.Join(home, "real-ws", "skills") + "\nskill_format=muse\n"
		},
	}
	for name, decl := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			switch name {
			case "through link":
				mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
				if err := os.Symlink(filepath.Join(home, "workspace"), filepath.Join(home, "ws-link")); err != nil {
					t.Fatal(err)
				}
			case "real via link":
				// ~/workspace itself is a link to where the files live.
				mustMkdirAll(t, filepath.Join(home, "real-ws", "skills"))
				if err := os.Symlink(filepath.Join(home, "real-ws"), filepath.Join(home, "workspace")); err != nil {
					t.Fatal(err)
				}
			default:
				mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
			}
			writeMarker(t, home, decl(home))
			r := newGatedRepo(t, museGated())

			rep, err := skillinject.Tick(context.Background(), r.cfg(home))
			if err != nil {
				t.Fatalf("Tick: %v", err)
			}
			if o, ok := toolOutcome(rep, "muse"); !ok || o.Action != skillinject.ActionCreate {
				t.Fatalf("muse outcome = %+v (found %v), want create", o, ok)
			}
			if got := mustRead(t, museSkillPath(home)); got != museWant {
				t.Errorf("muse SKILL.md = %q", got)
			}
		})
	}
}

// PILOT_MUSE_FRONTMATTER=0 and =1 runs of the installer, in turn, over one
// ~/workspace/skills: the daemon follows the marker the last run left and
// never flips the file back on its own.
func TestGated_FormatOptOutIsNotOverridden(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Dir(museSkillPath(home)))
	r := newGatedRepo(t, museGated())
	install := func(format, copy string) {
		mustWriteFile(t, museSkillPath(home), copy, 0o644)
		writeMarker(t, home, "skills_dir=~/workspace/skills\nskill_format="+format+"\n")
	}
	tick := func() (skillinject.Outcome, bool) {
		t.Helper()
		rep, err := skillinject.Tick(context.Background(), r.cfg(home))
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
		return toolOutcome(rep, "muse")
	}

	install("canonical", canonicalMuseCopy)
	for i := 0; i < 2; i++ {
		if o, ok := tick(); ok {
			t.Fatalf("tick %d acted on the canonical copy: %+v", i, o)
		}
		if got := mustRead(t, museSkillPath(home)); got != canonicalMuseCopy {
			t.Fatalf("tick %d rewrote the canonical copy: %q", i, got)
		}
	}

	install("muse", museWant)
	if o, _ := tick(); o.Action != skillinject.ActionNoop {
		t.Errorf("after a muse-format install, muse outcome = %+v, want noop", o)
	}

	install("canonical", canonicalMuseCopy)
	if o, ok := tick(); ok {
		t.Errorf("after going back to canonical, muse acted: %+v", o)
	}
	if got := mustRead(t, museSkillPath(home)); got != canonicalMuseCopy {
		t.Errorf("canonical copy rewritten: %q", got)
	}
}

// A row that is broken is an error on a marked host whatever the marker
// says, so a manifest mistake is not hidden behind a marker mismatch.
func TestGated_BrokenRowIsAnErrorEvenIfMarkerIsForAnotherTarget(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, "workspace", "skills"))
	writeMarker(t, home, "skills_dir=~/.agentx/skills\nskill_format=muse\n")
	g := museGated()
	g.SkillsDir = "~/.ssh"
	r := newGatedRepo(t, g)

	rep, err := skillinject.Tick(context.Background(), r.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if o, _ := toolOutcome(rep, "muse"); o.Action != skillinject.ActionError {
		t.Errorf("muse outcome = %+v, want an error row", o)
	}
}
