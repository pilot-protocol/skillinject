// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !no_skillinject
// +build !no_skillinject

package skillinject

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneEmptyParent_BasenameMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Parent basename is the temp dir name — definitely not "pilot-protocol".
	pruneEmptyParent(filepath.Join(dir, "child"))
	// The dir must still exist.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir removed despite wrong basename: %v", err)
	}
}

func TestPruneEmptyParent_NonEmptyDirNotRemoved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "pilot-protocol")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Add a file so dir isn't empty.
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("y"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pruneEmptyParent(filepath.Join(dir, "child"))
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("non-empty dir removed: %v", err)
	}
}

func TestPruneEmptyParent_EmptyPilotProtocolDirRemoved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "pilot-protocol")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pruneEmptyParent(filepath.Join(dir, "child"))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("empty pilot-protocol dir should be removed: %v", err)
	}
}

func TestDirIsEmpty_NonExistentReturnsFalse(t *testing.T) {
	t.Parallel()
	if dirIsEmpty("/no/such/path") {
		t.Error("non-existent path: want false")
	}
}

func TestDirIsEmpty_EmptyDirReturnsTrue(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if !dirIsEmpty(dir) {
		t.Error("empty dir: want true")
	}
}

func TestDirIsEmpty_NonEmptyReturnsFalse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("y"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if dirIsEmpty(dir) {
		t.Error("non-empty dir: want false")
	}
}
