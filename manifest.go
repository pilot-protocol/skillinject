// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"
)

// TODO: update to pilot-protocol/pilot-skills once that repo is transferred from TeoSlayer.

// DefaultManifestURL is the canonical raw GitHub URL for the inject
// manifest. Overridable via Config.ManifestURL (test hook).
const DefaultManifestURL = "https://raw.githubusercontent.com/TeoSlayer/pilot-skills/main/inject-manifest.json"

// TODO: update to pilot-protocol/pilot-skills once that repo is transferred from TeoSlayer.

// DefaultRepoBaseURL is the prefix used to fetch any path the manifest
// references (skills/<name>/SKILL.md, heartbeats/<tool>.md). Overridable
// via Config.RepoBaseURL.
const DefaultRepoBaseURL = "https://raw.githubusercontent.com/TeoSlayer/pilot-skills/main/"

// Manifest mirrors inject-manifest.json. Field tags match the upstream
// schema. Unknown fields are ignored (forward-compat with new tool fields).
type Manifest struct {
	Version     int              `json:"version"`
	Entrypoint  string           `json:"entrypoint"`
	Description string           `json:"description,omitempty"`
	Tools       []ManifestTool   `json:"tools"`
	Helpers     []ManifestHelper `json:"helpers,omitempty"`
	// Retired lists surfaces earlier manifests installed that this one no
	// longer manages; every tick and Uninstall remove them. Merged with the
	// built-in list in retired.go. Optional.
	Retired *ManifestRetired `json:"retired,omitempty"`
	// GatedTools are skill targets that are active only on hosts that
	// opted in by creating a marker file under ~/.pilot (see gated.go).
	// They have their own key, not rows in Tools, because releases that
	// predate this field drop the key when they decode the manifest. A
	// row in Tools would be installed by those releases on every host
	// where its rootDir exists, with no marker check. Optional.
	GatedTools []ManifestGatedTool `json:"gatedTools,omitempty"`
}

// ManifestGatedTool is one "gatedTools" row: a skill target whose
// directory is too generic to detect by existence alone (Meta Muse loads
// skills from ~/workspace/skills). Only the entrypoint skill copy is
// installed; there is no heartbeat or plugin.
type ManifestGatedTool struct {
	Name string `json:"name"`
	// RootDir must exist, and must be inside the home directory. Every
	// file operation for this tool stays inside it (see gated.go).
	RootDir string `json:"rootDir"`
	// SkillsDir must be RootDir or inside it.
	SkillsDir string `json:"skillsDir"`
	// SkillNaming is "" (directory layout, the default) or "flat", as
	// for ManifestTool.
	SkillNaming string `json:"skillNaming,omitempty"`
	// RequireMarker is the file that turns the tool on, e.g.
	// "~/.pilot/targets/muse". It must be inside ~/.pilot, and it must
	// name SkillsDir and SkillFormat ("skills_dir=..." and
	// "skill_format=..." lines, see gated.go). While it is absent, or
	// names another directory or format, nothing under RootDir is read,
	// written or removed.
	RequireMarker string `json:"requireMarker"`
	// SkillFormat names a rewrite applied to the entrypoint SKILL.md
	// before it is written: "" copies it unchanged, SkillFormatMuse
	// rewrites the frontmatter (see skillformat.go). An unknown value is
	// an error row and nothing is written.
	SkillFormat string `json:"skillFormat,omitempty"`
}

// ManifestHelper is one helper script the daemon installs at a
// well-known path so any AI tool on the host can invoke it. Used to
// ship pilot-ask (the directory + specialist round-trip wrapper).
//
// Helpers are tool-agnostic — they live under ~/.pilot/bin/ and are
// referenced by every tool's heartbeat directive.
type ManifestHelper struct {
	Name string `json:"name"`
	// Src is a repo-relative path fetched via fetchRepoFile, e.g.
	// "workflow-injection/pilot-ask".
	Src string `json:"src"`
	// Dst is the absolute install target. Supports ~/ expansion, e.g.
	// "~/.pilot/bin/pilot-ask".
	Dst string `json:"dst"`
	// Mode is the file mode in octal string form, e.g. "0755". Empty
	// defaults to 0755 (helpers are executables).
	Mode string `json:"mode,omitempty"`
}

// ManifestTool is one tool target row.
type ManifestTool struct {
	Name              string          `json:"name"`
	RootDir           string          `json:"rootDir"`
	SkillsDir         string          `json:"skillsDir"`
	HeartbeatPath     string          `json:"heartbeatPath,omitempty"`
	HeartbeatTemplate string          `json:"heartbeatTemplate,omitempty"`
	SkillNaming       string          `json:"skillNaming,omitempty"` // "" = "directory" (default), "flat" = single-file
	SelfHeartbeat     bool            `json:"selfHeartbeat,omitempty"`
	Plugin            *ManifestPlugin `json:"plugin,omitempty"`
}

// ManifestPlugin describes a per-tool plugin that the daemon writes
// onto disk (alongside the heartbeat + skill copy) and tracks in the
// tool's own plugin allow-list. Today this is openclaw-only — the
// plugin registers a `before_prompt_build` hook that prepends the
// pilot directive into the system prompt on every turn. SKILL.md and
// the heartbeat file are loaded by their tools' own lifecycles
// (workspace bootstrap / periodic), neither fires per-turn, so the
// plugin is the only reliable per-prompt injection surface.
type ManifestPlugin struct {
	// ID matches openclaw.plugin.json's "id" field. Used as the
	// allow-list key + directory name.
	ID string `json:"id"`
	// InstallPath is where the plugin directory is written
	// (e.g. "~/.openclaw/extensions/pilotprotocol-prompt-injector").
	InstallPath string `json:"installPath"`
	// Files lists the plugin source files the daemon copies in.
	// Order doesn't matter — each file is reconciled independently.
	Files []ManifestPluginFile `json:"files"`
	// AllowList, if set, tells the daemon to ensure the plugin id
	// appears in the tool's plugin allow-list + entries map. Nil
	// disables the JSON-merge step (e.g. for tools without an
	// explicit allow-list concept).
	AllowList *ManifestPluginAllowList `json:"allowList,omitempty"`
}

// ManifestPluginFile is one file the daemon writes into the plugin
// install directory. Mirrors ManifestHelper but scoped to a plugin.
type ManifestPluginFile struct {
	// Name is the filename relative to InstallPath (e.g.
	// "openclaw.plugin.json", "index.mjs").
	Name string `json:"name"`
	// Src is a repo-relative path fetched via fetchRepoFile
	// (e.g. "workflow-injection/openclaw-plugin/index.mjs").
	Src string `json:"src"`
}

// ManifestPluginAllowList describes how the daemon merges its plugin id
// into a tool's configuration to mark the plugin as trusted/enabled.
// Today targets openclaw.json with paths `plugins.allow` (string array)
// and `plugins.entries.<id>.enabled` (bool).
type ManifestPluginAllowList struct {
	// ConfigPath is the JSON file the daemon merges into
	// (e.g. "~/.openclaw/openclaw.json").
	ConfigPath string `json:"configPath"`
	// AllowListJsonPath is a dotted path to the trust array. Created
	// if absent. Daemon appends the plugin id iff not already present.
	AllowListJsonPath string `json:"allowListJsonPath"`
	// EntriesJsonPath is a dotted path to the per-plugin entries
	// object (e.g. "plugins.entries"). The daemon ensures
	// `entries.<id>.enabled` is `true`.
	EntriesJsonPath string `json:"entriesJsonPath"`
}

// EnvManifestPublicKey names the environment variable holding an
// Ed25519 public key (hex or base64) that replaces the built-in one for
// verifying fetched resources. Use it to point a host at a fork of
// pilot-skills signed with its own key.
const EnvManifestPublicKey = "PILOT_SKILLINJECT_PUBKEY"

// EnvDaemonManifestPublicKey is the name pilot-daemon has always read
// for the same override. The library honours it too, ahead of
// EnvManifestPublicKey, so pilotctl verifies with the key the daemon
// uses when both run with the same environment.
const EnvDaemonManifestPublicKey = "PILOT_SKILLINJECT_MANIFEST_PUBKEY"

// EnvRequireSignedManifest named the environment variable that made a
// verification key mandatory.
//
// Deprecated: signatures are always required; the variable is ignored.
const EnvRequireSignedManifest = "PILOT_SKILLINJECT_REQUIRE_SIG"

// manifestPublicKeyRel is the path under ~/.pilot holding a trusted
// Ed25519 public key in hex or base64 form, one key per file.
const manifestPublicKeyRel = "skillinject.pub"

// DefaultManifestPublicKeyHex is the built-in verification key, hex
// encoded: the public half of the key that signs TeoSlayer/pilot-skills
// (cmd/sign-skills). It is used when Config.ManifestPublicKey, the
// environment and ~/.pilot/skillinject.pub supply none. A build with an
// empty value has no key and refuses every fetch.
const DefaultManifestPublicKeyHex = "95effdc0863c8e37d65abb292f338856ac703705041a23246c8e468d7426fb31"

// fetcher is a small wrapper around http.Client that returns response
// bodies. Pulled out so tests can inject a fake.
type fetcher struct {
	httpClient  *http.Client
	manifestURL string
	repoBase    string
	publicKey   ed25519.PublicKey // nil = no key resolved; every fetch fails
	keyErr      error             // non-nil when a configured key failed to decode
	// ownsTransport: httpClient is the refreshing client newFetcher built
	// (see proxy.go), which retries a 407 itself and whose idle
	// connections close drops.
	ownsTransport bool
}

func newFetcher(cfg Config) *fetcher {
	c := cfg.HTTPClient
	owns := false
	if c == nil {
		c, owns = defaultHTTPClient(cfg)
	}
	mu := cfg.ManifestURL
	if mu == "" {
		mu = DefaultManifestURL
	}
	rb := cfg.RepoBaseURL
	if rb == "" {
		rb = DefaultRepoBaseURL
	}
	if !strings.HasSuffix(rb, "/") {
		rb += "/"
	}
	key, keyErr := resolveManifestPublicKey(cfg)
	return &fetcher{
		httpClient:    c,
		manifestURL:   mu,
		repoBase:      rb,
		publicKey:     key,
		keyErr:        keyErr,
		ownsTransport: owns,
	}
}

// close releases the idle connections of a transport newFetcher built. A
// client from Config.HTTPClient, or one on the shared
// http.DefaultTransport, is left alone.
func (f *fetcher) close() {
	if f.ownsTransport {
		f.httpClient.CloseIdleConnections()
	}
}

// resolveManifestPublicKey returns the Ed25519 public key used to verify
// fetched resources, checking the Config field, then the environment
// (EnvDaemonManifestPublicKey, then EnvManifestPublicKey), then the
// on-disk trust file, then the built-in default. A nil key with a nil
// error means no source supplied one, which only a build with an empty
// DefaultManifestPublicKeyHex can reach.
func resolveManifestPublicKey(cfg Config) (ed25519.PublicKey, error) {
	if len(cfg.ManifestPublicKey) > 0 {
		if len(cfg.ManifestPublicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("configured manifest public key is %d bytes, want %d",
				len(cfg.ManifestPublicKey), ed25519.PublicKeySize)
		}
		return cfg.ManifestPublicKey, nil
	}
	for _, env := range []string{EnvDaemonManifestPublicKey, EnvManifestPublicKey} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			k, err := decodeEd25519PublicKey(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", env, err)
			}
			return k, nil
		}
	}
	if p := manifestPublicKeyPath(cfg.Home); p != "" {
		if raw, err := os.ReadFile(p); err == nil {
			if v := strings.TrimSpace(string(raw)); v != "" {
				k, derr := decodeEd25519PublicKey(v)
				if derr != nil {
					return nil, fmt.Errorf("%s: %w", p, derr)
				}
				return k, nil
			}
		}
	}
	if DefaultManifestPublicKeyHex != "" {
		k, err := decodeEd25519PublicKey(DefaultManifestPublicKeyHex)
		if err != nil {
			return nil, fmt.Errorf("built-in manifest public key: %w", err)
		}
		return k, nil
	}
	return nil, nil
}

// manifestPublicKeyPath returns the on-disk trust file location, or ""
// when the home directory cannot be determined.
func manifestPublicKeyPath(home string) string {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = h
	}
	return filepath.Join(home, ".pilot", manifestPublicKeyRel)
}

// decodeEd25519PublicKey parses a public key encoded as hex, standard
// base64, or raw (unpadded) base64.
func decodeEd25519PublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	decoders := []func(string) ([]byte, error){
		hex.DecodeString,
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
	}
	for _, dec := range decoders {
		b, err := dec(s)
		if err != nil {
			continue
		}
		if len(b) == ed25519.PublicKeySize {
			return ed25519.PublicKey(b), nil
		}
	}
	return nil, fmt.Errorf("not a %d-byte hex or base64 ed25519 public key", ed25519.PublicKeySize)
}

func (f *fetcher) get(ctx context.Context, url string) ([]byte, error) {
	resp, err := f.do(ctx, url)
	if err != nil && !f.ownsTransport && ctx.Err() == nil {
		switch {
		case proxyAuthRejected(err):
			// The proxy refused the credentials, and the transport says so
			// with a *netproxy.ConnectError: it follows a proxy resolver
			// that refreshes them on a 407 (pilot-daemon's
			// http.DefaultTransport does), so a second request goes out
			// with the new ones.
			resp, err = f.do(ctx, url)
		case unreadableProxyReply(err):
			// An answer net/http could not parse, which is how some
			// proxies (Meta Muse's) reject expired credentials. The
			// transport never saw a status, so it refreshed nothing
			// itself; its resolver re-reads the credentials in the
			// background once its interval has passed (pilot-daemon's:
			// 60s, started by the lookup that just failed). Give that a
			// moment, then retry once.
			if sleepCtx(ctx, unreadableReplyRetryDelay) {
				resp, err = f.do(ctx, url)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d for %s", resp.StatusCode, url)
	}
	// A larger file is cut at the cap and then fails its signature.
	return io.ReadAll(io.LimitReader(resp.Body, maxFetchBody))
}

// maxFetchBody caps every fetched body (cmd/sign-skills refuses to sign a
// larger file).
const maxFetchBody = 1 << 20

// do sends one GET for url.
func (f *fetcher) do(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pilot-daemon/skillinject")
	return f.httpClient.Do(req)
}

// fetchManifest grabs, verifies and parses the manifest from the
// configured URL.
func (f *fetcher) fetchManifest(ctx context.Context) (*Manifest, error) {
	body, err := f.getVerified(ctx, f.manifestURL, manifestSignedName(f.manifestURL))
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("unsupported manifest version: %d", m.Version)
	}
	if m.Entrypoint == "" {
		return nil, fmt.Errorf("manifest missing entrypoint")
	}
	if len(m.Tools) == 0 {
		return nil, fmt.Errorf("manifest has no tools")
	}
	return &m, nil
}

// manifestSignedName is the name the manifest's signature covers: the
// last element of its URL path, ManifestName for the default URL.
func manifestSignedName(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		return path.Base(u.Path)
	}
	return ManifestName
}

// fetchRepoFile retrieves and verifies a path relative to the repo root
// (e.g. "skills/pilotctl/SKILL.md", "heartbeats/openclaw.md").
func (f *fetcher) fetchRepoFile(ctx context.Context, relPath string) ([]byte, error) {
	name := repoPath(relPath)
	return f.getVerified(ctx, f.repoBase+name, name)
}

// getVerified returns the body at url once its detached signature at
// url+SignatureSuffix verifies for repo path name. An error fetching the
// body is returned as is; any failure to obtain or check the signature,
// or a missing or unusable key, wraps ErrSignature.
func (f *fetcher) getVerified(ctx context.Context, url, name string) ([]byte, error) {
	if f.keyErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrSignature, f.keyErr)
	}
	if f.publicKey == nil {
		return nil, fmt.Errorf("%w: no verification key (set %s or %s, or write one to %s)",
			ErrSignature, EnvDaemonManifestPublicKey, EnvManifestPublicKey, manifestPublicKeyPath(""))
	}
	body, err := f.get(ctx, url)
	if err != nil {
		return nil, err
	}
	sig, err := f.get(ctx, url+SignatureSuffix)
	if err != nil {
		return nil, fmt.Errorf("%w: fetch %s%s: %v", ErrSignature, name, SignatureSuffix, err)
	}
	if err := VerifyFile(f.publicKey, name, body, sig); err != nil {
		return nil, err
	}
	return body, nil
}

// pathWithin reports whether path, after lexical cleaning, resolves to
// dir itself or to a location beneath it. Both arguments are treated as
// plain lexical paths; symlinks are not resolved.
func pathWithin(dir, path string) bool {
	cleanDir := filepath.Clean(dir)
	cleanPath := filepath.Clean(path)
	if cleanPath == cleanDir {
		return true
	}
	return strings.HasPrefix(cleanPath, cleanDir+string(os.PathSeparator))
}

// expandHome resolves "~/" in a manifest path against the user's home dir.
func expandHome(p, home string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	if p == "~" {
		return home
	}
	return p
}

// renderHeartbeat fills in {{.EntrypointPath}} (and any future fields) in
// the per-tool heartbeat template.
type heartbeatVars struct {
	EntrypointPath string
}

func renderHeartbeat(tmplBody []byte, v heartbeatVars) (string, error) {
	t, err := template.New("hb").Parse(string(tmplBody))
	if err != nil {
		return "", fmt.Errorf("parse heartbeat template: %w", err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, v); err != nil {
		return "", fmt.Errorf("render heartbeat template: %w", err)
	}
	return sb.String(), nil
}

// cacheDir is where fetched skill content is mirrored on disk. Used for
// telemetry and future offline fallback (currently no fallback).
func cacheDir(home string) string {
	return filepath.Join(home, ".pilot", "skills-cache")
}

func writeCache(home, relPath string, body []byte) error {
	p := filepath.Join(cacheDir(home), filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, body, 0o644)
}
