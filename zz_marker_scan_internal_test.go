// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findMarkers pairs each end token with the nearest begin before it. A
// begin whose end line was deleted by hand is not a block, and the text
// after it is never treated as part of a later block.
func TestFindMarkers(t *testing.T) {
	t.Parallel()
	cur := renderMarker("body", "aaaaaaaaaaaa", "111111111111")
	legacyMulti := "<!-- pilot:begin v=1 hash=bbbbbbbbbbbb\n     Inserted by pilot-daemon. Remove with: pilotctl skills disable\n-->\nold\n<!-- pilot:end -->\n"
	legacyOne := "<!-- pilot:begin v=1 hash=cccccccccccc -->\nold\n<!-- pilot:end -->\n"
	orphan := "<!-- pilot:begin v=1 hash=dddddddddddd\n     Inserted by pilot-daemon. Remove with: pilotctl skills disable\n-->\nold directive\n"

	for _, tc := range []struct {
		name string
		s    string
		want []string // text of each block found
		hash []string
		r    []string
	}{
		{name: "none", s: "just text\n"},
		{name: "current format", s: "top\n" + cur + "\n\nend\n", want: []string{cur + "\n\n"}, hash: []string{"aaaaaaaaaaaa"}, r: []string{"111111111111"}},
		{name: "legacy multi-line", s: legacyMulti, want: []string{legacyMulti}, hash: []string{"bbbbbbbbbbbb"}, r: []string{""}},
		{name: "legacy single-line", s: legacyOne, want: []string{legacyOne}, hash: []string{"cccccccccccc"}, r: []string{""}},
		{name: "two blocks", s: cur + "mid\n" + legacyOne, want: []string{cur, legacyOne}, hash: []string{"aaaaaaaaaaaa", "cccccccccccc"}, r: []string{"111111111111", ""}},
		{name: "orphan begin before a block", s: orphan + "\n## MY NOTES\n\n" + cur, want: []string{cur}, hash: []string{"aaaaaaaaaaaa"}, r: []string{"111111111111"}},
		{name: "orphan begin, nothing after", s: "x\n" + orphan + "notes\n"},
		{name: "orphan end only", s: "old\n<!-- pilot:end -->\nnotes\n" + cur, want: []string{cur}, hash: []string{"aaaaaaaaaaaa"}, r: []string{"111111111111"}},
		{
			// The begin comment lost its own "-->": the header must not
			// run on into the next block's header.
			name: "header without its close",
			s:    "<!-- pilot:begin v=1 hash=eeeeeeeeeeee\n     Inserted by pilot-daemon.\nnotes\n" + cur,
			want: []string{cur}, hash: []string{"aaaaaaaaaaaa"}, r: []string{"111111111111"},
		},
		{name: "v=2 begin inside is an orphan boundary", s: "<!-- pilot:begin v=1 hash=ffffffffffff -->\nx\n<!-- pilot:begin v=2 hash=1 -->\ny\n<!-- pilot:end -->\n"},
	} {
		got := findMarkers(tc.s)
		if len(got) != len(tc.want) {
			t.Errorf("%s: found %d blocks, want %d: %+v", tc.name, len(got), len(tc.want), got)
			continue
		}
		for i, b := range got {
			if tc.s[b.start:b.end] != tc.want[i] || b.hash != tc.hash[i] || b.r != tc.r[i] {
				t.Errorf("%s: block %d = %q hash=%q r=%q, want %q hash=%q r=%q",
					tc.name, i, tc.s[b.start:b.end], b.hash, b.r, tc.want[i], tc.hash[i], tc.r[i])
			}
		}
	}
}

// logTickTo reports removals in the summary line and logs each retired
// row with its path, so automatic deletions and config edits leave a
// trace in the daemon log.
func TestLogTick_LogsRemovals(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	logTickTo(l, &Report{Disabled: true, Outcomes: []Outcome{
		{Tool: "claude-code", Kind: KindMarker, Path: "/h/.claude/CLAUDE.md", State: StateIdentical, Action: ActionNoop},
		{Tool: "openclaw", Kind: KindMarker, Path: "/h/.openclaw/workspace/HEARTBEAT.md", State: StateRetired, Action: ActionRemove},
		{Tool: "pilotprotocol-prompt-injector", Kind: KindPluginAllowList, Path: "/h/.openclaw/openclaw.json", State: StateRetired, Action: ActionRemove},
		{Tool: "pilotprotocol-prompt-injector", Kind: KindPluginFile, Path: "/h/x/index.mjs", State: StateRetired, Action: ActionRewrite, Note: "neutralized"},
		{Tool: "pilot-ask", Kind: KindHelper, Path: "/h/.pilot/bin/pilot-ask", State: StateRetired, Action: ActionError, Err: "permission denied"},
	}}, nil)
	out := buf.String()
	for _, want := range []string{
		"removes=2", "errors=1", "rewrites=1", "disabled=true",
		`path=/h/.openclaw/workspace/HEARTBEAT.md action=remove`,
		`path=/h/.openclaw/openclaw.json action=remove`,
		`path=/h/x/index.mjs action=rewrite note=neutralized`,
		`level=WARN msg="skillinject retired surface" tool=pilot-ask`,
		`err="permission denied"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CLAUDE.md") {
		t.Errorf("non-retired rows should only be counted, not listed:\n%s", out)
	}
}

// Heartbeat writes keep the user's file mode: a new file is 0644, an
// existing 0600 file stays 0600 through a rewrite and a strip, and no temp
// file is left behind.
func TestWriteUserFile_Modes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fresh := filepath.Join(dir, "sub", "NEW.md")
	if err := writeMarker(fresh, "ref", "aaaaaaaaaaaa", "111111111111"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(fresh); st.Mode().Perm() != 0o644 {
		t.Errorf("new heartbeat mode = %o, want 644", st.Mode().Perm())
	}

	private := filepath.Join(dir, "PRIVATE.md")
	if err := os.WriteFile(private, []byte("# secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(private, "ref", "aaaaaaaaaaaa", "111111111111"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(private); st.Mode().Perm() != 0o600 {
		t.Errorf("rewritten heartbeat mode = %o, want 600", st.Mode().Perm())
	}
	if r := stripMarkerFile("t", private); r.Action != RemovalStripped {
		t.Fatalf("strip: %+v", r)
	}
	if st, _ := os.Stat(private); st.Mode().Perm() != 0o600 {
		t.Errorf("stripped heartbeat mode = %o, want 600", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(private); string(b) != "# secret\n\n" {
		t.Errorf("strip left %q", b)
	}
	if _, err := os.Stat(private + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}
