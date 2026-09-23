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

// Marker block format.
//
//	<!-- pilot:begin v=1 hash=<skill> r=<block>
//	     Inserted by pilot-daemon. Remove with: pilotctl skills disable all
//	-->
//	<rendered heartbeat>
//	<!-- pilot:end -->
//
// hash= is the first 12 hex chars of sha256(SKILL.md), exactly what
// releases up to v0.2.4-beta.4 wrote and compared. r= is markerHash: it
// also covers the rendered block, so a heartbeat-only edit is Drifted and
// ships. Keeping hash= first and unchanged matters while an older binary
// still ticks the same home (daemon not restarted after an update, the
// manual-mode post-update tick running in the old pilotctl, a pinned
// binary): that binary reads only hash=, finds it current, and leaves the
// block alone. It never gets to rewrite it, so it cannot put back its
// `$`-garbled text, and the two binaries do not rewrite each other's block
// on every tick. Blocks without r= (all of them before this release) are
// Drifted here and rewritten in place once.
//
// The marker stays v=1 on purpose: older releases only recognise v=1 and
// would append a second block next to a v=2 one.

// markerHeaderRE matches a block's begin comment. The `[^>]*?` after the
// hash accepts every released header: the single-line ` -->` form
// (pre-v1.10.1), the multi-line form with the disclosure line, and this
// release's ` r=<hex>` field in front of the disclosure. Group 1 is hash=,
// group 2 the rest of the header (where r= lives).
var markerHeaderRE = regexp.MustCompile(`<!-- pilot:begin v=1 hash=([0-9a-f]+)([^>]*?)-->`)

// markerRenderRE extracts r= from the rest of the header.
var markerRenderRE = regexp.MustCompile(`^ r=([0-9a-f]+)(?:\s|$)`)

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

// markerBlock is one complete marker block found in a file.
type markerBlock struct {
	start, end int    // byte span, including the newlines after the end token
	hash       string // hash= (SKILL.md short hash)
	r          string // r= (markerHash); "" in blocks from older releases
}

// findMarkers returns every complete marker block in s, in order.
//
// A begin comment is paired with the first end token after it, but only if
// no other begin token comes first. A begin whose end line was deleted by
// hand is skipped. Pairing it with the next block's end instead (what a
// lazy `begin.*?end` regex does) would make that span one "block", and the
// next rewrite or strip would delete the user's text inside it.
func findMarkers(s string) []markerBlock {
	var out []markerBlock
	pos := 0
	for pos < len(s) {
		h := markerHeaderRE.FindStringSubmatchIndex(s[pos:])
		if h == nil {
			break
		}
		start, bodyStart := pos+h[0], pos+h[1]
		// A header can only run into another begin comment when its own
		// "-->" was deleted; resume at that begin.
		if i := strings.Index(s[start+len(markerBeginToken):bodyStart], markerBeginToken); i >= 0 {
			pos = start + len(markerBeginToken) + i
			continue
		}
		rel := strings.Index(s[bodyStart:], markerEndToken)
		if rel < 0 {
			break // no end token after this begin, so none after any later one either
		}
		endTok := bodyStart + rel
		if i := strings.Index(s[bodyStart:endTok], markerBeginToken); i >= 0 {
			pos = bodyStart + i // orphaned begin: the end belongs to a later block
			continue
		}
		end := endTok + len(markerEndToken)
		for end < len(s) && s[end] == '\n' {
			end++
		}
		b := markerBlock{start: start, end: end, hash: s[pos+h[2] : pos+h[3]]}
		if m := markerRenderRE.FindStringSubmatch(s[pos+h[4] : pos+h[5]]); m != nil {
			b.r = m[1]
		}
		out = append(out, b)
		pos = end
	}
	return out
}

// markerHash is the r= value written into a block's begin comment and
// compared by classifyMarker. It covers the entrypoint SKILL.md (skillHash)
// AND the complete rendered block (disclosure line plus heartbeat body), so
// a change to any of them re-renders the block on the next tick. The hash=
// field alone covered only SKILL.md, so an edit to a heartbeat template
// (or to the disclosure line) never reached hosts until SKILL.md also
// changed.
//
// The block is hashed as renderMarker(ref, "", "") because the hash cannot
// cover itself.
func markerHash(skillHash, ref string) string {
	return sha256Hex([]byte(skillHash + "\n" + renderMarker(ref, "", "")))[:12]
}

// validateMarkerRef rejects a rendered heartbeat that contains a marker
// delimiter. A body that quoted one would be split on the next rewrite,
// leaving the tail of the old body behind as unmanaged text that grows
// with every hash change.
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
// Heartbeat files belong to the user, so the write goes through
// writeUserFile: a symlinked file (a dotfiles repo, or one tool's file
// linked to another's) is written at its target and the link survives, and
// the file keeps its mode.
//
// Empirical pilot-first behavior is best when our directive lives in a
// file the tool loads every session but the user rarely edits by hand; the
// pilot-skills inject-manifest.json picks that file per tool.
func writeMarker(path, ref, skillShort, rShort string) error {
	block := renderMarker(ref, skillShort, rShort)

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	var next []byte
	if err != nil {
		next = []byte(block)
	} else {
		current := string(existing)

		if blocks := findMarkers(current); len(blocks) > 0 {
			next = []byte(spliceMarker(current, blocks, block))
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
	return writeUserFile(path, next)
}

// spliceMarker returns s with the first marker block replaced by block and
// every further block removed. Text between and around blocks is kept
// byte-for-byte. blocks come from findMarkers (ascending, non-overlapping).
// Pure string concatenation, no template expansion. With block == "" it
// strips every block.
func spliceMarker(s string, blocks []markerBlock, block string) string {
	var b strings.Builder
	b.Grow(len(s) + len(block))
	b.WriteString(s[:blocks[0].start])
	b.WriteString(block)
	prev := blocks[0].end
	for _, x := range blocks[1:] {
		b.WriteString(s[prev:x.start])
		prev = x.end
	}
	b.WriteString(s[prev:])
	return b.String()
}

// renderMarker formats the marker block. The begin comment carries a
// self-disclosure line explaining where the block came from and how to
// remove it — so anyone opening their CLAUDE.md / AGENTS.md / etc. and
// finding the block knows in one read what it is.
func renderMarker(ref, skillShort, rShort string) string {
	return "<!-- pilot:begin v=1 hash=" + skillShort + " r=" + rShort + "\n     " + markerDisclosure + "\n-->\n" + ref + "\n" + markerEndToken + "\n"
}

// followLink returns the file a write to path has to replace. That is path
// itself, or, when path is a symlink, the file the link points to. The
// atomic write renames a temp file over the target, so the link survives.
// A symlink whose target cannot be resolved is an error. Replacing the
// link with a regular file would detach it from wherever it pointed.
func followLink(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%s is a symlink whose target cannot be resolved (%v); refusing to replace the link", path, err)
	}
	return target, nil
}

// writeUserFile atomically replaces the content of a file the user owns
// (a heartbeat file we co-inhabit). Unlike writeFile it follows a symlink
// to its target (see followLink) and keeps an existing file's mode; a new
// file gets 0644. The temp file is created 0600 and the mode is applied
// after the rename, so the content is never more readable than the final
// file.
func writeUserFile(path string, content []byte) error {
	target, err := followLink(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(target); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(target, content, 0o600); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

// canonicalPath names the file p refers to once symlinks are resolved, so
// two paths that reach the same file compare equal. A path that does not
// exist yet is resolved through its parent directory, and failing that is
// only cleaned.
func canonicalPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if d, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(d, filepath.Base(p))
	}
	return filepath.Clean(p)
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
