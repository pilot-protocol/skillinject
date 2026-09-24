// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

// Gated tools.
//
// A regular tool is detected by its rootDir existing (~/.claude,
// ~/.openclaw, ...). That does not work for Meta Muse, which loads skills
// from ~/workspace/skills: many hosts have a directory by that name that
// has nothing to do with Muse. A gated tool is therefore active only while
// its marker file exists. The marker has to be inside ~/.pilot, which only
// Pilot's own installers write; the Muse installer creates
// ~/.pilot/targets/muse.
//
// Gated tools come from the manifest's "gatedTools" key, never from
// "tools". Released daemons decode the manifest into a struct without that
// field and encoding/json drops the key, so they never act on these rows.
// A row in "tools" would be installed by every released daemon on every
// host where the directory exists, with no marker check and no frontmatter
// rewrite.
//
// Rules:
//
//   - requireMarker must resolve inside ~/.pilot. Otherwise the row is an
//     error and nothing happens.
//   - While the marker is absent the tool is skipped: nothing under rootDir
//     is read, written or removed, by a tick or by Uninstall.
//   - rootDir must be inside the home directory and exist, skillsDir must
//     be rootDir or inside it, and the skill file must be inside skillsDir.
//   - Every file operation goes through an os.Root opened on rootDir, so no
//     symlink can take a read, write or removal outside rootDir, even one
//     created between a check and the write. On top of that, the skill file
//     and every directory between rootDir and it must not be symlinks at
//     all: a link there is not ours, and following it would write into
//     someone else's skill.
//   - The new content goes to a temp file created with O_EXCL under a
//     random name and is renamed over the skill file, so a link planted at
//     a predictable temp name is never followed.
//   - Only the entrypoint skill copy is installed. There is no heartbeat and
//     no plugin.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// gatedTarget is a gated tool resolved against the home directory.
type gatedTarget struct {
	rootDir  string // absolute and clean
	skillRel string // the skill file, relative to rootDir
	path     string // the skill file, absolute (for reports)
	flat     bool   // skillNaming "flat": the file sits directly in skillsDir
}

// gatedMarkerPath resolves gt.RequireMarker and checks that it is inside
// ~/.pilot.
func gatedMarkerPath(gt ManifestGatedTool, home string) (string, error) {
	if gt.RequireMarker == "" {
		return "", fmt.Errorf("gated tool %q has no requireMarker", gt.Name)
	}
	pilotDir := filepath.Join(home, ".pilot")
	marker := filepath.Clean(expandHome(gt.RequireMarker, home))
	if !filepath.IsAbs(marker) || marker == pilotDir || !pathWithin(pilotDir, marker) {
		return "", fmt.Errorf("gated tool %q: requireMarker %q must be a path inside ~/.pilot", gt.Name, gt.RequireMarker)
	}
	return marker, nil
}

// markerPresent reports whether the marker exists. A marker that cannot
// be checked counts as absent, which keeps the tool off.
func markerPresent(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// resolveGatedTarget checks gt's paths and returns where its skill copy
// goes. The marker is checked separately (gatedMarkerPath).
func resolveGatedTarget(gt ManifestGatedTool, entrypoint, home string) (gatedTarget, error) {
	if !validIdentifier(gt.Name) {
		return gatedTarget{}, fmt.Errorf("gated tool has invalid name %q", gt.Name)
	}
	if !validIdentifier(entrypoint) {
		return gatedTarget{}, fmt.Errorf("gated tool %q: entrypoint %q is not a plain name", gt.Name, entrypoint)
	}
	home = filepath.Clean(home)
	root := filepath.Clean(expandHome(gt.RootDir, home))
	if gt.RootDir == "" || !filepath.IsAbs(root) || root == home || !pathWithin(home, root) {
		return gatedTarget{}, fmt.Errorf("gated tool %q: rootDir %q must be a directory inside the home directory", gt.Name, gt.RootDir)
	}
	skills := filepath.Clean(expandHome(gt.SkillsDir, home))
	if gt.SkillsDir == "" || !pathWithin(root, skills) {
		return gatedTarget{}, fmt.Errorf("gated tool %q: skillsDir %q must be inside rootDir %q", gt.Name, gt.SkillsDir, gt.RootDir)
	}
	flat := gt.SkillNaming == "flat"
	path := filepath.Join(skills, entrypoint, "SKILL.md")
	if flat {
		path = filepath.Join(skills, entrypoint+".md")
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return gatedTarget{}, fmt.Errorf("gated tool %q: skill path %q escapes rootDir %q", gt.Name, path, gt.RootDir)
	}
	return gatedTarget{rootDir: root, skillRel: rel, path: path, flat: flat}, nil
}

// validIdentifier accepts a single plain path element made of letters,
// digits, '-', '_' and '.', and not "." or "..".
func validIdentifier(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// reconcileGatedTool installs or refreshes one gated tool's skill copy.
// skipped is true when the tool is inactive (marker or rootDir absent);
// the Outcome is then empty. taken maps the canonical skill paths the
// regular tools reconciled this tick to their tool names, so a gated row
// that points at one of them is refused instead of rewriting it with
// different bytes on every tick.
func reconcileGatedTool(gt ManifestGatedTool, entrypoint, home string, skillBody []byte, taken map[string]string, dryRun bool) (o Outcome, skipped bool) {
	o = Outcome{Tool: gt.Name, Kind: KindSkill}
	fail := func(err error) (Outcome, bool) {
		o.Action = ActionError
		o.Err = err.Error()
		return o, false
	}

	marker, err := gatedMarkerPath(gt, home)
	if err != nil {
		o.Path = gt.RequireMarker
		return fail(err)
	}
	if !markerPresent(marker) {
		return Outcome{}, true
	}
	t, err := resolveGatedTarget(gt, entrypoint, home)
	if err != nil {
		o.Path = gt.SkillsDir
		return fail(err)
	}
	o.Path = t.path
	if !dirExists(t.rootDir) {
		return Outcome{}, true
	}
	if owner, ok := taken[canonicalPath(t.path)]; ok {
		return fail(fmt.Errorf("%s is also the %s skill copy; refusing to write it twice", t.path, owner))
	}
	want, err := formatSkill(skillBody, gt.SkillFormat, entrypoint)
	if err != nil {
		return fail(err)
	}
	o.Hash = sha256Hex(want)

	root, err := os.OpenRoot(t.rootDir)
	if err != nil {
		return fail(err)
	}
	defer root.Close()

	state, err := classifyGatedSkill(root, t.skillRel, o.Hash)
	if err != nil {
		return fail(err)
	}
	o.State = state
	o.Action = actionFor(state)
	if o.Action != ActionNoop && !dryRun {
		if err := writeGatedSkill(root, t.skillRel, want); err != nil {
			return fail(err)
		}
	}
	return o, false
}

// removeGatedTool is Uninstall for one gated tool: it deletes the skill
// copy and, for the directory layout, the entrypoint directory if that
// leaves it empty. It returns no rows while the tool is inactive, since
// nothing under rootDir may be touched then.
func removeGatedTool(gt ManifestGatedTool, entrypoint, home string) []Removal {
	r := Removal{Tool: gt.Name, Kind: KindSkill}
	fail := func(err error) []Removal {
		r.Action = RemovalError
		r.Err = err.Error()
		return []Removal{r}
	}

	marker, err := gatedMarkerPath(gt, home)
	if err != nil {
		r.Path = gt.RequireMarker
		return fail(err)
	}
	if !markerPresent(marker) {
		return nil
	}
	t, err := resolveGatedTarget(gt, entrypoint, home)
	if err != nil {
		r.Path = gt.SkillsDir
		return fail(err)
	}
	r.Path = t.path
	if !dirExists(t.rootDir) {
		r.Action = RemovalNoop
		return []Removal{r}
	}
	root, err := os.OpenRoot(t.rootDir)
	if err != nil {
		return fail(err)
	}
	defer root.Close()

	if err := checkNoLinks(root, t.skillRel); err != nil {
		return fail(err)
	}
	fi, err := root.Lstat(t.skillRel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Action = RemovalNoop
		return []Removal{r}
	case err != nil:
		return fail(err)
	case !fi.Mode().IsRegular():
		return fail(fmt.Errorf("%s is not a regular file; leaving it in place", t.path))
	}
	if err := root.Remove(t.skillRel); err != nil {
		return fail(err)
	}
	r.Action = RemovalDeleted
	if dir := filepath.Dir(t.skillRel); !t.flat && dir != "." && filepath.Base(dir) == entrypoint {
		// Remove fails on a directory that still has entries, which is
		// the emptiness check.
		_ = root.Remove(dir)
	}
	return []Removal{r}
}

// classifyGatedSkill is classifySkill for a file under root.
func classifyGatedSkill(root *os.Root, rel, wantHash string) (State, error) {
	if err := checkNoLinks(root, rel); err != nil {
		return "", err
	}
	cur, err := root.ReadFile(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return StateAbsent, nil
	}
	if err != nil {
		return "", err
	}
	if sha256Hex(cur) == wantHash {
		return StateIdentical, nil
	}
	return StateDrifted, nil
}

// checkNoLinks fails when rel, or any directory between root and rel, is
// a symlink. A component that does not exist ends the check: nothing
// below it exists either.
func checkNoLinks(root *os.Root, rel string) error {
	cur := ""
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		cur = filepath.Join(cur, part)
		fi, err := root.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; refusing to follow it", filepath.Join(root.Name(), cur))
		}
	}
	return nil
}

// writeGatedSkill writes content to rel under root: missing directories
// are created, the bytes go to an O_EXCL temp file with a random name in
// the same directory, and that file is renamed over rel.
func writeGatedSkill(root *os.Root, rel string, content []byte) error {
	dir := filepath.Dir(rel)
	if dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := checkNoLinks(root, rel); err != nil {
		return err
	}
	f, tmp, err := createGatedTemp(root, dir, filepath.Base(rel))
	if err != nil {
		return err
	}
	_, werr := f.Write(content)
	if werr == nil {
		werr = f.Chmod(0o644) // the mode writeFile gives skill copies
	}
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

// createGatedTemp creates and opens a file named ".<base>.pilot-<random>"
// in dir with O_EXCL. It returns the file and its path relative to root.
func createGatedTemp(root *os.Root, dir, base string) (*os.File, string, error) {
	for range 8 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, "", err
		}
		name := filepath.Join(dir, "."+base+".pilot-"+hex.EncodeToString(b[:]))
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return f, name, nil
	}
	return nil, "", fmt.Errorf("could not create a temp file in %s", filepath.Join(root.Name(), dir))
}
