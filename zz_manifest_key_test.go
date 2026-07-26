// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestKey returns a deterministic-enough Ed25519 keypair for tests.
func newTestKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

func TestPathWithin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		dir, path string
		want      bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/b/c/d.txt", true},
		{"/a/b/", "/a/b/c", true},
		{"/a/b", "/a/b/../c", false},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
		{"/a/b", "/a/b/../../etc/passwd", false},
		{"/a/b", "/etc/passwd", false},
	}
	for _, c := range cases {
		if got := pathWithin(c.dir, c.path); got != c.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", c.dir, c.path, got, c.want)
		}
	}
}

func TestDecodeEd25519PublicKey(t *testing.T) {
	t.Parallel()
	pub, _ := newTestKey(t)

	for name, encoded := range map[string]string{
		"hex":       hex.EncodeToString(pub),
		"base64":    base64.StdEncoding.EncodeToString(pub),
		"rawbase64": base64.RawStdEncoding.EncodeToString(pub),
		"padded":    "  " + hex.EncodeToString(pub) + "\n",
	} {
		got, err := decodeEd25519PublicKey(encoded)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
			continue
		}
		if !got.Equal(pub) {
			t.Errorf("%s: decoded key mismatch", name)
		}
	}

	for _, bad := range []string{"", "not-a-key", hex.EncodeToString(pub[:16])} {
		if _, err := decodeEd25519PublicKey(bad); err == nil {
			t.Errorf("decodeEd25519PublicKey(%q) succeeded, want error", bad)
		}
	}
}

func TestResolveManifestPublicKey_ConfigWins(t *testing.T) {
	pub, _ := newTestKey(t)
	other, _ := newTestKey(t)
	home := t.TempDir()

	t.Setenv(EnvManifestPublicKey, hex.EncodeToString(other))
	writeKeyFile(t, home, hex.EncodeToString(other))

	got, err := resolveManifestPublicKey(Config{Home: home, ManifestPublicKey: pub})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Equal(pub) {
		t.Fatalf("Config key did not take precedence")
	}
}

func TestResolveManifestPublicKey_EnvBeatsFile(t *testing.T) {
	pub, _ := newTestKey(t)
	other, _ := newTestKey(t)
	home := t.TempDir()

	t.Setenv(EnvManifestPublicKey, base64.StdEncoding.EncodeToString(pub))
	writeKeyFile(t, home, hex.EncodeToString(other))

	got, err := resolveManifestPublicKey(Config{Home: home})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Equal(pub) {
		t.Fatalf("environment key did not take precedence over the trust file")
	}
}

func TestResolveManifestPublicKey_FileFallback(t *testing.T) {
	pub, _ := newTestKey(t)
	home := t.TempDir()

	t.Setenv(EnvManifestPublicKey, "")
	writeKeyFile(t, home, hex.EncodeToString(pub))

	got, err := resolveManifestPublicKey(Config{Home: home})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Equal(pub) {
		t.Fatalf("trust file key not picked up")
	}
}

func TestResolveManifestPublicKey_NoSourceIsNilNoError(t *testing.T) {
	t.Setenv(EnvManifestPublicKey, "")
	got, err := resolveManifestPublicKey(Config{Home: t.TempDir()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil key when no source supplies one, got %d bytes", len(got))
	}
}

func TestResolveManifestPublicKey_BadEncodingErrors(t *testing.T) {
	t.Setenv(EnvManifestPublicKey, "zzzz-not-a-key")
	if _, err := resolveManifestPublicKey(Config{Home: t.TempDir()}); err == nil {
		t.Fatalf("expected an error for an undecodable environment key")
	}
}

func TestResolveManifestPublicKey_WrongSizeConfigKeyErrors(t *testing.T) {
	if _, err := resolveManifestPublicKey(Config{Home: t.TempDir(), ManifestPublicKey: ed25519.PublicKey("short")}); err == nil {
		t.Fatalf("expected an error for a wrong-size Config key")
	}
}

func TestRequireSignedManifest_EnvAndConfig(t *testing.T) {
	t.Setenv(EnvRequireSignedManifest, "")
	if requireSignedManifest(Config{}) {
		t.Fatalf("default should not require a signature")
	}
	if !requireSignedManifest(Config{RequireSignedManifest: true}) {
		t.Fatalf("Config field should require a signature")
	}
	for _, v := range []string{"1", "true", "YES"} {
		t.Setenv(EnvRequireSignedManifest, v)
		if !requireSignedManifest(Config{}) {
			t.Errorf("%q should require a signature", v)
		}
	}
	t.Setenv(EnvRequireSignedManifest, "0")
	if requireSignedManifest(Config{}) {
		t.Fatalf("%q should not require a signature", "0")
	}
}

// TestGetOrVerify_DefaultUnverified pins the backward-compatible default:
// with no key anywhere, the body is returned and no .sig is requested.
func TestGetOrVerify_DefaultUnverified(t *testing.T) {
	t.Setenv(EnvManifestPublicKey, "")
	t.Setenv(EnvRequireSignedManifest, "")

	var sigRequests int
	srv := newBodyServer(t, []byte("hello"), &sigRequests)

	f := newFetcher(Config{Home: t.TempDir(), HTTPClient: srv.Client()})
	body, err := f.getOrVerify(context.Background(), srv.URL+"/x")
	if err != nil {
		t.Fatalf("getOrVerify: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
	if sigRequests != 0 {
		t.Fatalf("expected no signature fetch, got %d", sigRequests)
	}
}

// TestGetOrVerify_RequireWithoutKeyFails pins the opt-in strict mode.
func TestGetOrVerify_RequireWithoutKeyFails(t *testing.T) {
	t.Setenv(EnvManifestPublicKey, "")
	t.Setenv(EnvRequireSignedManifest, "")

	var sigRequests int
	srv := newBodyServer(t, []byte("hello"), &sigRequests)

	f := newFetcher(Config{Home: t.TempDir(), HTTPClient: srv.Client(), RequireSignedManifest: true})
	if _, err := f.getOrVerify(context.Background(), srv.URL+"/x"); err == nil {
		t.Fatalf("expected an error when a signature is required and no key resolves")
	} else if !strings.Contains(err.Error(), EnvManifestPublicKey) {
		t.Fatalf("error should name the environment override, got %v", err)
	}
}

// TestGetOrVerify_ResolvedKeyVerifies checks that a key supplied purely
// via the trust file is picked up and used to verify.
func TestGetOrVerify_ResolvedKeyVerifies(t *testing.T) {
	t.Setenv(EnvManifestPublicKey, "")
	t.Setenv(EnvRequireSignedManifest, "")

	pub, priv := newTestKey(t)
	home := t.TempDir()
	writeKeyFile(t, home, hex.EncodeToString(pub))

	body := []byte("signed body")
	sig := ed25519.Sign(priv, body)

	mux := http.NewServeMux()
	mux.HandleFunc("/x", func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
	mux.HandleFunc("/x.sig", func(w http.ResponseWriter, _ *http.Request) { w.Write(sig) })
	mux.HandleFunc("/bad", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("tampered")) })
	mux.HandleFunc("/bad.sig", func(w http.ResponseWriter, _ *http.Request) { w.Write(sig) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f := newFetcher(Config{Home: home, HTTPClient: srv.Client()})
	got, err := f.getOrVerify(context.Background(), srv.URL+"/x")
	if err != nil {
		t.Fatalf("getOrVerify: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("body = %q", got)
	}
	if _, err := f.getOrVerify(context.Background(), srv.URL+"/bad"); err == nil {
		t.Fatalf("expected verification failure on a mismatched body")
	}
}

func writeKeyFile(t *testing.T, home, contents string) {
	t.Helper()
	p := manifestPublicKeyPath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(contents+"\n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
}

func newBodyServer(t *testing.T, body []byte, sigRequests *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			*sigRequests++
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}
