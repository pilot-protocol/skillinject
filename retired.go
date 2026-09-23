// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

// Retired surfaces.
//
// The manifest says what the injector manages now. When a path or plugin
// drops out of it, nothing in the current manifest points at the old copy,
// so neither reconcile nor Uninstall reached it and the stale directive
// kept loading. The concrete case: OpenClaw's heartbeat moved from
// workspace/HEARTBEAT.md to workspace/AGENTS.md, but the old HEARTBEAT.md
// block stayed, and the retired pilotprotocol-prompt-injector plugin (still
// trusted and enabled in openclaw.json) kept prepending it, with the user's
// own file text, to every turn. `pilotctl skills disable all` left both
// behind.
//
// The retired list closes that gap. Every tick removes what is on it,
// including ticks on a host in disabled mode (see collectRetiredWhileDisabled),
// and Uninstall removes it too. It merges two sources:
//
//   - builtinRetired: surfaces released manifests are known to have
//     installed and have since dropped.
//   - Manifest.Retired: lets pilot-skills retire a path in the same change
//     that stops writing it, without waiting for a skillinject release.
//
// Safety rules, the same for both sources:
//
//   - Anything the current manifest still manages is skipped, so a path
//     that is adopted again later is never written and then stripped in
//     alternate ticks. Paths are compared after resolving symlinks, so a
//     retired file that links to an active one is skipped too.
//   - Marker files are user-owned: only our marker block is stripped and
//     the file is kept.
//   - Plugin files are deleted only from a directory whose name is the
//     plugin id, only for the listed file names, and only after the id has
//     been taken out of the tool's allow-list. A config that cannot be
//     parsed is never rewritten. OpenClaw accepts JSON5, so comments and
//     trailing commas are normal there. The files are then kept, so the
//     tool never trusts a plugin with no files. A plugin with a known
//     no-op stub (retiredPluginStubs) has its entry file replaced by the
//     stub, reported once. Any other plugin gets an error row.
//   - Helpers are deleted only from under ~/.pilot.
//   - Only surfaces still present on disk produce a report row, so a host
//     that never had them sees nothing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ManifestRetired lists surfaces an earlier manifest installed that the
// current one no longer manages. It is the "retired" key of
// inject-manifest.json.
type ManifestRetired struct {
	// Markers are heartbeat files our marker block was written into. The
	// block is stripped and the file is kept.
	Markers []RetiredMarker `json:"markers,omitempty"`
	// Plugins are plugin installs to remove: the id is dropped from the
	// allow-list config (when AllowList is set), then each listed file
	// is deleted from InstallPath and the directory is removed if empty.
	// File Src fields are ignored.
	Plugins []ManifestPlugin `json:"plugins,omitempty"`
	// Helpers are helper scripts to delete. Src and Mode are ignored.
	// Only paths under ~/.pilot are acted on.
	Helpers []ManifestHelper `json:"helpers,omitempty"`
}

// RetiredMarker is a heartbeat file that held our marker block under an
// earlier manifest.
type RetiredMarker struct {
	// Tool is the tool name used in reports (e.g. "openclaw").
	Tool string `json:"tool"`
	// Path is the heartbeat file. Supports ~/ expansion.
	Path string `json:"path"`
}

// builtinRetired returns the surfaces released pilot-skills manifests
// installed and later dropped. Entries only ever get added here.
func builtinRetired() ManifestRetired {
	return ManifestRetired{
		Markers: []RetiredMarker{
			// OpenClaw's heartbeat moved to workspace/AGENTS.md
			// (pilot-skills ae13411).
			{Tool: "openclaw", Path: "~/.openclaw/workspace/HEARTBEAT.md"},
			// PicoClaw's heartbeat moved from workspace/AGENT.md to
			// workspace/HEARTBEAT.md (pilot-skills dbfc225).
			{Tool: "picoclaw", Path: "~/.picoclaw/workspace/AGENT.md"},
		},
		Plugins: []ManifestPlugin{{
			// Per-turn prompt injector for OpenClaw. Its marker regex
			// predates the multi-line begin comment, so it now prepends
			// the whole of workspace/HEARTBEAT.md (up to 4000 chars)
			// instead of just our block.
			ID:          "pilotprotocol-prompt-injector",
			InstallPath: "~/.openclaw/extensions/pilotprotocol-prompt-injector",
			Files: []ManifestPluginFile{
				{Name: "openclaw.plugin.json"},
				{Name: "index.mjs"},
			},
			AllowList: &ManifestPluginAllowList{
				ConfigPath:        "~/.openclaw/openclaw.json",
				AllowListJsonPath: "plugins.allow",
				EntriesJsonPath:   "plugins.entries",
			},
		}},
		Helpers: []ManifestHelper{
			// Shipped as a manifest helper in pilot-skills 55b70d5,
			// dropped in 8842310.
			{Name: "pilot-ask", Dst: "~/.pilot/bin/pilot-ask"},
		},
	}
}

// retiredItems is the resolved retired list for one manifest: absolute
// paths, deduplicated, with every surface the manifest still manages
// removed.
type retiredItems struct {
	markers []retiredMarker
	plugins []retiredPlugin
	helpers []retiredHelper
}

type retiredMarker struct{ tool, path string }

type retiredPlugin struct {
	id         string
	installDir string
	files      []string                 // absolute, each inside installDir
	allowList  *ManifestPluginAllowList // nil = no config entry to undo
	cfgPath    string                   // resolved allowList.ConfigPath
	stubs      map[string]string        // file base name -> no-op content
}

// retiredPluginStubs maps a built-in retired plugin id to no-op
// replacements for its files. They are used only when the id cannot be
// taken out of the tool's config (see neutralizeRetiredPlugin).
var retiredPluginStubs = map[string]map[string]string{
	"pilotprotocol-prompt-injector": {"index.mjs": promptInjectorStub},
}

// promptInjectorStub replaces the retired prompt injector's index.mjs. It
// keeps the plugin's id and export shape, so OpenClaw still loads the id
// it trusts, and its register() adds no hook.
const promptInjectorStub = `// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Retired Pilot Protocol prompt injector, neutralized by pilot-daemon.
//
// This plugin is retired. pilot-daemon removes a retired plugin by taking
// its id out of plugins.allow and plugins.entries in
// ~/.openclaw/openclaw.json and then deleting this directory. That file
// could not be read as strict JSON (for example it has comments or
// trailing commas), and pilot-daemon does not rewrite a file it cannot
// parse. It replaced the plugin code with this no-op instead.
//
// To finish the removal, delete "pilotprotocol-prompt-injector" from
// plugins.allow and plugins.entries. The next pilot-daemon tick then
// deletes this directory.

export default {
  id: "pilotprotocol-prompt-injector",
  name: "Pilot Protocol Prompt Injector (retired)",
  description: "Retired. Registers nothing.",
  register() {},
};
`

type retiredHelper struct{ name, path string }

// collectRetired merges builtinRetired with m.Retired and resolves it
// against home, skipping anything m still manages and anything that falls
// outside the safety rules in the file comment.
func collectRetired(m *Manifest, home string) retiredItems {
	resolve := func(p string) (string, bool) {
		if p == "" {
			return "", false
		}
		abs := filepath.Clean(expandHome(p, home))
		return abs, filepath.IsAbs(abs)
	}

	// Markers and plugin dirs are keyed by the path they resolve to: a
	// retired heartbeat that is a symlink to an active one (or the other
	// way round) is the same file, and stripping it would undo the active
	// write on every tick.
	activeMarkers := map[string]bool{}
	activeHelpers := map[string]bool{}
	activePluginIDs := map[string]bool{}
	activePluginDirs := map[string]bool{}
	for _, mt := range m.Tools {
		if p, ok := resolve(mt.HeartbeatPath); ok {
			activeMarkers[canonicalPath(p)] = true
		}
		if mt.Plugin != nil {
			activePluginIDs[mt.Plugin.ID] = true
			if p, ok := resolve(mt.Plugin.InstallPath); ok {
				activePluginDirs[canonicalPath(p)] = true
			}
		}
	}
	for _, h := range m.Helpers {
		if p, ok := resolve(h.Dst); ok {
			activeHelpers[p] = true
		}
	}

	sources := []ManifestRetired{builtinRetired()}
	if m.Retired != nil {
		sources = append(sources, *m.Retired)
	}
	pilotDir := filepath.Join(home, ".pilot")

	var out retiredItems
	seen := map[string]bool{}
	for _, src := range sources {
		for _, rm := range src.Markers {
			p, ok := resolve(rm.Path)
			if !ok {
				continue
			}
			key := canonicalPath(p)
			if activeMarkers[key] || seen["marker:"+key] {
				continue
			}
			seen["marker:"+key] = true
			out.markers = append(out.markers, retiredMarker{tool: rm.Tool, path: p})
		}
		for _, rp := range src.Plugins {
			dir, ok := resolve(rp.InstallPath)
			if !ok || rp.ID == "" || filepath.Base(dir) != rp.ID {
				continue
			}
			if activePluginIDs[rp.ID] || activePluginDirs[canonicalPath(dir)] || seen["plugin:"+rp.ID] {
				continue
			}
			seen["plugin:"+rp.ID] = true
			x := retiredPlugin{id: rp.ID, installDir: dir, stubs: retiredPluginStubs[rp.ID]}
			for _, f := range rp.Files {
				dst := filepath.Join(dir, f.Name)
				if f.Name == "" || dst == dir || !pathWithin(dir, dst) {
					continue
				}
				x.files = append(x.files, dst)
			}
			if rp.AllowList != nil {
				if cfg, ok := resolve(rp.AllowList.ConfigPath); ok {
					x.allowList = rp.AllowList
					x.cfgPath = cfg
				}
			}
			out.plugins = append(out.plugins, x)
		}
		for _, rh := range src.Helpers {
			p, ok := resolve(rh.Dst)
			if !ok || p == pilotDir || !pathWithin(pilotDir, p) {
				continue
			}
			if activeHelpers[p] || seen["helper:"+p] {
				continue
			}
			seen["helper:"+p] = true
			out.helpers = append(out.helpers, retiredHelper{name: rh.Name, path: p})
		}
	}
	return out
}

// collectRetiredWhileDisabled is the retired list for a host in disabled
// mode: the built-in list plus the "retired" key of the manifest the last
// tick cached. Nothing is managed while disabled, so no active surface is
// skipped. It never touches the network; with no readable cache it is the
// built-in list alone.
func collectRetiredWhileDisabled(home string) retiredItems {
	m := &Manifest{}
	if raw, err := os.ReadFile(filepath.Join(cacheDir(home), manifestCacheRel)); err == nil {
		var cached Manifest
		if json.Unmarshal(raw, &cached) == nil {
			m.Retired = cached.Retired
		}
	}
	return collectRetired(m, home)
}

// retiredResult is one retired surface that was removed, or in a dry run
// would be. action is RemovalStripped, RemovalDeleted, RemovalMerged,
// RemovalNeutralized or RemovalError.
type retiredResult struct {
	tool   string
	kind   FileKind
	path   string
	action RemovalKind
	err    string
	note   string
}

// applyRetired removes every retired surface still present on disk. With
// dryRun it only reports what it would remove.
func applyRetired(items retiredItems, dryRun bool) []retiredResult {
	var out []retiredResult
	for _, rm := range items.markers {
		if !fileHasMarker(rm.path) {
			continue
		}
		r := retiredResult{tool: rm.tool, kind: KindMarker, path: rm.path, action: RemovalStripped}
		if !dryRun {
			if x := stripMarkerFile(rm.tool, rm.path); x.Action == RemovalError {
				r.action, r.err = RemovalError, x.Err
			}
		}
		out = append(out, r)
	}
	for _, rp := range items.plugins {
		out = append(out, applyRetiredPlugin(rp, dryRun)...)
	}
	for _, rh := range items.helpers {
		if !ownedFilePresent(rh.path) {
			continue
		}
		r := retiredResult{tool: rh.name, kind: KindHelper, path: rh.path, action: RemovalDeleted}
		if !dryRun {
			if err := os.Remove(rh.path); err != nil && !os.IsNotExist(err) {
				r.action, r.err = RemovalError, err.Error()
			}
		}
		out = append(out, r)
	}
	return out
}

// applyRetiredPlugin removes one retired plugin: allow-list entry first,
// then its files, then its directory if that left it empty.
func applyRetiredPlugin(rp retiredPlugin, dryRun bool) []retiredResult {
	var present []string
	for _, f := range rp.files {
		if ownedFilePresent(f) {
			present = append(present, f)
		}
	}

	var out []retiredResult
	// A tool that still trusts and enables the id but finds its files gone
	// may refuse to load, so files go only once the id is out of the
	// config (or was never in it).
	if rp.allowList != nil {
		listed, err := pluginInConfig(rp.cfgPath, rp.allowList, rp.id)
		switch {
		case err != nil:
			if len(present) == 0 {
				// Nothing of the plugin is on disk; an unreadable config
				// is not ours to report on every tick.
				return nil
			}
			if len(rp.stubs) > 0 {
				return neutralizeRetiredPlugin(rp, present, err, dryRun)
			}
			return []retiredResult{{
				tool: rp.id, kind: KindPluginAllowList, path: rp.cfgPath, action: RemovalError,
				err: fmt.Sprintf("%v; leaving the retired plugin's files in place", err),
			}}
		case listed:
			r := retiredResult{tool: rp.id, kind: KindPluginAllowList, path: rp.cfgPath, action: RemovalMerged}
			if !dryRun {
				if err := dropPluginFromConfig(rp.cfgPath, rp.allowList, rp.id); err != nil {
					r.action, r.err = RemovalError, err.Error()
					return append(out, r)
				}
			}
			out = append(out, r)
		}
	}

	for _, f := range present {
		r := retiredResult{tool: rp.id, kind: KindPluginFile, path: f, action: RemovalDeleted}
		if !dryRun {
			if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
				r.action, r.err = RemovalError, err.Error()
			}
		}
		out = append(out, r)
	}
	if !dryRun && dirIsEmpty(rp.installDir) {
		_ = os.Remove(rp.installDir)
	}
	return out
}

// neutralizeRetiredPlugin handles a retired plugin whose id cannot be taken
// out of the tool's config, because the config cannot be read or does not
// parse as strict JSON. Deleting the files would leave the tool trusting
// an id with nothing on disk, and rewriting a JSON5 file would drop the
// user's comments. Instead each present file that has a stub is overwritten
// with it, so the id still resolves and the plugin does nothing. A file
// that already holds its stub is skipped: the change is reported once,
// not as an error on every tick. Once the config parses, the next tick
// removes the id and deletes the files as usual.
func neutralizeRetiredPlugin(rp retiredPlugin, present []string, cfgErr error, dryRun bool) []retiredResult {
	var out []retiredResult
	for _, f := range present {
		stub, ok := rp.stubs[filepath.Base(f)]
		if !ok {
			continue
		}
		if cur, err := os.ReadFile(f); err == nil && string(cur) == stub {
			continue
		}
		r := retiredResult{
			tool: rp.id, kind: KindPluginFile, path: f, action: RemovalNeutralized,
			note: fmt.Sprintf("%v; %s is replaced with a no-op, so the retired plugin stays listed but does nothing. Remove %q from %s and %s to finish; the next tick then deletes the plugin",
				cfgErr, filepath.Base(f), rp.id, rp.allowList.AllowListJsonPath, rp.allowList.EntriesJsonPath),
		}
		if !dryRun {
			if err := writeFileAtomic(f, []byte(stub), 0o644); err != nil {
				r.action, r.err, r.note = RemovalError, err.Error(), ""
			}
		}
		out = append(out, r)
	}
	return out
}

// pruneRetired is the reconcile side of applyRetired: one Outcome per
// retired surface present on disk, State retired, Action remove (rewrite
// for a neutralized plugin file, error on failure).
func pruneRetired(items retiredItems, dryRun bool) []Outcome {
	res := applyRetired(items, dryRun)
	out := make([]Outcome, 0, len(res))
	for _, r := range res {
		o := Outcome{Tool: r.tool, Kind: r.kind, Path: r.path, State: StateRetired, Action: ActionRemove, Note: r.note}
		switch r.action {
		case RemovalError:
			o.Action = ActionError
			o.Err = r.err
		case RemovalNeutralized:
			o.Action = ActionRewrite
		}
		out = append(out, o)
	}
	return out
}

// removeRetired is the Uninstall side of applyRetired.
func removeRetired(items retiredItems) []Removal {
	res := applyRetired(items, false)
	out := make([]Removal, 0, len(res))
	for _, r := range res {
		out = append(out, Removal{Tool: r.tool, Kind: r.kind, Path: r.path, Action: r.action, Err: r.err, Note: r.note})
	}
	return out
}

// fileHasMarker reports whether path is readable and holds a complete
// marker block.
func fileHasMarker(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && len(findMarkers(string(b))) > 0
}

// ownedFilePresent reports whether path exists as a regular file or a
// symlink, the only things a retired owned file is deleted as. A directory
// at that path is not ours to remove.
func ownedFilePresent(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0
}

// pluginInConfig reports whether id is in the allow-list or the entries
// map of the JSON config at cfgPath. A missing file is (false, nil).
func pluginInConfig(cfgPath string, al *ManifestPluginAllowList, id string) (bool, error) {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	obj, err := decodeConfig(raw)
	if err != nil {
		return false, fmt.Errorf("parse %s (refusing to edit): %w", cfgPath, err)
	}
	return allowListContains(obj, al.AllowListJsonPath, id) || entriesHas(obj, al.EntriesJsonPath, id), nil
}

// dropPluginFromConfig removes id from the allow-list array and the
// entries map of the JSON config at cfgPath, keeping everything else.
//
// It edits the live config of a running tool, so it is careful about what
// it writes: numbers are kept as written (no float64 round-trip that would
// corrupt a large chat or account id), strings are not HTML-escaped, the
// result is re-parsed and must still carry every original top-level key,
// the file mode is kept (the config can hold credentials), and a symlinked
// config is edited at its target so dotfile setups keep their link. It
// does not touch the .pilot-bak snapshot, which belongs to the active
// plugin merge in plugin_allowlist.go.
func dropPluginFromConfig(cfgPath string, al *ManifestPluginAllowList, id string) error {
	target, err := filepath.EvalSymlinks(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	obj, err := decodeConfig(raw)
	if err != nil {
		return fmt.Errorf("parse %s (refusing to edit): %w", cfgPath, err)
	}
	topKeys := make([]string, 0, len(obj))
	for k := range obj {
		topKeys = append(topKeys, k)
	}

	changed := removeAllowListEntry(obj, al.AllowListJsonPath, id)
	if removeEntriesEntry(obj, al.EntriesJsonPath, id) {
		changed = true
	}
	if !changed {
		return nil
	}

	next, err := encodeConfig(obj)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", cfgPath, err)
	}
	check, err := decodeConfig(next)
	if err != nil {
		return fmt.Errorf("verify %s before write: %w", cfgPath, err)
	}
	for _, k := range topKeys {
		if _, ok := check[k]; !ok {
			return fmt.Errorf("verify %s before write: top-level key %q lost", cfgPath, k)
		}
	}
	if allowListContains(check, al.AllowListJsonPath, id) || entriesHas(check, al.EntriesJsonPath, id) {
		return fmt.Errorf("verify %s before write: %q still present", cfgPath, id)
	}

	perm := info.Mode().Perm()
	if err := writeFileAtomic(target, next, perm); err != nil {
		return err
	}
	// os.WriteFile only applies perm when it creates the temp file.
	_ = os.Chmod(target, perm)
	return nil
}

// decodeConfig parses a JSON object, keeping numbers as json.Number and
// rejecting trailing data after the object.
func decodeConfig(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("unexpected data after the top-level JSON object")
	}
	return obj, nil
}

// encodeConfig writes obj with 2-space indent (the same layout
// mergePluginAllowList produces), no HTML escaping, and a trailing newline.
func encodeConfig(obj map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// entriesHas reports whether the entries object at jsonPath has a key id,
// whatever its value.
func entriesHas(obj map[string]any, jsonPath, id string) bool {
	parent, leaf := walkObject(obj, jsonPath, false)
	if parent == nil {
		return false
	}
	entries, ok := parent[leaf].(map[string]any)
	if !ok {
		return false
	}
	_, present := entries[id]
	return present
}
