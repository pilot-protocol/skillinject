// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sign-skills signs the pilot-skills content that pilot-daemon's
// skill injector fetches, and checks those signatures.
//
//	sign-skills sign   [-repo DIR] [-key FILE | -key-env NAME] [-any-key]
//	sign-skills verify [-repo DIR] [-pubkey KEY]
//	sign-skills genkey -out FILE
//	sign-skills pubkey [-key FILE | -key-env NAME]
//
// sign reads DIR/inject-manifest.json, and writes a .sig file next to it
// and next to every file it lists (skillinject.SignedFiles): the
// entrypoint SKILL.md, heartbeat templates, helper scripts and plugin
// files. The private key is read from -key (a file, or "-" for stdin, the
// default) or from the environment variable -key-env names, as hex or
// base64 of the 32-byte seed or the 64-byte private key. It is never
// printed or written anywhere. Unless -any-key is given, sign refuses a
// key whose public half is not skillinject.DefaultManifestPublicKeyHex,
// since daemons would refuse what it signs.
//
// On macOS, with the key in the login keychain:
//
//	security find-generic-password -s pilot-skills-signing -a "$USER" -w |
//	    sign-skills sign -repo ~/work/pilot-skills
//
// verify checks the same set of files against -pubkey (hex or base64),
// the built-in key by default, and exits 1 if any is missing or fails.
//
// genkey writes a new private key seed (hex) to a file it creates with
// mode 0600 (it refuses an existing file) and prints the public key in
// hex. Move the seed into a secret store and delete the file.
//
// pubkey prints the public key (hex) of a private key, so a stored key can
// be compared with the built-in one without exposing it.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilot-protocol/skillinject"
)

// maxFile mirrors the injector's fetch cap: a larger file would arrive
// cut short and fail its signature on every host.
const maxFile = 1 << 20

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "sign-skills:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: sign-skills sign|verify|genkey|pubkey [flags]")
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("sign-skills "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "pilot-skills checkout")
	keyFile := fs.String("key", "-", `private key file, "-" for stdin`)
	keyEnv := fs.String("key-env", "", "read the private key from this environment variable instead of -key")
	anyKey := fs.Bool("any-key", false, "sign with a key other than the built-in one (for forks and tests)")
	pubFlag := fs.String("pubkey", skillinject.DefaultManifestPublicKeyHex, "public key to verify with (hex or base64)")
	out := fs.String("out", "", "genkey: file to create for the new private key seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	switch cmd {
	case "sign":
		priv, err := readPrivateKey(*keyFile, *keyEnv, stdin)
		if err != nil {
			return err
		}
		defer wipe(priv)
		pub := priv.Public().(ed25519.PublicKey)
		if !*anyKey {
			want, err := skillinject.ParsePublicKey(skillinject.DefaultManifestPublicKeyHex)
			if err != nil {
				return fmt.Errorf("built-in public key: %w", err)
			}
			if !pub.Equal(want) {
				return fmt.Errorf("this key's public half %s is not the built-in key %s; daemons would refuse what it signs (pass -any-key to sign anyway)",
					hex.EncodeToString(pub), skillinject.DefaultManifestPublicKeyHex)
			}
		}
		return sign(*repo, priv, stdout)
	case "verify":
		pub, err := skillinject.ParsePublicKey(*pubFlag)
		if err != nil {
			return fmt.Errorf("-pubkey: %w", err)
		}
		return verify(*repo, pub, stdout)
	case "genkey":
		return genkey(*out, stdout)
	case "pubkey":
		priv, err := readPrivateKey(*keyFile, *keyEnv, stdin)
		if err != nil {
			return err
		}
		defer wipe(priv)
		fmt.Fprintln(stdout, hex.EncodeToString(priv.Public().(ed25519.PublicKey)))
		return nil
	default:
		return fmt.Errorf("unknown command %q (want sign, verify, genkey or pubkey)", cmd)
	}
}

// signedSet returns the repo paths to sign or verify: the manifest
// first, then every file it lists.
func signedSet(repo string) ([]string, error) {
	raw, err := readRepoFile(repo, skillinject.ManifestName)
	if err != nil {
		return nil, err
	}
	files, err := skillinject.SignedFiles(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", skillinject.ManifestName, err)
	}
	return append([]string{skillinject.ManifestName}, files...), nil
}

func sign(repo string, priv ed25519.PrivateKey, stdout io.Writer) error {
	names, err := signedSet(repo)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	// Read and sign everything first, so a missing file leaves no .sig
	// written.
	sigs := make([][]byte, len(names))
	for i, name := range names {
		body, err := readRepoFile(repo, name)
		if err != nil {
			return err
		}
		sigs[i] = skillinject.SignFile(priv, name, body)
		if err := skillinject.VerifyFile(pub, name, body, sigs[i]); err != nil {
			return fmt.Errorf("self-check %s: %w", name, err)
		}
	}
	for i, name := range names {
		p := filepath.Join(repo, filepath.FromSlash(name)) + skillinject.SignatureSuffix
		if err := os.WriteFile(p, sigs[i], 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "signed %s\n", name)
	}
	fmt.Fprintf(stdout, "%d files signed with %s\n", len(names), hex.EncodeToString(pub))
	return nil
}

func verify(repo string, pub ed25519.PublicKey, stdout io.Writer) error {
	names, err := signedSet(repo)
	if err != nil {
		return err
	}
	var failed int
	for _, name := range names {
		err := verifyOne(repo, pub, name)
		if err != nil {
			failed++
			fmt.Fprintf(stdout, "FAIL %s: %v\n", name, err)
			continue
		}
		fmt.Fprintf(stdout, "ok   %s\n", name)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d files failed verification against %s", failed, len(names), hex.EncodeToString(pub))
	}
	fmt.Fprintf(stdout, "%d files verified against %s\n", len(names), hex.EncodeToString(pub))
	return nil
}

func verifyOne(repo string, pub ed25519.PublicKey, name string) error {
	body, err := readRepoFile(repo, name)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(name)) + skillinject.SignatureSuffix)
	if err != nil {
		return fmt.Errorf("%w: %v", skillinject.ErrSignature, err)
	}
	return skillinject.VerifyFile(pub, name, body, sig)
}

// readRepoFile reads repo path name: a regular file inside repo, at most
// maxFile bytes.
func readRepoFile(repo, name string) ([]byte, error) {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	if st.Size() > maxFile {
		return nil, fmt.Errorf("%s is %d bytes; the injector fetches at most %d", name, st.Size(), maxFile)
	}
	return io.ReadAll(io.LimitReader(f, maxFile+1))
}

func genkey(out string, stdout io.Writer) error {
	if out == "" {
		return errors.New("genkey: -out FILE is required (the key is never printed)")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	defer wipe(priv)
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	seed := []byte(hex.EncodeToString(priv.Seed()) + "\n")
	defer wipe(seed)
	if _, err := f.Write(seed); err != nil {
		f.Close()
		os.Remove(out)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(out)
		return err
	}
	fmt.Fprintln(stdout, hex.EncodeToString(pub))
	return nil
}

func readPrivateKey(file, env string, stdin io.Reader) (ed25519.PrivateKey, error) {
	var raw []byte
	switch {
	case env != "":
		v, ok := os.LookupEnv(env)
		if !ok || v == "" {
			return nil, fmt.Errorf("%s is not set", env)
		}
		raw = []byte(v)
	case file == "-":
		b, err := io.ReadAll(io.LimitReader(stdin, 4096))
		if err != nil {
			return nil, err
		}
		raw = b
	default:
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	defer wipe(raw)
	return parsePrivateKey(bytes.TrimSpace(raw))
}

// parsePrivateKey accepts hex or base64 of a 32-byte seed or a 64-byte
// private key.
func parsePrivateKey(s []byte) (ed25519.PrivateKey, error) {
	decoders := []func([]byte) ([]byte, error){
		func(b []byte) ([]byte, error) {
			out := make([]byte, hex.DecodedLen(len(b)))
			n, err := hex.Decode(out, b)
			return out[:n], err
		},
		func(b []byte) ([]byte, error) {
			out := make([]byte, base64.StdEncoding.DecodedLen(len(b)))
			n, err := base64.StdEncoding.Decode(out, b)
			return out[:n], err
		},
	}
	for _, dec := range decoders {
		b, err := dec(s)
		if err != nil {
			wipe(b)
			continue
		}
		switch len(b) {
		case ed25519.SeedSize:
			k := ed25519.NewKeyFromSeed(b)
			wipe(b)
			return k, nil
		case ed25519.PrivateKeySize:
			k := ed25519.NewKeyFromSeed(b[:ed25519.SeedSize])
			ok := bytes.Equal(k[ed25519.SeedSize:], b[ed25519.SeedSize:])
			wipe(b)
			if !ok {
				wipe(k)
				return nil, errors.New("private key's public half does not match its seed")
			}
			return k, nil
		}
		wipe(b)
	}
	return nil, errors.New("private key is not hex or base64 of a 32-byte seed or 64-byte key")
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
