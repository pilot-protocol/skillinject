// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !no_skillinject
// +build !no_skillinject

package skillinject

import (
	"testing"
)

func TestRemoveAllowListEntry_HappyAndMissing(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"allowlist": map[string]any{
			"plugins": []any{"a", "b", "c"},
		},
	}
	// Remove existing entry.
	if !removeAllowListEntry(obj, "allowlist.plugins", "b") {
		t.Error("removeAllowListEntry should have removed 'b'")
	}
	// Verify b was removed.
	arr, _ := obj["allowlist"].(map[string]any)["plugins"].([]any)
	if len(arr) != 2 {
		t.Errorf("arr len = %d, want 2", len(arr))
	}

	// Removing again should return false.
	if removeAllowListEntry(obj, "allowlist.plugins", "b") {
		t.Error("second removeAllowListEntry should return false")
	}
}

func TestRemoveAllowListEntry_ParentMissing(t *testing.T) {
	t.Parallel()
	if removeAllowListEntry(map[string]any{}, "nope.x", "id") {
		t.Error("parent missing: want false")
	}
}

func TestRemoveAllowListEntry_NotAnArray(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"plugins": "not-an-array",
	}
	if removeAllowListEntry(obj, "plugins", "id") {
		t.Error("non-array: want false")
	}
}

func TestRemoveEntriesEntry_HappyAndMissing(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"entries": map[string]any{
			"id-1": "value-1",
			"id-2": "value-2",
		},
	}
	if !removeEntriesEntry(obj, "entries", "id-1") {
		t.Error("removeEntriesEntry should have removed id-1")
	}
	entries, _ := obj["entries"].(map[string]any)
	if _, ok := entries["id-1"]; ok {
		t.Error("id-1 should be deleted")
	}

	// Second removal returns false.
	if removeEntriesEntry(obj, "entries", "id-1") {
		t.Error("second removeEntriesEntry should return false")
	}
}

func TestRemoveEntriesEntry_ParentMissing(t *testing.T) {
	t.Parallel()
	if removeEntriesEntry(map[string]any{}, "nope.x", "id") {
		t.Error("parent missing: want false")
	}
}

func TestRemoveEntriesEntry_NotAMap(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"entries": []any{"not-a-map"},
	}
	if removeEntriesEntry(obj, "entries", "id") {
		t.Error("non-map: want false")
	}
}
