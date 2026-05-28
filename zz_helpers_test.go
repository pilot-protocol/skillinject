// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !no_skillinject
// +build !no_skillinject

package skillinject

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandHome_TildeSlash(t *testing.T) {
	t.Parallel()
	got := expandHome("~/.config/foo", "/home/alice")
	want := filepath.Join("/home/alice", ".config/foo")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpandHome_TildeOnly(t *testing.T) {
	t.Parallel()
	if got := expandHome("~", "/home/alice"); got != "/home/alice" {
		t.Errorf("got %q, want /home/alice", got)
	}
}

func TestExpandHome_NoTildeReturnedAsIs(t *testing.T) {
	t.Parallel()
	if got := expandHome("/abs/path", "/home/x"); got != "/abs/path" {
		t.Errorf("got %q", got)
	}
	if got := expandHome("relative/path", "/home/x"); got != "relative/path" {
		t.Errorf("got %q", got)
	}
}

func TestRenderHeartbeat_BadTemplate(t *testing.T) {
	t.Parallel()
	_, err := renderHeartbeat([]byte(`{{.UnclosedAction`), heartbeatVars{})
	if err == nil {
		t.Error("expected parse error")
	}
}

func TestRenderHeartbeat_HappyPath(t *testing.T) {
	t.Parallel()
	out, err := renderHeartbeat([]byte(`entry={{.EntrypointPath}}`), heartbeatVars{EntrypointPath: "/usr/local/bin/foo"})
	if err != nil {
		t.Fatalf("renderHeartbeat: %v", err)
	}
	if !strings.Contains(out, "/usr/local/bin/foo") {
		t.Errorf("output missing entrypoint: %q", out)
	}
}
