// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Signed content.
//
// Every file the injector fetches from the pilot-skills repo (the
// manifest, the entrypoint SKILL.md, heartbeat templates, helper scripts,
// plugin files) has a detached Ed25519 signature next to it: the file's
// URL plus SignatureSuffix. A tick verifies all of them before it writes
// anything, so content that is unsigned, signed by another key, or
// altered after signing never reaches disk (see tick in skillinject.go).
//
// A .sig file holds the standard base64 encoding of the 64-byte
// signature, optionally followed by a newline. The signed message is
//
//	"pilot-skills-sig-v1" 0x00 <repo path> 0x00 <file bytes>
//
// where <repo path> is the file's path relative to the repo root as the
// manifest names it ("skills/pilotctl/SKILL.md", "inject-manifest.json").
// Binding the path means a signed file cannot be served in place of
// another one, e.g. a signed heartbeat template as a helper script.
//
// cmd/sign-skills writes the .sig files; SignFile and VerifyFile are the
// only two places that build the message.

// SignatureSuffix is appended to a file's URL (and its repo path) to name
// its detached signature.
const SignatureSuffix = ".sig"

// ManifestName is the manifest's path in the pilot-skills repo, which is
// the name its signature covers.
const ManifestName = "inject-manifest.json"

// signatureDomain separates these signatures from anything else the same
// key might ever sign.
const signatureDomain = "pilot-skills-sig-v1"

// maxSigFile bounds a .sig body: 88 base64 characters plus whitespace.
const maxSigFile = 256

// ErrSignature marks a fetch refused because the file's signature is
// missing, malformed, or does not verify, or because no usable
// verification key is configured. A tick that hits it writes nothing.
var ErrSignature = errors.New("signature check failed")

func signatureMessage(name string, body []byte) []byte {
	msg := make([]byte, 0, len(signatureDomain)+len(name)+len(body)+2)
	msg = append(msg, signatureDomain...)
	msg = append(msg, 0)
	msg = append(msg, name...)
	msg = append(msg, 0)
	return append(msg, body...)
}

// SignFile returns the contents of the .sig file for body published at
// repo path name.
func SignFile(priv ed25519.PrivateKey, name string, body []byte) []byte {
	sig := ed25519.Sign(priv, signatureMessage(name, body))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// VerifyFile checks sigFile (the contents of name's .sig file) against
// body. Errors wrap ErrSignature.
func VerifyFile(pub ed25519.PublicKey, name string, body, sigFile []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: verification key is %d bytes, want %d", ErrSignature, len(pub), ed25519.PublicKeySize)
	}
	if len(sigFile) > maxSigFile {
		return fmt.Errorf("%w: %s%s is %d bytes, not a signature", ErrSignature, name, SignatureSuffix, len(sigFile))
	}
	sig, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sigFile)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: %s%s is not a base64 Ed25519 signature", ErrSignature, name, SignatureSuffix)
	}
	if !ed25519.Verify(pub, signatureMessage(name, body), sig) {
		return fmt.Errorf("%w: %s does not match its signature", ErrSignature, name)
	}
	return nil
}

// ParsePublicKey decodes an Ed25519 public key given as hex, standard
// base64, or unpadded base64 (the forms PILOT_SKILLINJECT_PUBKEY,
// PILOT_SKILLINJECT_MANIFEST_PUBKEY and ~/.pilot/skillinject.pub accept).
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	return decodeEd25519PublicKey(s)
}

// SignedFiles returns the repo paths, other than the manifest itself, that
// the manifest in raw can make an injector fetch, sorted and without
// duplicates: the entrypoint SKILL.md and every "src" and
// "heartbeatTemplate" value anywhere in the document. Reading those keys
// at any depth, rather than only the rows this release decodes, also
// covers keys a later release reads ("plugins" next to "plugin", say),
// so content signed today keeps verifying there.
//
// A path that is absolute or climbs out of the repo with ".." is an
// error: no file of the repo can be signed under that name.
func SignedFiles(raw []byte) ([]string, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	top, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("manifest is not a JSON object")
	}
	entry, _ := top["entrypoint"].(string)
	if entry == "" {
		return nil, fmt.Errorf("manifest missing entrypoint")
	}
	seen := map[string]bool{}
	var bad []string
	add := func(p string) {
		name := repoPath(p)
		if !validRepoPath(name) {
			bad = append(bad, p)
			return
		}
		seen[name] = true
	}
	add(entrypointPath(entry))
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if s, isStr := child.(string); isStr && (k == "src" || k == "heartbeatTemplate") {
					if s != "" {
						add(s)
					}
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	for k, v := range top {
		if k != "retired" {
			walk(v)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf("manifest names paths outside the repo: %s", strings.Join(bad, ", "))
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// entrypointPath is the repo path of the entrypoint skill.
func entrypointPath(entrypoint string) string {
	return fmt.Sprintf("skills/%s/SKILL.md", entrypoint)
}

// repoPath is the name a fetched file's signature covers: the manifest
// path with any leading "/" dropped, exactly as fetchRepoFile appends it
// to the repo base URL.
func repoPath(p string) string {
	return strings.TrimPrefix(p, "/")
}

// validRepoPath reports whether name stays inside the repo.
func validRepoPath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
		return false
	}
	c := path.Clean(name)
	return c != "." && c != ".." && !strings.HasPrefix(c, "../")
}
