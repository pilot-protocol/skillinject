// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilot-protocol/skillinject"
)

var testKey = ed25519.NewKeyFromSeed(func() []byte {
	s := sha256.Sum256([]byte("sign-skills test key"))
	return s[:]
}())

const manifest = `{"version":1,"entrypoint":"pilotctl",
 "helpers":[{"name":"h","src":"workflow-injection/h","dst":"~/.pilot/bin/h"}],
 "tools":[{"name":"claude-code","rootDir":"~/.claude","skillsDir":"~/.claude/skills",
   "heartbeatPath":"~/.claude/CLAUDE.md","heartbeatTemplate":"heartbeats/claude-code.md",
   "plugins":[{"id":"p","files":[{"name":"index.mjs","src":"workflow-injection/p/index.mjs"}]}]}]}`

var repoFiles = map[string]string{
	"inject-manifest.json":           manifest,
	"skills/pilotctl/SKILL.md":       "# skill\n",
	"heartbeats/claude-code.md":      "directive {{.EntrypointPath}}\n",
	"workflow-injection/h":           "#!/bin/sh\n",
	"workflow-injection/p/index.mjs": "export default {}\n",
	"skills.json":                    "{}\n", // not listed: never signed
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range repoFiles {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func seedHex() string { return hex.EncodeToString(testKey.Seed()) }

func pubHex() string { return hex.EncodeToString(testKey.Public().(ed25519.PublicKey)) }

func runCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := run(args, strings.NewReader(stdin), &out, &errOut)
	return out.String() + errOut.String(), err
}

func TestSignThenVerify(t *testing.T) {
	repo := newRepo(t)
	out, err := runCmd(t, seedHex()+"\n", "sign", "-repo", repo, "-any-key")
	if err != nil {
		t.Fatalf("sign: %v\n%s", err, out)
	}
	if strings.Contains(out, seedHex()) {
		t.Fatal("sign printed the private key")
	}
	for _, name := range []string{"inject-manifest.json", "skills/pilotctl/SKILL.md", "heartbeats/claude-code.md", "workflow-injection/h", "workflow-injection/p/index.mjs"} {
		sig, err := os.ReadFile(filepath.Join(repo, name) + ".sig")
		if err != nil {
			t.Fatalf("%s.sig: %v", name, err)
		}
		if err := skillinject.VerifyFile(testKey.Public().(ed25519.PublicKey), name, []byte(repoFiles[name]), sig); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "skills.json.sig")); !os.IsNotExist(err) {
		t.Errorf("an unlisted file was signed: %v", err)
	}

	if out, err := runCmd(t, "", "verify", "-repo", repo, "-pubkey", pubHex()); err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	// The built-in key did not sign this.
	if _, err := runCmd(t, "", "verify", "-repo", repo); err == nil {
		t.Fatal("verify against the built-in key passed for content signed by a test key")
	}

	// Tamper with one file: verify names it and fails.
	if err := os.WriteFile(filepath.Join(repo, "heartbeats", "claude-code.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runCmd(t, "", "verify", "-repo", repo, "-pubkey", pubHex())
	if err == nil || !strings.Contains(out, "FAIL heartbeats/claude-code.md") {
		t.Fatalf("verify after tampering = %v\n%s", err, out)
	}
}

func TestSign_KeyForms(t *testing.T) {
	forms := map[string]string{
		"hex seed":       seedHex(),
		"base64 seed":    base64.StdEncoding.EncodeToString(testKey.Seed()),
		"hex private":    hex.EncodeToString(testKey),
		"base64 private": base64.StdEncoding.EncodeToString(testKey),
	}
	for name, k := range forms {
		t.Run(name, func(t *testing.T) {
			repo := newRepo(t)
			if out, err := runCmd(t, k, "sign", "-repo", repo, "-any-key"); err != nil {
				t.Fatalf("sign: %v\n%s", err, out)
			}
			if out, err := runCmd(t, "", "verify", "-repo", repo, "-pubkey", pubHex()); err != nil {
				t.Fatalf("verify: %v\n%s", err, out)
			}
		})
	}
	t.Run("env", func(t *testing.T) {
		repo := newRepo(t)
		t.Setenv("TEST_SKILLS_KEY", seedHex())
		if out, err := runCmd(t, "", "sign", "-repo", repo, "-any-key", "-key-env", "TEST_SKILLS_KEY"); err != nil {
			t.Fatalf("sign: %v\n%s", err, out)
		}
	})
	t.Run("mismatched private key", func(t *testing.T) {
		bad := append(ed25519.PrivateKey(nil), testKey...)
		bad[40] ^= 1
		if _, err := runCmd(t, hex.EncodeToString(bad), "sign", "-repo", newRepo(t), "-any-key"); err == nil {
			t.Fatal("accepted a private key whose public half does not match its seed")
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, err := runCmd(t, "nope", "sign", "-repo", newRepo(t), "-any-key"); err == nil {
			t.Fatal("accepted garbage as a key")
		}
	})
}

// Without -any-key, sign refuses a key daemons would not trust.
func TestSign_RefusesKeyOtherThanBuiltIn(t *testing.T) {
	repo := newRepo(t)
	out, err := runCmd(t, seedHex(), "sign", "-repo", repo)
	if err == nil || !strings.Contains(err.Error(), "not the built-in key") {
		t.Fatalf("sign with a foreign key = %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, "inject-manifest.json.sig")); !os.IsNotExist(err) {
		t.Fatalf("a refused sign wrote a signature: %v", err)
	}
}

// A listed file that is missing, too large or outside the repo fails the
// run before any .sig is written.
func TestSign_BadListedFileWritesNothing(t *testing.T) {
	for name, breakIt := range map[string]func(repo string){
		"missing": func(repo string) { os.Remove(filepath.Join(repo, "workflow-injection", "p", "index.mjs")) },
		"too large": func(repo string) {
			os.WriteFile(filepath.Join(repo, "workflow-injection", "h"), make([]byte, maxFile+1), 0o644)
		},
		"symlink out": func(repo string) {
			p := filepath.Join(repo, "workflow-injection", "h")
			os.Remove(p)
			os.Symlink("/etc/hosts", p)
		},
		"escaping path": func(repo string) {
			os.WriteFile(filepath.Join(repo, "inject-manifest.json"), []byte(`{"entrypoint":"pilotctl","helpers":[{"src":"../outside"}]}`), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newRepo(t)
			breakIt(repo)
			if out, err := runCmd(t, seedHex(), "sign", "-repo", repo, "-any-key"); err == nil {
				t.Fatalf("sign succeeded\n%s", out)
			}
			sigs, _ := filepath.Glob(filepath.Join(repo, "*.sig"))
			more, _ := filepath.Glob(filepath.Join(repo, "*", "*", "*.sig"))
			if len(sigs)+len(more) > 0 {
				t.Fatalf("signatures written: %v %v", sigs, more)
			}
		})
	}
}

func TestGenkeyAndPubkey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "seed")
	out, err := runCmd(t, "", "genkey", "-out", p)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	pub := strings.TrimSpace(out)
	if len(pub) != 64 {
		t.Fatalf("genkey printed %q, want only the public key in hex", out)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("seed file mode %v, want 0600", st.Mode().Perm())
	}
	seed, _ := os.ReadFile(p)
	if strings.Contains(out, strings.TrimSpace(string(seed))) {
		t.Fatal("genkey printed the private key")
	}
	got, err := runCmd(t, string(seed), "pubkey")
	if err != nil || strings.TrimSpace(got) != pub {
		t.Fatalf("pubkey = %q, %v; want %s", got, err, pub)
	}
	// It never overwrites a key.
	if _, err := runCmd(t, "", "genkey", "-out", p); err == nil {
		t.Fatal("genkey overwrote an existing file")
	}
	if _, err := runCmd(t, "", "genkey"); err == nil {
		t.Fatal("genkey without -out should fail rather than print the key")
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"verify", "extra"}, {"verify", "-pubkey", "zz"}} {
		if _, err := runCmd(t, "", args...); err == nil {
			t.Errorf("run(%q) succeeded", args)
		}
	}
}
