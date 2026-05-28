// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !no_skillinject
// +build !no_skillinject

package skillinject

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilot-protocol/common/coreapi"
)

func TestSetEnabled_RoundtripsThroughDisk(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := SetEnabled(home, false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if IsEnabled(home) {
		t.Error("after SetEnabled(false): IsEnabled = true, want false")
	}
	if err := SetEnabled(home, true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if !IsEnabled(home) {
		t.Error("after SetEnabled(true): IsEnabled = false, want true")
	}
}

func TestSetEnabled_PreservesOtherKeys(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".pilot")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Seed an existing config with an unrelated key.
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"),
		[]byte(`{"other_key":"preserved"}`), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := SetEnabled(home, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(cfgDir, "config.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["other_key"]; !ok {
		t.Error("other_key should be preserved across SetEnabled")
	}
	if _, ok := raw["skill_inject"]; !ok {
		t.Error("skill_inject key missing after SetEnabled")
	}
}

func TestIsEnabled_MissingFileDefaultsTrue(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if !IsEnabled(home) {
		t.Error("missing config: want true (opt-out)")
	}
}

func TestIsEnabled_BadJSONDefaultsTrue(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".pilot")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("not json"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !IsEnabled(home) {
		t.Error("bad JSON: want default true")
	}
}

func TestIsEnabled_MissingSubkeyDefaultsTrue(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".pilot")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"unrelated":1}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !IsEnabled(home) {
		t.Error("missing subkey: want default true")
	}
}

func TestIsEnabled_BadSubkeyDefaultsTrue(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".pilot")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// skill_inject value is not the expected EnabledFlag shape.
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"),
		[]byte(`{"skill_inject":42}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !IsEnabled(home) {
		t.Error("bad subkey type: want default true")
	}
}

func TestService_Lifecycle(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	s := NewService(cfg)
	if s == nil {
		t.Fatal("NewService returned nil")
	}
	if s.Name() != "skillinject" {
		t.Errorf("Name = %q", s.Name())
	}
	if s.Order() != 200 {
		t.Errorf("Order = %d, want 200", s.Order())
	}

	// Start with a context that cancels right away so Run exits cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx, coreapi.Deps{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestService_StopWithoutStart(t *testing.T) {
	t.Parallel()
	s := NewService(Config{})
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop without Start: %v", err)
	}
}
