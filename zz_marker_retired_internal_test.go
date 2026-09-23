// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// writeMarker splices the block in literally on all three paths (new
// file, append, in-place replace). regexp.ReplaceAllString would have
// expanded $1 / $5 / ${x} as group references and collapsed $$.
func TestWriteMarker_DollarSignsAreLiteral(t *testing.T) {
	t.Parallel()
	refs := []string{
		"per-user **$5 budget**",
		"$1 $2 $10 ${1} ${x} ${name}",
		"$$ and $$$",
		"$HOME/.pilot and ${HOME}",
		`"$(ls -1t ~/.pilot/inbox/*.json | head -1)"`,
		`\$0 trailing $`,
	}
	for _, ref := range refs {
		dir := t.TempDir()
		newFile := filepath.Join(dir, "new.md")
		appended := filepath.Join(dir, "append.md")
		replaced := filepath.Join(dir, "replace.md")
		if err := os.WriteFile(appended, []byte("# user\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := "# user\n\n" + renderMarker("old body", "0123456789ab", "ba9876543210") + "\n# after\n"
		if err := os.WriteFile(replaced, []byte(old), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{newFile, appended, replaced} {
			if err := writeMarker(p, ref, "abcdef012345", "543210fedcba"); err != nil {
				t.Fatalf("writeMarker(%s): %v", p, err)
			}
			b, _ := os.ReadFile(p)
			if !strings.Contains(string(b), "-->\n"+ref+"\n<!-- pilot:end -->\n") {
				t.Errorf("%s: ref %q not written literally:\n%s", filepath.Base(p), ref, b)
			}
		}
		b, _ := os.ReadFile(replaced)
		if !strings.HasPrefix(string(b), "# user\n\n") || !strings.HasSuffix(string(b), "# after\n") {
			t.Errorf("user text around the replaced block changed:\n%s", b)
		}
	}
}

func TestSpliceMarker_KeepsFirstDropsRest(t *testing.T) {
	t.Parallel()
	a := renderMarker("A", "aaaaaaaaaaaa", "111111111111")
	b := renderMarker("B", "bbbbbbbbbbbb", "222222222222")
	s := "top\n" + a + "mid\n" + b + "\n\nend\n"
	got := spliceMarker(s, findMarkers(s), "NEW\n")
	if want := "top\nNEW\nmid\nend\n"; got != want {
		t.Fatalf("spliceMarker:\n got %q\nwant %q", got, want)
	}
}

func TestClassifyMarker_States(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.md")
	if s := classifyMarker(p, "abc", "def"); s != StateAbsent {
		t.Errorf("missing file: %s", s)
	}
	one := renderMarker("x", "aaaaaaaaaaaa", "111111111111")
	for _, tc := range []struct {
		body string
		want State
	}{
		{"no block\n", StateAbsent},
		{one, StateIdentical},
		{renderMarker("x", "bbbbbbbbbbbb", "111111111111"), StateDrifted}, // SKILL.md changed
		{renderMarker("x", "aaaaaaaaaaaa", "222222222222"), StateDrifted}, // block changed
		// A block from an older release has hash= only: rewritten once.
		{"<!-- pilot:begin v=1 hash=aaaaaaaaaaaa\n     Inserted by pilot-daemon. Remove with: pilotctl skills disable\n-->\nx\n<!-- pilot:end -->\n", StateDrifted},
		{"<!-- pilot:begin v=1 hash=aaaaaaaaaaaa -->\nx\n<!-- pilot:end -->\n", StateDrifted},
		{one + "\n" + one, StateDrifted}, // duplicates always drift
	} {
		if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if s := classifyMarker(p, "aaaaaaaaaaaa", "111111111111"); s != tc.want {
			t.Errorf("classifyMarker(%q) = %s, want %s", tc.body, s, tc.want)
		}
	}
}

// markerHash moves with SKILL.md, the heartbeat body and the disclosure
// line, is stable for equal inputs, and round-trips through findMarkers.
func TestMarkerHash(t *testing.T) {
	t.Parallel()
	base := markerHash("skill-1", "body")
	if base != markerHash("skill-1", "body") {
		t.Fatal("markerHash not deterministic")
	}
	if base == markerHash("skill-2", "body") {
		t.Error("hash ignores SKILL.md")
	}
	if base == markerHash("skill-1", "body.") {
		t.Error("hash ignores the rendered heartbeat")
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(base) {
		t.Errorf("hash %q is not 12 hex chars", base)
	}
	// The hash covers the whole rendered block, disclosure included.
	if !strings.Contains(renderMarker("body", "", ""), markerDisclosure) {
		t.Error("renderMarker does not carry the disclosure line")
	}
	bs := findMarkers("x\n" + renderMarker("body", "0123456789ab", base))
	if len(bs) != 1 || bs[0].hash != "0123456789ab" || bs[0].r != base {
		t.Fatalf("findMarkers does not round-trip the block: %+v", bs)
	}
}

func TestMarkerDisclosure_NamesWorkingCommand(t *testing.T) {
	t.Parallel()
	if !strings.HasSuffix(markerDisclosure, "pilotctl skills disable all") {
		t.Fatalf("disclosure %q must name `pilotctl skills disable all`", markerDisclosure)
	}
	if strings.Contains(markerDisclosure, ">") {
		t.Fatal("disclosure must not contain '>' (markerHeaderRE's header is [^>]*?)")
	}
}

func TestValidateMarkerRef(t *testing.T) {
	t.Parallel()
	for ref, ok := range map[string]bool{
		"plain body $5":                   true,
		"mentions pilot:end in prose":     true,
		"x <!-- pilot:end --> y":          false,
		"x <!-- pilot:begin v=1 hash=a":   false,
		"<!-- pilot:begin v=2 hash=a -->": false,
	} {
		if err := validateMarkerRef(ref); (err == nil) != ok {
			t.Errorf("validateMarkerRef(%q) err=%v, want ok=%v", ref, err, ok)
		}
	}
}

func TestCollectRetired_SkipsActiveAndUnsafeEntries(t *testing.T) {
	t.Parallel()
	home := "/home/u"
	m := &Manifest{
		Tools: []ManifestTool{{
			Name: "picoclaw", HeartbeatPath: "~/.picoclaw/workspace/AGENT.md",
			Plugin: &ManifestPlugin{ID: "pilotprotocol-prompt-injector", InstallPath: "~/.openclaw/extensions/pilotprotocol-prompt-injector"},
		}},
		Helpers: []ManifestHelper{{Name: "pilot-ask", Dst: "~/.pilot/bin/pilot-ask"}},
		Retired: &ManifestRetired{
			Markers: []RetiredMarker{
				{Tool: "openclaw", Path: "~/.openclaw/workspace/HEARTBEAT.md"}, // dup of built-in
				{Tool: "x", Path: ""},
				{Tool: "x", Path: "rel/path.md"},
			},
			Plugins: []ManifestPlugin{
				{ID: "p-ok", InstallPath: "~/.tool/ext/p-ok", Files: []ManifestPluginFile{
					{Name: "index.mjs"}, {Name: "../escape.js"}, {Name: ""}, {Name: "."},
				}, AllowList: &ManifestPluginAllowList{ConfigPath: "~/.tool/cfg.json", AllowListJsonPath: "a", EntriesJsonPath: "e"}},
				{ID: "p-bad", InstallPath: "~/.tool/ext/other-name"},
				{ID: "", InstallPath: "~/.tool/ext/"},
			},
			Helpers: []ManifestHelper{
				{Name: "pilot-dir", Dst: "~/.pilot"},
				{Name: "outside", Dst: "~/.pilotx/bin/h"},
				{Name: "ok", Dst: "~/.pilot/bin/old"},
			},
		},
	}
	got := collectRetired(m, home)

	var markers []string
	for _, x := range got.markers {
		markers = append(markers, x.path)
	}
	// Built-in AGENT.md is active in m → skipped; HEARTBEAT.md listed once.
	if want := []string{"/home/u/.openclaw/workspace/HEARTBEAT.md"}; strings.Join(markers, ",") != strings.Join(want, ",") {
		t.Errorf("markers = %v, want %v", markers, want)
	}

	if len(got.plugins) != 1 || got.plugins[0].id != "p-ok" {
		t.Fatalf("plugins = %+v, want only p-ok (built-in prompt-injector is active in m)", got.plugins)
	}
	p := got.plugins[0]
	if strings.Join(p.files, ",") != "/home/u/.tool/ext/p-ok/index.mjs" {
		t.Errorf("plugin files = %v, want only index.mjs", p.files)
	}
	if p.cfgPath != "/home/u/.tool/cfg.json" || p.allowList == nil {
		t.Errorf("plugin allow-list not resolved: %+v", p)
	}

	var helpers []string
	for _, x := range got.helpers {
		helpers = append(helpers, x.path)
	}
	// Built-in pilot-ask is active in m → skipped.
	if want := []string{"/home/u/.pilot/bin/old"}; strings.Join(helpers, ",") != strings.Join(want, ",") {
		t.Errorf("helpers = %v, want %v", helpers, want)
	}
}

func TestCollectRetired_BuiltinsWithEmptyManifest(t *testing.T) {
	t.Parallel()
	got := collectRetired(&Manifest{}, "/h")
	if len(got.markers) != 2 || len(got.plugins) != 1 || len(got.helpers) != 1 {
		t.Fatalf("built-in list not fully applied: %+v", got)
	}
	if got.plugins[0].cfgPath != "/h/.openclaw/openclaw.json" {
		t.Errorf("prompt-injector config path = %q", got.plugins[0].cfgPath)
	}
}

// A symlinked openclaw.json (dotfiles repo) is edited at its target and
// the link survives.
func TestDropPluginFromConfig_FollowsSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "openclaw.json")
	link := filepath.Join(dir, "openclaw.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"plugins":{"allow":["gone","keep"],"entries":{"gone":{"enabled":false}}}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	al := &ManifestPluginAllowList{ConfigPath: link, AllowListJsonPath: "plugins.allow", EntriesJsonPath: "plugins.entries"}
	if err := dropPluginFromConfig(link, al, "gone"); err != nil {
		t.Fatalf("dropPluginFromConfig: %v", err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", fi, err)
	}
	b, _ := os.ReadFile(target)
	if strings.Contains(string(b), "gone") || !strings.Contains(string(b), `"keep"`) {
		t.Fatalf("target not edited correctly:\n%s", b)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 640", st.Mode().Perm())
	}
	// Removing it again is a no-op.
	before, _ := os.ReadFile(target)
	if err := dropPluginFromConfig(link, al, "gone"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(target); string(after) != string(before) {
		t.Error("second drop rewrote the file")
	}
}

func TestDropPluginFromConfig_MissingAndMalformed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	al := &ManifestPluginAllowList{AllowListJsonPath: "plugins.allow", EntriesJsonPath: "plugins.entries"}
	if err := dropPluginFromConfig(filepath.Join(dir, "missing.json"), al, "x"); err != nil {
		t.Errorf("missing config: %v", err)
	}
	for _, body := range []string{"{ nope", `{"a":1} {"b":2}`, `[1,2]`} {
		p := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := dropPluginFromConfig(p, al, "x"); err == nil {
			t.Errorf("config %q: want parse error", body)
		}
		if listed, err := pluginInConfig(p, al, "x"); err == nil || listed {
			t.Errorf("pluginInConfig(%q) = %v, %v; want error", body, listed, err)
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Errorf("malformed config %q was modified", body)
		}
	}
}

func TestPluginInConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	al := &ManifestPluginAllowList{AllowListJsonPath: "plugins.allow", EntriesJsonPath: "plugins.entries"}
	p := filepath.Join(dir, "c.json")
	if listed, err := pluginInConfig(p, al, "x"); listed || err != nil {
		t.Errorf("missing file: %v %v", listed, err)
	}
	for body, want := range map[string]bool{
		`{}`:                               false,
		`null`:                             false,
		`{"plugins":{"allow":["x"]}}`:      true,
		`{"plugins":{"entries":{"x":{}}}}`: true,
		`{"plugins":{"allow":["y"],"entries":{}}}`:  false,
		`{"plugins":{"allow":"x","entries":["x"]}}`: false,
	} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		listed, err := pluginInConfig(p, al, "x")
		if err != nil || listed != want {
			t.Errorf("pluginInConfig(%s) = %v, %v; want %v", body, listed, err, want)
		}
	}
}

// Retired plugin files are deleted only after the allow-list edit
// succeeds; with no allow-list configured they go directly. An empty
// plugin dir left behind is removed.
func TestApplyRetiredPlugin_NoAllowListAndEmptyDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "index.mjs")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := applyRetiredPlugin(retiredPlugin{id: "p", installDir: dir, files: []string{f}}, false)
	if len(res) != 1 || res[0].action != RemovalDeleted {
		t.Fatalf("results = %+v", res)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("empty plugin dir not removed: %v", err)
	}
	// Nothing left: no results at all.
	if res := applyRetiredPlugin(retiredPlugin{id: "p", installDir: dir, files: []string{f}}, false); len(res) != 0 {
		t.Errorf("second pass reported %+v", res)
	}
}

// With nothing of the plugin on disk, an unreadable config produces no
// row: it would otherwise show as an error on every tick for every host.
func TestApplyRetiredPlugin_UnparseableConfigWithoutFilesIsQuiet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.json")
	if err := os.WriteFile(cfg, []byte("{ nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	rp := retiredPlugin{
		id: "p", installDir: filepath.Join(dir, "p"),
		files:     []string{filepath.Join(dir, "p", "index.mjs")},
		allowList: &ManifestPluginAllowList{AllowListJsonPath: "a", EntriesJsonPath: "e"}, cfgPath: cfg,
	}
	if res := applyRetiredPlugin(rp, false); len(res) != 0 {
		t.Errorf("want no rows, got %+v", res)
	}
}
