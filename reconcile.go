// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// writeFile is an atomic write-via-rename. Always overwrites — callers
// classify first and only call when an action is required.
func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// writeHelper installs a helper script at path with the given mode if
// the file is missing or its content hash differs. Atomic write-via-rename.
// Returns the resulting State for reporting (Absent → created, Drifted →
// rewritten, Identical → noop).
func writeHelper(path string, content []byte, mode os.FileMode) (State, error) {
	return writeHelperMaybe(path, content, mode, false)
}

// classifyHelper computes a helper's State without touching disk. Used by
// the dry-run (Plan) path so `pilotctl skills status` can preview.
func classifyHelper(path string, content []byte, mode os.FileMode) (State, error) {
	st, err := writeHelperMaybe(path, content, mode, true)
	return st, err
}

// writeHelperMaybe installs a helper (or, when dryRun, only classifies it).
func writeHelperMaybe(path string, content []byte, mode os.FileMode, dryRun bool) (State, error) {
	cur, err := os.ReadFile(path)
	state := StateAbsent
	switch {
	case err != nil && !os.IsNotExist(err):
		return state, err
	case err == nil:
		if sha256Hex(cur) == sha256Hex(content) {
			st, sErr := os.Stat(path)
			if sErr == nil && st.Mode().Perm() == mode.Perm() {
				return StateIdentical, nil
			}
			state = StateDrifted
		} else {
			state = StateDrifted
		}
	}
	if dryRun {
		return state, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return state, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return state, err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return state, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return state, err
	}
	return state, nil
}

// ParseFileMode parses an octal mode string like "0755". Empty input
// returns the default 0o755 (executable). Invalid input returns 0o755
// with no error so a malformed manifest doesn't break the tick.
func ParseFileMode(s string) os.FileMode {
	if s == "" {
		return 0o755
	}
	var v uint64
	if _, err := fmt.Sscanf(s, "%o", &v); err != nil || v == 0 {
		return 0o755
	}
	return os.FileMode(v)
}

// markerRE matches our heartbeat marker block. The hash field lets us
// detect drift cheaply: if the canonical hash changes we re-render the
// block. The `[^>]*?` after the hash makes the regex tolerant of two
// formats: the old single-line ` -->` ending (pre-v1.10.1) and the new
// multi-line ending where the begin comment carries a self-disclosure
// line explaining what the block is and how to remove it. Both formats
// are recognised so disable cleans up markers from any released version.
var markerRE = regexp.MustCompile(`(?s)<!-- pilot:begin v=1 hash=([0-9a-f]+)[^>]*?-->.*?<!-- pilot:end -->\n*`)

// markerBeginToken and markerEndToken are the literal delimiters of a
// marker block. A rendered heartbeat must not contain either one (see
// validateMarkerRef).
const (
	markerBeginToken = "<!-- pilot:begin"
	markerEndToken   = "<!-- pilot:end -->"
)

// markerDisclosure is the line inside every block's begin comment telling
// whoever opens the file what the block is and how to remove it. The
// command has to be one that works as printed: `pilotctl skills disable`
// without an argument is rejected ("skill id required").
const markerDisclosure = "Inserted by pilot-daemon. Remove with: pilotctl skills disable all"

// markerHash is the hash written into a block's begin comment and compared
// by classifyMarker. It covers the entrypoint SKILL.md (skillHash) AND the
// complete rendered block — disclosure line plus heartbeat body — so a
// change to any of them re-renders the block on the next tick. Before this
// the hash was the SKILL.md hash alone, so an edit to a heartbeat template
// (or to the disclosure line) never reached hosts until SKILL.md also
// changed.
//
// The block is hashed as renderMarker(ref, "") — i.e. with an empty hash
// field — because the hash cannot cover itself.
//
// Compatibility: blocks written by earlier releases carry the old
// SKILL.md-only hash, which never equals this one, so every existing block
// is rewritten in place exactly once after upgrade and is Identical from
// then on. The marker version stays v=1 on purpose: markerRE in older
// releases only recognises v=1, and a v=2 block would make an older binary
// on the same host append a second block instead of replacing this one.
func markerHash(skillHash, ref string) string {
	return sha256Hex([]byte(skillHash + "\n" + renderMarker(ref, "")))[:12]
}

// validateMarkerRef rejects a rendered heartbeat that contains a marker
// delimiter. markerRE ends a block at the first end token, so a body that
// quoted one would be split on the next rewrite, leaving the tail of the
// old body behind as unmanaged text that grows with every hash change.
func validateMarkerRef(ref string) error {
	if strings.Contains(ref, markerBeginToken) || strings.Contains(ref, markerEndToken) {
		return fmt.Errorf("rendered heartbeat contains a pilot marker delimiter (%q or %q); refusing to write", markerBeginToken, markerEndToken)
	}
	return nil
}

// writeMarker inserts or replaces our marker block in path. If the file
// doesn't exist it is created with just the marker block. If one or more
// marker blocks exist (any hash) the first is replaced in place and any
// others are removed; otherwise the block is appended at the bottom of the
// body (after any YAML frontmatter).
//
// The block is spliced in as literal text. It must never go through
// regexp.ReplaceAllString: that treats `$1`, `$5`, `${x}` in the
// replacement as capture-group references, so "**$5 budget**" in a
// heartbeat template came out as "** budget**" on every rewrite.
//
// Empirical pilot-first behavior is best when our directive lives in a
// file the tool loads every session but the user rarely edits by hand; the
// pilot-skills inject-manifest.json picks that file per tool.
func writeMarker(path, ref, short string) error {
	block := renderMarker(ref, short)

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	var next []byte
	if os.IsNotExist(err) {
		next = []byte(block)
	} else {
		current := string(existing)

		if locs := markerRE.FindAllStringIndex(current, -1); len(locs) > 0 {
			next = []byte(spliceMarker(current, locs, block))
		} else {
			// Append at the bottom of the file so user persona sections
			// (e.g. SOUL.md identity, AGENTS.md workspace instructions)
			// stay prominent at the top. The Pilot directive is placed
			// after all user content and after any YAML frontmatter.
			frontmatter, body := splitFrontmatter(current)
			body = strings.TrimRight(body, "\n")
			sep := ""
			if frontmatter != "" && !strings.HasSuffix(frontmatter, "\n") {
				sep = "\n"
			}
			bodySep := ""
			if body != "" {
				bodySep = "\n\n"
			}
			next = []byte(frontmatter + sep + body + bodySep + block)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, next, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// spliceMarker returns s with the first marker block (locs[0]) replaced by
// block and every further block (locs[1:]) removed. Text between and
// around blocks is kept byte-for-byte. locs are markerRE match indices in
// ascending order. Pure string concatenation — no template expansion.
func spliceMarker(s string, locs [][]int, block string) string {
	var b strings.Builder
	b.Grow(len(s) + len(block))
	b.WriteString(s[:locs[0][0]])
	b.WriteString(block)
	prev := locs[0][1]
	for _, l := range locs[1:] {
		b.WriteString(s[prev:l[0]])
		prev = l[1]
	}
	b.WriteString(s[prev:])
	return b.String()
}

// renderMarker formats the marker block. The begin comment carries a
// self-disclosure line explaining where the block came from and how to
// remove it — so anyone opening their CLAUDE.md / AGENTS.md / etc. and
// finding the block knows in one read what it is.
func renderMarker(ref, short string) string {
	return "<!-- pilot:begin v=1 hash=" + short + "\n     " + markerDisclosure + "\n-->\n" + ref + "\n" + markerEndToken + "\n"
}

// frontmatterRE matches a YAML frontmatter block at the very start of a
// file (--- ... ---) followed by a blank line. Used to keep our marker
// from breaking parsers that require frontmatter at line 1.
var frontmatterRE = regexp.MustCompile(`(?s)\A(---\n.*?\n---\n+)`)

// splitFrontmatter returns (frontmatter, body) for a file. If no
// frontmatter is present, frontmatter == "" and body == s.
func splitFrontmatter(s string) (string, string) {
	m := frontmatterRE.FindStringIndex(s)
	if m == nil {
		return "", s
	}
	return s[m[0]:m[1]], s[m[1]:]
}
