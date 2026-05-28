// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

// Regression for P0 secret leakage: the `.pilot-bak` sidecar mirrors
// the operator's openclaw.json byte-for-byte, including any embedded
// API keys / tokens / credentials. The original implementation wrote
// it with mode 0o644 — world-readable on any multi-user host. Any
// local user (or any process running as a different UID) could read
// the operator's secrets via the backup file.
//
// Fix: write .pilot-bak with mode 0o600 (user-only read/write).
// Strictly tighter — no caller besides the same skillinject process
// reads this file, and it's removed on uninstall.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPilotBakWrittenWith0600(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "openclaw.json")

	// Seed with content that triggers a real merge (and therefore a
	// backup write).
	original := map[string]any{
		"gateway":   map[string]any{"mode": "auto"},
		"someToken": "super-secret-credential-blob",
	}
	writeJSON(t, cfg, original)

	if err := mergePluginAllowList(cfg, "plugins.allow", "plugins.entries", "pilot"); err != nil {
		t.Fatalf("merge: %v", err)
	}

	st, err := os.Stat(cfg + BackupSuffix)
	if err != nil {
		t.Fatalf("expected .pilot-bak at %s%s; got: %v", cfg, BackupSuffix, err)
	}

	// We tolerate file modes <= 0600 (e.g. 0600 or stricter via umask).
	// We REJECT anything that allows group or other to read.
	mode := st.Mode().Perm()
	if mode&0o077 != 0 {
		t.Fatalf(".pilot-bak file mode = %o, want no group/other access (≤ 0600). "+
			"This file mirrors openclaw.json byte-for-byte and may contain "+
			"operator credentials.", mode)
	}
}
