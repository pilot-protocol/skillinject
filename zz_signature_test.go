// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSignFile_VerifyFile(t *testing.T) {
	t.Parallel()
	body := []byte("# skill\n")
	sig := SignFile(testSigningKey, "skills/pilotctl/SKILL.md", body)
	if !strings.HasSuffix(string(sig), "\n") || strings.Count(string(sig), "\n") != 1 {
		t.Fatalf("sig file = %q, want one base64 line", sig)
	}
	if err := VerifyFile(testPublicKey, "skills/pilotctl/SKILL.md", body, sig); err != nil {
		t.Fatalf("VerifyFile: %v", err)
	}
	// Surrounding whitespace is fine (an editor's trailing newline, CRLF).
	if err := VerifyFile(testPublicKey, "skills/pilotctl/SKILL.md", body, []byte(" "+strings.TrimSpace(string(sig))+"\r\n")); err != nil {
		t.Fatalf("VerifyFile with whitespace: %v", err)
	}

	other, _ := newTestKey(t)
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	for name, c := range map[string]struct {
		pub  ed25519.PublicKey
		name string
		body []byte
		sig  []byte
	}{
		"other key":      {other, "skills/pilotctl/SKILL.md", body, sig},
		"other name":     {testPublicKey, "heartbeats/claude-code.md", body, sig},
		"other body":     {testPublicKey, "skills/pilotctl/SKILL.md", []byte("# skill!\n"), sig},
		"raw signature":  {testPublicKey, "skills/pilotctl/SKILL.md", body, raw},
		"short":          {testPublicKey, "skills/pilotctl/SKILL.md", body, []byte(base64.StdEncoding.EncodeToString(raw[:63]))},
		"empty":          {testPublicKey, "skills/pilotctl/SKILL.md", body, nil},
		"oversized":      {testPublicKey, "skills/pilotctl/SKILL.md", body, append(sig, make([]byte, maxSigFile)...)},
		"bad key length": {ed25519.PublicKey("short"), "skills/pilotctl/SKILL.md", body, sig},
		"unsigned bytes": {testPublicKey, "x", body, SignFile(testSigningKey, "x", nil)},
	} {
		if err := VerifyFile(c.pub, c.name, c.body, c.sig); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: VerifyFile = %v, want ErrSignature", name, err)
		}
	}
}

// The domain prefix and NUL separators keep (name, body) pairs apart:
// moving bytes between the name and the body changes the message.
func TestSignatureMessage_Unambiguous(t *testing.T) {
	t.Parallel()
	a := signatureMessage("ab", []byte("c"))
	b := signatureMessage("a", []byte("bc"))
	if string(a) == string(b) {
		t.Fatal("messages collide")
	}
	if !strings.HasPrefix(string(a), signatureDomain+"\x00ab\x00") {
		t.Fatalf("message = %q", a)
	}
}

func TestSignedFiles(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
	  "version": 1,
	  "entrypoint": "pilotctl",
	  "helpers": [{"name": "h", "src": "/workflow-injection/h", "dst": "~/.pilot/bin/h"}],
	  "tools": [
	    {"name": "a", "heartbeatTemplate": "heartbeats/a.md",
	     "plugin": {"files": [{"name": "i.mjs", "src": "plugin/i.mjs"}]},
	     "plugins": [{"files": [{"name": "j.mjs", "src": "plugin/j.mjs"}, {"name": "i.mjs", "src": "plugin/i.mjs"}]}]},
	    {"name": "b", "heartbeatTemplate": "heartbeats/a.md", "selfHeartbeat": true},
	    {"name": "c", "heartbeatTemplate": ""}
	  ],
	  "retired": {"plugins": [{"files": [{"name": "old.mjs", "src": "gone/old.mjs"}]}]},
	  "referencedSkills": ["https://github.com/TeoSlayer/pilot-skills/blob/main/skills/x/SKILL.md"]
	}`)
	got, err := SignedFiles(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"heartbeats/a.md",
		"plugin/i.mjs",
		"plugin/j.mjs",
		"skills/pilotctl/SKILL.md",
		"workflow-injection/h",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SignedFiles = %q\nwant %q", got, want)
	}

	for name, bad := range map[string]string{
		"parent":      `{"entrypoint":"p","tools":[{"heartbeatTemplate":"../x.md"}]}`,
		"inner climb": `{"entrypoint":"p","helpers":[{"src":"a/../../x"}]}`,
		"backslash":   `{"entrypoint":"p","helpers":[{"src":"a\\b"}]}`,
		"entrypoint":  `{"entrypoint":"../..","tools":[]}`,
		"no entry":    `{"tools":[]}`,
		"not object":  `[1]`,
		"not json":    `{`,
	} {
		if _, err := SignedFiles([]byte(bad)); err == nil {
			t.Errorf("%s: SignedFiles(%s) succeeded", name, bad)
		}
	}
}

// The gated fixture (a real manifest shape: "plugins" rows the current
// decoder ignores, gatedTools) lists its plugin sources too.
func TestSignedFiles_GatedFixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(gatedManifestFixture)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SignedFiles(raw)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"skills/pilotctl/SKILL.md", "heartbeats/claude-code.md", "index.mjs"} {
		if !strings.Contains(joined, want) {
			t.Errorf("SignedFiles = %v, missing %s", got, want)
		}
	}
}

func TestManifestSignedName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		DefaultManifestURL:                                ManifestName,
		"http://127.0.0.1:9/m.json":                       "m.json",
		"http://127.0.0.1:9/a/b/inject-manifest.json?x=1": ManifestName,
		"::not a url":                                     ManifestName,
	} {
		if got := manifestSignedName(in); got != want {
			t.Errorf("manifestSignedName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultManifestPublicKeyHex_Decodes(t *testing.T) {
	t.Parallel()
	k, err := ParsePublicKey(DefaultManifestPublicKeyHex)
	if err != nil || len(k) != ed25519.PublicKeySize {
		t.Fatalf("built-in key %q: %v", DefaultManifestPublicKeyHex, err)
	}
	if DefaultManifestPublicKeyHex != strings.ToLower(DefaultManifestPublicKeyHex) || len(DefaultManifestPublicKeyHex) != 64 {
		t.Fatalf("built-in key should be 64 lowercase hex characters")
	}
}
