// SPDX-License-Identifier: AGPL-3.0-or-later

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

// manifestMux serves a minimal but complete inject manifest plus a skill
// body and heartbeat template, so a full tick has real work to do
// (create a SKILL.md and a marker in CLAUDE.md).
func manifestMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "inject-manifest.json":
			_, _ = w.Write([]byte(`{"version":1,"entrypoint":"pilotctl","tools":[
  {"name":"claude-code","rootDir":"~/.claude","skillsDir":"~/.claude/skills",
   "heartbeatPath":"~/.claude/CLAUDE.md","heartbeatTemplate":"heartbeats/claude.md"}
]}`))
		case "skills/pilotctl/SKILL.md":
			_, _ = w.Write([]byte("# pilot skill body"))
		case "heartbeats/claude.md":
			_, _ = w.Write([]byte("Pilot heartbeat directive: {{.EntrypointPath}}"))
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

// TestPlan_WritesNothing is the core guarantee behind `pilotctl skills
// status`: Plan classifies exactly what a real tick would do, but leaves
// the disk untouched.
func TestPlan_WritesNothing(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srv := httptest.NewServer(manifestMux())
	defer srv.Close()

	cfg := Config{
		Home:        home,
		ManifestURL: srv.URL + "/inject-manifest.json",
		RepoBaseURL: srv.URL + "/",
	}

	rep, err := Plan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// It must report the actions a real tick WOULD take (create the skill
	// + the marker), so the preview is meaningful.
	var sawCreate bool
	for _, o := range rep.Outcomes {
		if o.Action == ActionCreate {
			sawCreate = true
		}
		if o.Action == ActionError {
			t.Errorf("unexpected error outcome in plan: %+v", o)
		}
	}
	if !sawCreate {
		t.Errorf("Plan reported no create actions; outcomes=%+v", rep.Outcomes)
	}

	// ...but nothing may have been written. Walk the home tree: the only
	// entry allowed is the empty .claude dir we created above.
	skillPath := filepath.Join(home, ".claude", "skills", "pilotctl", "SKILL.md")
	if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
		t.Errorf("Plan wrote the skill file %s (err=%v); it must not touch disk", skillPath, err)
	}
	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	if _, err := os.Stat(claudeMD); !os.IsNotExist(err) {
		t.Errorf("Plan wrote %s; it must not touch disk", claudeMD)
	}
	// The manifest/entrypoint cache under ~/.pilot must also be absent.
	if _, err := os.Stat(filepath.Join(home, ".pilot")); !os.IsNotExist(err) {
		t.Errorf("Plan created ~/.pilot cache; it must not touch disk")
	}
}

// TestPlan_ThenTick_Applies confirms the preview matches reality: after a
// dry run reports a create, a real ForceTick actually creates the file.
func TestPlan_ThenTick_Applies(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srv := httptest.NewServer(manifestMux())
	defer srv.Close()
	cfg := Config{
		Home:        home,
		ManifestURL: srv.URL + "/inject-manifest.json",
		RepoBaseURL: srv.URL + "/",
	}

	if _, err := Plan(context.Background(), cfg); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	skillPath := filepath.Join(home, ".claude", "skills", "pilotctl", "SKILL.md")
	if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: Plan should not have written %s", skillPath)
	}

	if _, err := ForceTick(context.Background(), cfg); err != nil {
		t.Fatalf("ForceTick: %v", err)
	}
	if _, err := os.Stat(skillPath); err != nil {
		t.Errorf("ForceTick did not create %s after Plan previewed it: %v", skillPath, err)
	}
}
