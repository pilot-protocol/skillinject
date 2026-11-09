// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"crypto/ed25519"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
)

// Test servers sign what they serve with testSigningKey, and test configs
// trust testPublicKey through Config.ManifestPublicKey. The built-in key's
// private half is not in the repo, so no test can produce content it
// accepts; the tests that rely on the built-in key check that it refuses.
var (
	testSigningKey = ed25519.NewKeyFromSeed(func() []byte {
		s := sha256.Sum256([]byte("skillinject test signing key"))
		return s[:]
	}())
	testPublicKey = testSigningKey.Public().(ed25519.PublicKey)
)

// signedHandler serves h and, for every <path>.sig, the signature of what
// h serves at <path>, under the repo path <path> without its leading "/".
// A body h does not serve with 200 has no signature (404).
func signedHandler(h http.Handler) http.Handler {
	return signedHandlerWith(testSigningKey, h)
}

// signedHandlerWith is signedHandler with another key.
func signedHandlerWith(priv ed25519.PrivateKey, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, SignatureSuffix) {
			h.ServeHTTP(w, r)
			return
		}
		bodyPath := strings.TrimSuffix(r.URL.Path, SignatureSuffix)
		inner := r.Clone(r.Context())
		inner.URL.Path, inner.URL.RawPath = bodyPath, ""
		inner.RequestURI = ""
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, inner)
		if rec.Code != http.StatusOK {
			http.NotFound(w, r)
			return
		}
		w.Write(SignFile(priv, strings.TrimPrefix(bodyPath, "/"), rec.Body.Bytes()))
	})
}

// For the external test package (package skillinject_test).
var (
	SignedHandlerForTest     = signedHandler
	SignedHandlerWithForTest = signedHandlerWith
	TestSigningKeyForTest    = testSigningKey
	TestPublicKeyForTest     = testPublicKey
)
