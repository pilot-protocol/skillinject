// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// configFileName is the per-user pilot config file. The same file the
// rest of pilotctl reads from. We only touch known subkeys.
const configFileName = "config.json"

// configUpdateKey is the subkey under which we store the update settings.
const configUpdateKey = "update"

// configKey is the subkey under which we store the mode flag.
const configKey = "skill_inject"

// Mode names for the tri-state skill_inject.mode setting.
const (
	// ModeManual: install once + refresh on Pilot update, no 15-min ticker.
	ModeManual = "manual"
	// ModeAuto: current always-live behaviour (15-min reconcile ticker).
	ModeAuto = "auto"
	// ModeDisabled: remove skills + no ticks.
	ModeDisabled = "disabled"
)

// defaultMode is the mode used when no persisted value exists.
const defaultMode = ModeManual

// ModeFlag describes the persisted tri-state mode. Stored at
// ~/.pilot/config.json under "skill_inject" → {"mode": "manual"|"auto"|"disabled"}.
type ModeFlag struct {
	Mode string `json:"mode,omitempty"`
}

// EnabledFlag describes the persisted opt-out state (legacy format).
// Stored at ~/.pilot/config.json under "skill_inject" → {"enabled": bool}.
// Deprecated: use ModeFlag with GetMode/SetMode instead.
type EnabledFlag struct {
	Enabled bool `json:"enabled"`
}

func configFilePath(home string) string {
	return filepath.Join(home, ".pilot", configFileName)
}

// GetMode returns the current skill_inject mode. Defaults to ModeAuto
// when the flag isn't present, so existing installs keep their current
// live-ticker behaviour. New installs will be set to ModeManual on
// the first call to SetMode (configured by the daemon's startup path).
func GetMode(home string) string {
	f, err := os.Open(configFilePath(home))
	if err != nil {
		return ModeAuto // existing behaviour — live ticker
	}
	defer f.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return ModeAuto
	}
	sub, ok := raw[configKey]
	if !ok {
		return ModeAuto
	}
	// Try the new mode-based format first.
	var modeFlag ModeFlag
	if err := json.Unmarshal(sub, &modeFlag); err == nil && modeFlag.Mode != "" {
		switch modeFlag.Mode {
		case ModeManual, ModeAuto, ModeDisabled:
			return modeFlag.Mode
		}
		// Unknown mode value — fall through to legacy.
	}
	// Fall back to the legacy enabled-based format.
	var enabledFlag EnabledFlag
	if err := json.Unmarshal(sub, &enabledFlag); err == nil {
		if enabledFlag.Enabled {
			return ModeAuto
		}
		return ModeDisabled
	}
	return ModeAuto
}

// SetMode persists the skill_inject mode. Reads the existing config (if
// any), updates only the skill_inject key, writes back atomically.
// Accepts ModeManual, ModeAuto, or ModeDisabled. Empty string is treated
// as ModeAuto for backward compatibility.
func SetMode(home, mode string) error {
	switch mode {
	case "":
		mode = ModeAuto
	case ModeManual, ModeAuto, ModeDisabled:
		// valid
	default:
		mode = ModeAuto
	}
	p := configFilePath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &raw)
	}
	flagJSON, _ := json.Marshal(ModeFlag{Mode: mode})
	raw[configKey] = flagJSON
	out, _ := json.MarshalIndent(raw, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// IsEnabled returns whether skill injection is on. Returns true for
// ModeManual and ModeAuto, false for ModeDisabled. Defaults to true
// (opt-out, not opt-in) when the flag isn't present, so fresh installs
// get the feature without any extra step.
//
// Deprecated: use GetMode to get the full tri-state.
func IsEnabled(home string) bool {
	return GetMode(home) != ModeDisabled
}

// SetEnabled persists the opt-out flag. Maps true → ModeAuto, false →
// ModeDisabled. Keeps existing Manual mode unless explicitly toggled.
//
// Deprecated: use SetMode for the full tri-state control.
func SetEnabled(home string, enabled bool) error {
	mode := ModeAuto
	if !enabled {
		mode = ModeDisabled
	}
	return SetMode(home, mode)
}

// --- update config ---

// UpdateConfig describes the runtime-tunable auto-update settings.
// Stored at ~/.pilot/config.json under "update" → {...}.
// Zero values leave the updater default in place.
type UpdateConfig struct {
	// Auto controls whether the updater checks for and applies new
	// versions. True (default) = auto-update enabled.
	Auto *bool `json:"auto,omitempty"`
	// Pin locks the updater to a specific release tag (e.g. "v1.10.5").
	// Empty string (default) means no pin — follow latest stable.
	Pin string `json:"pin,omitempty"`
	// Interval is the minimum time between update checks, in Go duration
	// string format ("15m", "1h"). Empty string (default) means use the
	// updater's built-in default check interval.
	Interval string `json:"interval,omitempty"`
}

// GetUpdateConfig reads the update subkey from ~/.pilot/config.json.
// Returns a zero-value UpdateConfig when the subkey is absent, so the
// caller can apply its own defaults.
func GetUpdateConfig(home string) UpdateConfig {
	f, err := os.Open(configFilePath(home))
	if err != nil {
		return UpdateConfig{}
	}
	defer f.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return UpdateConfig{}
	}
	sub, ok := raw[configUpdateKey]
	if !ok {
		return UpdateConfig{}
	}
	var cfg UpdateConfig
	_ = json.Unmarshal(sub, &cfg)
	return cfg
}

// SetUpdateConfig persists the update subkey. Reads the existing config
// (if any), updates only the update key, writes back atomically.
func SetUpdateConfig(home string, cfg UpdateConfig) error {
	p := configFilePath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &raw)
	}
	cfgJSON, _ := json.Marshal(cfg)
	raw[configUpdateKey] = cfgJSON
	out, _ := json.MarshalIndent(raw, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
