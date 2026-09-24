// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gatedManifestFixture is pilot-skills inject-manifest.json with the Meta
// Muse "gatedTools" row, byte for byte what that repo ships.
const gatedManifestFixture = "testdata/inject-manifest-gated.json"

// releasedManifest is Manifest as every release decodes it: v0.2.2 and
// v0.2.3, the 902f745 build that pilot v1.13.2 to v1.13.10-rc.1 ship,
// and v0.2.4 (which added Retired). None has GatedTools. All of them use
// plain json.Unmarshal, which drops keys the struct does not name.
type releasedManifest struct {
	Version     int              `json:"version"`
	Entrypoint  string           `json:"entrypoint"`
	Description string           `json:"description,omitempty"`
	Tools       []ManifestTool   `json:"tools"`
	Helpers     []ManifestHelper `json:"helpers,omitempty"`
	Retired     *ManifestRetired `json:"retired,omitempty"`
}

// releasedParse is the parse and the checks fetchManifest makes in every
// release, unchanged since v0.2.2.
func releasedParse(body []byte) (*releasedManifest, error) {
	var m releasedManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m.Version != 1 || m.Entrypoint == "" || len(m.Tools) == 0 {
		return nil, os.ErrInvalid
	}
	return &m, nil
}

// hardeningWriteRoots is the tool write-root allowlist from the pending
// path-hardening work (validateManifestPaths, branch
// wip/preserve-2026-08-02 a72ae88). It is in no release, but it rejects a
// whole manifest when any "tools" path falls outside these roots, so
// gated rows must never move into "tools" once it lands.
var hardeningWriteRoots = []string{
	"~/.pilot/bin", "~/.claude", "~/.codex", "~/.openclaw", "~/.picoclaw",
	"~/.openhands", "~/.hermes", "~/.config/goose", "~/.config/opencode",
}

func withinWriteRoots(p, home string) bool {
	for _, r := range hardeningWriteRoots {
		if pathWithin(expandHome(r, home), expandHome(p, home)) {
			return true
		}
	}
	return false
}

// The manifest with the new key is accepted by the released decoder and
// checks, and nothing a released daemon acts on points at the Muse
// directory: the muse row reaches only builds that know "gatedTools".
func TestGatedManifest_ValidUnderReleasedRules(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(gatedManifestFixture)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["gatedTools"]; !ok {
		t.Fatal("fixture has no gatedTools key")
	}

	old, err := releasedParse(body)
	if err != nil {
		t.Fatalf("released parse rejects the manifest: %v", err)
	}
	home := "/home/u"
	for _, mt := range old.Tools {
		if mt.Name == "muse" {
			t.Errorf("a released daemon would see a muse tool row: %+v", mt)
		}
		for _, p := range []string{mt.RootDir, mt.SkillsDir, mt.HeartbeatPath} {
			if p != "" && pathWithin(expandHome("~/workspace", home), expandHome(p, home)) {
				t.Errorf("released tool %s writes under ~/workspace: %s", mt.Name, p)
			}
		}
		// Every "tools" path also passes the write-root allowlist of the
		// pending path hardening, so landing it does not reject this
		// manifest.
		for _, p := range []string{mt.RootDir, mt.SkillsDir, mt.HeartbeatPath, skillTargetPath(mt, old.Entrypoint, home)} {
			if p != "" && !withinWriteRoots(p, home) {
				t.Errorf("tools row %s: %s is outside the hardening write roots", mt.Name, p)
			}
		}
	}

	// The muse row as a "tools" row would fail that allowlist and take the
	// whole manifest down with it.
	var cur Manifest
	if err := json.Unmarshal(body, &cur); err != nil {
		t.Fatal(err)
	}
	if len(cur.GatedTools) != 1 {
		t.Fatalf("GatedTools = %+v, want the muse row", cur.GatedTools)
	}
	g := cur.GatedTools[0]
	if withinWriteRoots(g.RootDir, home) {
		t.Errorf("muse rootDir %s unexpectedly inside the tool write roots", g.RootDir)
	}
	want := ManifestGatedTool{
		Name: "muse", RootDir: "~/workspace/skills", SkillsDir: "~/workspace/skills",
		RequireMarker: "~/.pilot/targets/muse", SkillFormat: SkillFormatMuse,
	}
	if g != want {
		t.Errorf("muse row = %+v, want %+v", g, want)
	}
	if _, err := gatedMarkerPath(g, home); err != nil {
		t.Errorf("muse marker rejected: %v", err)
	}
	if _, err := resolveGatedTarget(g, cur.Entrypoint, home); err != nil {
		t.Errorf("muse row rejected: %v", err)
	}
}

// The fixture served over HTTP goes through this build's fetchManifest,
// is cached with its gatedTools key (so an offline Uninstall still finds
// the muse row), and the cached copy still parses under the released
// rules (a downgraded binary reads the same cache).
func TestGatedManifest_FetchAndCache(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(gatedManifestFixture)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "inject-manifest.json":
			_, _ = w.Write(body)
		case "skills/pilotctl/SKILL.md":
			_, _ = w.Write([]byte("---\nname: pilotctl\ndescription: d\n---\nbody\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	home := t.TempDir()
	cfg := Config{Home: home, ManifestURL: srv.URL + "/inject-manifest.json", RepoBaseURL: srv.URL + "/"}
	if _, err := Tick(context.Background(), cfg); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	cached, err := os.ReadFile(filepath.Join(cacheDir(home), manifestCacheRel))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(cached, &m); err != nil || len(m.GatedTools) != 1 || m.GatedTools[0].Name != "muse" {
		t.Fatalf("cached manifest gatedTools = %+v, %v", m.GatedTools, err)
	}
	if _, err := releasedParse(cached); err != nil {
		t.Errorf("released parse rejects the cached manifest: %v", err)
	}
}

func TestCanonicalPath_MissingDescendantOfLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// Neither the file nor its parent exists, so resolution has to go
	// through the nearest existing ancestor ("link").
	a := canonicalPath(filepath.Join(dir, "link", "a", "b", "SKILL.md"))
	b := canonicalPath(filepath.Join(dir, "real", "a", "b", "SKILL.md"))
	if a != b {
		t.Errorf("canonicalPath through a link = %q, direct = %q", a, b)
	}
}
