// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"os"
	"time"
)

// State is the classification of one managed file at the start of a tick.
type State string

const (
	// File (or marker block) does not exist on disk.
	StateAbsent State = "absent"
	// File/marker exists and matches the canonical we want.
	StateIdentical State = "identical"
	// File/marker exists but the content/hash differs from canonical.
	StateDrifted State = "drifted"
	// File/marker/plugin was installed by an earlier manifest that the
	// current one no longer manages (see retired.go). Present on disk and
	// due for removal.
	StateRetired State = "retired"
)

// Action is what the reconcile loop chose to do in response to a State.
type Action string

const (
	ActionNoop    Action = "noop"
	ActionCreate  Action = "create"
	ActionRewrite Action = "rewrite"
	ActionError   Action = "error"
	// ActionRemove: a retired surface is stripped (marker block) or
	// deleted (owned file, allow-list entry). See retired.go.
	ActionRemove Action = "remove"
)

// FileKind names which of a target's managed surfaces an Outcome is
// about.
type FileKind string

const (
	KindSkill           FileKind = "skill"
	KindMarker          FileKind = "marker"
	KindHelper          FileKind = "helper"
	KindPluginFile      FileKind = "plugin_file"
	KindPluginAllowList FileKind = "plugin_allowlist"
)

func actionFor(s State) Action {
	switch s {
	case StateAbsent:
		return ActionCreate
	case StateDrifted:
		return ActionRewrite
	default:
		return ActionNoop
	}
}

// Outcome records one reconcile decision.
type Outcome struct {
	Tool   string   `json:"tool"`
	Kind   FileKind `json:"kind"`
	Path   string   `json:"path"`
	State  State    `json:"state"`
	Action Action   `json:"action"`
	Hash   string   `json:"hash,omitempty"`
	Err    string   `json:"err,omitempty"`
}

// Report is the result of one Tick.
type Report struct {
	At       time.Time `json:"at"`
	Outcomes []Outcome `json:"outcomes"`
	Skipped  []string  `json:"skipped,omitempty"`
	// Disabled is true if the tick was a no-op because the user has run
	// `pilotctl skills disable all`.
	Disabled bool `json:"disabled,omitempty"`
}

// Counts returns how many outcomes hit each Action.
func (r *Report) Counts() map[Action]int {
	c := map[Action]int{}
	for _, o := range r.Outcomes {
		c[o.Action]++
	}
	return c
}

// classifySkill inspects path and returns the State of the skill copy.
func classifySkill(path, wantHash string) State {
	cur, err := os.ReadFile(path)
	if err != nil {
		return StateAbsent
	}
	if sha256Hex(cur) == wantHash {
		return StateIdentical
	}
	return StateDrifted
}

// classifyMarker inspects the heartbeat file at path and returns the State
// of *our* marker block within it. wantShort is the markerHash of the
// block the current tick would write.
//
// More than one marker block in the same file is always Drifted, whatever
// their hashes: writeMarker then collapses them to a single block, so a
// file that picked up a duplicate (an old release appending instead of
// replacing, a hand-merged dotfile) heals on the next tick instead of
// carrying two directives forever.
func classifyMarker(path, wantShort string) State {
	cur, err := os.ReadFile(path)
	if err != nil {
		return StateAbsent
	}
	ms := markerRE.FindAllStringSubmatch(string(cur), -1)
	switch {
	case len(ms) == 0:
		return StateAbsent
	case len(ms) > 1:
		return StateDrifted
	case ms[0][1] == wantShort:
		return StateIdentical
	default:
		return StateDrifted
	}
}

// classifyPluginFile inspects a plugin source file at path and returns
// the State by content-hash comparison. Identical → noop; drifted →
// rewrite; absent → create.
func classifyPluginFile(path, wantHash string) State {
	cur, err := os.ReadFile(path)
	if err != nil {
		return StateAbsent
	}
	if sha256Hex(cur) == wantHash {
		return StateIdentical
	}
	return StateDrifted
}
