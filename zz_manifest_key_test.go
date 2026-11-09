// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
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

	t.Setenv(EnvDaemonManifestPublicKey, "")
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

	clearKeyEnv(t)
	writeKeyFile(t, home, hex.EncodeToString(pub))

	got, err := resolveManifestPublicKey(Config{Home: home})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Equal(pub) {
		t.Fatalf("trust file key not picked up")
	}
}

func TestResolveManifestPublicKey_NoOverrideIsBuiltIn(t *testing.T) {
	clearKeyEnv(t)
	got, err := resolveManifestPublicKey(Config{Home: t.TempDir()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want, err := decodeEd25519PublicKey(DefaultManifestPublicKeyHex)
	if err != nil {
		t.Fatalf("built-in key does not decode: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("with no override the key is not the built-in one")
	}
}

// The daemon's variable name works in the library too (pilotctl), and
// wins over the library's own name, as Config (set from it by the
// daemon) does in pilot-daemon.
func TestResolveManifestPublicKey_DaemonEnvName(t *testing.T) {
	pub, _ := newTestKey(t)
	other, _ := newTestKey(t)
	home := t.TempDir()
	writeKeyFile(t, home, hex.EncodeToString(other))

	t.Setenv(EnvManifestPublicKey, hex.EncodeToString(other))
	t.Setenv(EnvDaemonManifestPublicKey, base64.StdEncoding.EncodeToString(pub))
	got, err := resolveManifestPublicKey(Config{Home: home})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Equal(pub) {
		t.Fatalf("%s did not take precedence", EnvDaemonManifestPublicKey)
	}

	t.Setenv(EnvDaemonManifestPublicKey, "not-a-key")
	if _, err := resolveManifestPublicKey(Config{Home: home}); err == nil || !strings.Contains(err.Error(), EnvDaemonManifestPublicKey) {
		t.Fatalf("a bad %s should be an error naming it, got %v", EnvDaemonManifestPublicKey, err)
	}
}

func TestResolveManifestPublicKey_BadEncodingErrors(t *testing.T) {
	clearKeyEnv(t)
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

// TestGetVerified_DefaultRefusesUnsigned: with no override, a body
// without a signature is refused (the built-in key is always used).
func TestGetVerified_DefaultRefusesUnsigned(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv(EnvRequireSignedManifest, "0") // deprecated; must not turn verification off

	var sigRequests int
	srv := newBodyServer(t, []byte("hello"), &sigRequests)

	f := newFetcher(Config{Home: t.TempDir(), HTTPClient: srv.Client()})
	_, err := f.getVerified(context.Background(), srv.URL+"/x", "x")
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("unsigned body: err = %v, want ErrSignature", err)
	}
	if sigRequests != 1 {
		t.Fatalf("expected one signature fetch, got %d", sigRequests)
	}
}

// TestGetVerified_DefaultRefusesOtherKey: content signed by any key but
// the built-in one is refused when nothing overrides the key.
func TestGetVerified_DefaultRefusesOtherKey(t *testing.T) {
	clearKeyEnv(t)
	srv := httptest.NewServer(signedHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("signed by the test key"))
	})))
	t.Cleanup(srv.Close)

	f := newFetcher(Config{Home: t.TempDir(), HTTPClient: srv.Client()})
	if _, err := f.getVerified(context.Background(), srv.URL+"/x", "x"); !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrSignature", err)
	}
}

// TestGetVerified_NoKeyRefuses: a build without a built-in key and no
// override fetches nothing.
func TestGetVerified_NoKeyRefuses(t *testing.T) {
	var sigRequests int
	srv := newBodyServer(t, []byte("hello"), &sigRequests)
	f := &fetcher{httpClient: srv.Client()}
	_, err := f.getVerified(context.Background(), srv.URL+"/x", "x")
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrSignature", err)
	}
	if !strings.Contains(err.Error(), EnvManifestPublicKey) || !strings.Contains(err.Error(), EnvDaemonManifestPublicKey) {
		t.Fatalf("error should name the overrides, got %v", err)
	}
}

// TestGetVerified_BadKeyRefuses: an override that does not decode stops
// every fetch rather than falling back to the built-in key.
func TestGetVerified_BadKeyRefuses(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv(EnvManifestPublicKey, "zzzz")
	var sigRequests int
	srv := newBodyServer(t, []byte("hello"), &sigRequests)
	f := newFetcher(Config{Home: t.TempDir(), HTTPClient: srv.Client()})
	if _, err := f.getVerified(context.Background(), srv.URL+"/x", "x"); !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrSignature", err)
	}
}

// TestGetVerified_ResolvedKeyVerifies checks that a key supplied purely
// via the trust file is picked up and used to verify.
func TestGetVerified_ResolvedKeyVerifies(t *testing.T) {
	clearKeyEnv(t)

	pub, priv := newTestKey(t)
	home := t.TempDir()
	writeKeyFile(t, home, hex.EncodeToString(pub))

	body := []byte("signed body")
	sig := SignFile(priv, "x", body)

	mux := http.NewServeMux()
	mux.HandleFunc("/x", func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
	mux.HandleFunc("/x.sig", func(w http.ResponseWriter, _ *http.Request) { w.Write(sig) })
	mux.HandleFunc("/bad", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("tampered")) })
	mux.HandleFunc("/bad.sig", func(w http.ResponseWriter, _ *http.Request) { w.Write(sig) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f := newFetcher(Config{Home: home, HTTPClient: srv.Client()})
	got, err := f.getVerified(context.Background(), srv.URL+"/x", "x")
	if err != nil {
		t.Fatalf("getVerified: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("body = %q", got)
	}
	if _, err := f.getVerified(context.Background(), srv.URL+"/bad", "bad"); !errors.Is(err, ErrSignature) {
		t.Fatalf("expected verification failure on a mismatched body, got %v", err)
	}
	// The signature covers the name: the same bytes under another name fail.
	if _, err := f.getVerified(context.Background(), srv.URL+"/x", "y"); !errors.Is(err, ErrSignature) {
		t.Fatalf("expected verification failure under another name, got %v", err)
	}
}

// clearKeyEnv removes key overrides a developer's shell may carry.
func clearKeyEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvManifestPublicKey, "")
	t.Setenv(EnvDaemonManifestPublicKey, "")
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
