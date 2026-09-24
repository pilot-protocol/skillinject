// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pilot-protocol/common/netproxy"
)

// rotatingProxy is a Muse-like egress proxy: CONNECT to :443 only, Basic
// auth, and a password that rotate() replaces. Every CONNECT, whatever
// host it names, is tunnelled to target, so https://example.com/ reaches
// the test's TLS server (whose certificate covers example.com) while the
// client still has to go through the proxy (it never proxies loopback).
type rotatingProxy struct {
	t      *testing.T
	ln     net.Listener
	target string
	gen    atomic.Int64
	// credFile holds the current proxy URL, as a fresh Muse shell sees it.
	credFile string

	// garble: reject credentials with a status line net/http cannot parse
	// ("HTTP/1.1 4O7 ..."), as Meta Muse's proxy does, instead of a 407.
	garble atomic.Bool

	mu  sync.Mutex
	log []string // "ALLOW gen=N" / "DENY 407 gen=N"
}

func newRotatingProxy(t *testing.T, target string) *rotatingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &rotatingProxy{t: t, ln: ln, target: target, credFile: filepath.Join(t.TempDir(), "https_proxy")}
	p.gen.Store(1)
	p.writeCreds()
	go p.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *rotatingProxy) password(gen int64) string { return fmt.Sprintf("gen%d-s3cret", gen) }

// url is the proxy URL with generation gen's credentials.
func (p *rotatingProxy) url(gen int64) string {
	return "http://muse:" + p.password(gen) + "@" + p.ln.Addr().String()
}

func (p *rotatingProxy) writeCreds() {
	if err := os.WriteFile(p.credFile, []byte(p.url(p.gen.Load())), 0o600); err != nil {
		p.t.Error(err)
	}
}

// rotate replaces the credentials; a fresh shell (credFile) sees the new
// ones, the process environment keeps the old.
func (p *rotatingProxy) rotate() {
	p.gen.Add(1)
	p.writeCreds()
}

// refreshCommand prints the current proxy URL, as
// bash -c 'printf %s "$https_proxy"' does in Muse.
func (p *rotatingProxy) refreshCommand() string { return "cat '" + p.credFile + "'" }

func (p *rotatingProxy) record(s string) {
	p.mu.Lock()
	p.log = append(p.log, s)
	p.mu.Unlock()
}

func (p *rotatingProxy) events() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.log...)
}

func (p *rotatingProxy) count(prefix string) int {
	n := 0
	for _, e := range p.events() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func (p *rotatingProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *rotatingProxy) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	gen := p.gen.Load()
	if req.Method != http.MethodConnect || !strings.HasSuffix(req.Host, ":443") {
		p.record("DENY 403 " + req.Method + " " + req.Host)
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\n\r\n")
		return
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("muse:"+p.password(gen)))
	if req.Header.Get("Proxy-Authorization") != want {
		p.record(fmt.Sprintf("DENY 407 gen=%d", gen))
		if p.garble.Load() {
			_, _ = io.WriteString(c, "HTTP/1.1 4O7 Proxy Authentication Required\r\n\r\n")
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"muse\"\r\n\r\n")
		return
	}
	up, err := net.Dial("tcp", p.target)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close()
	p.record(fmt.Sprintf("ALLOW gen=%d", gen))
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	go func() { _, _ = io.Copy(up, br); _ = up.Close() }()
	_, _ = io.Copy(c, up)
}

// proxyRepo is a TLS pilot-skills stand-in: a manifest with the claude-code
// tool and the gated muse row, and the entrypoint SKILL.md. onManifest runs
// before the manifest is served.
type proxyRepo struct {
	srv        *httptest.Server
	onManifest func(w http.ResponseWriter)
}

func newProxyRepo(t *testing.T) *proxyRepo {
	t.Helper()
	r := &proxyRepo{}
	manifest, err := json.Marshal(Manifest{
		Version:    1,
		Entrypoint: "pilotctl",
		Tools:      []ManifestTool{{Name: "claude-code", RootDir: "~/.claude", SkillsDir: "~/.claude/skills"}},
		GatedTools: []ManifestGatedTool{{
			Name: "muse", RootDir: "~/workspace/skills", SkillsDir: "~/workspace/skills",
			RequireMarker: "~/.pilot/targets/muse", SkillFormat: SkillFormatMuse,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch strings.TrimPrefix(req.URL.Path, "/") {
		case "inject-manifest.json":
			if r.onManifest != nil {
				r.onManifest(w)
			}
			_, _ = w.Write(manifest)
		case "skills/pilotctl/SKILL.md":
			_, _ = w.Write([]byte("---\nname: pilotctl\ndescription: Pilot.\n---\nbody\n"))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// trustRepo makes the refreshing client trust the repo's certificate.
func trustRepo(t *testing.T, r *proxyRepo) {
	t.Helper()
	prev := proxyBaseTransport
	pool := x509.NewCertPool()
	pool.AddCert(r.srv.Certificate())
	proxyBaseTransport = func() *http.Transport {
		tr := prev()
		if tr == nil {
			tr = &http.Transport{}
		}
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = new(tls.Config)
		}
		tr.TLSClientConfig.RootCAs = pool
		return tr
	}
	t.Cleanup(func() { proxyBaseTransport = prev })
}

// museProxyHome is a Muse VM home after the installer ran: ~/workspace/skills,
// the marker for it, and ~/.claude.
func museProxyHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{"workspace/skills", ".pilot/targets", ".claude"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	decl := "skills_dir=" + filepath.Join(home, "workspace", "skills") + "\nskill_format=muse\n"
	if err := os.WriteFile(filepath.Join(home, ".pilot", "targets", "muse"), []byte(decl), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// launchEnv is the proxy environment the daemon was started with: the
// credentials that were current then, and nothing that exempts the repo.
func launchEnv(t *testing.T, proxyURL string) {
	t.Helper()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", EnvProxy, EnvProxyCommand} {
		t.Setenv(k, "")
	}
	t.Setenv("HTTPS_PROXY", proxyURL)
}

func repoConfig(home string) Config {
	return Config{
		Home:        home,
		ManifestURL: "https://example.com/inject-manifest.json",
		RepoBaseURL: "https://example.com/",
	}
}

// noSecrets fails when err's text holds any generation's password.
func noSecrets(t *testing.T, p *rotatingProxy, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for g := int64(1); g <= p.gen.Load()+1; g++ {
		if strings.Contains(err.Error(), p.password(g)) {
			t.Errorf("error leaks proxy credentials: %v", err)
		}
	}
}

// The review repro: the daemon's environment keeps the launch-time
// credentials, the proxy rotates them between ticks, and every tick after
// the first used to fail with 407. With the refresh command every tick
// goes out with the current credentials and keeps the Muse copy current.
func TestProxyRefresh_TicksSurviveRotation(t *testing.T) {
	repo := newProxyRepo(t)
	p := newRotatingProxy(t, repo.srv.Listener.Addr().String())
	trustRepo(t, repo)
	launchEnv(t, p.url(1))
	home := museProxyHome(t)
	cfg := repoConfig(home)
	cfg.ProxyCommand = p.refreshCommand()
	skill := filepath.Join(home, "workspace", "skills", "pilotctl", "SKILL.md")

	for tick := 1; tick <= 4; tick++ {
		if tick > 1 {
			p.rotate()
			// A local edit the next tick has to repair.
			if err := os.WriteFile(skill, []byte("edited"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		rep, err := Tick(context.Background(), cfg)
		noSecrets(t, p, err)
		if err != nil {
			t.Fatalf("tick %d (credentials gen %d): %v\nproxy: %v", tick, p.gen.Load(), err, p.events())
		}
		var muse *Outcome
		for i := range rep.Outcomes {
			if rep.Outcomes[i].Tool == "muse" {
				muse = &rep.Outcomes[i]
			}
		}
		if muse == nil || muse.Action == ActionError || muse.Action == ActionNoop {
			t.Fatalf("tick %d muse outcome = %+v, want create/rewrite", tick, muse)
		}
		if b, _ := os.ReadFile(skill); !strings.HasPrefix(string(b), "---\nname: \"pilotctl\"") {
			t.Fatalf("tick %d left %q", tick, b)
		}
	}
	if n := p.count("ALLOW gen=4"); n == 0 {
		t.Errorf("no CONNECT with the gen 4 credentials: %v", p.events())
	}
}

// Rotation in the middle of a tick: the manifest response closes its
// connection and the credentials rotate while it is served, so the
// SKILL.md fetch needs a new CONNECT, meets 407, and RefreshingTransport
// re-runs the command and retries it once.
func TestProxyRefresh_RotationMidTickIsRetried(t *testing.T) {
	repo := newProxyRepo(t)
	p := newRotatingProxy(t, repo.srv.Listener.Addr().String())
	var once sync.Once
	repo.onManifest = func(w http.ResponseWriter) {
		w.Header().Set("Connection", "close")
		once.Do(p.rotate)
	}
	trustRepo(t, repo)
	launchEnv(t, p.url(1))
	home := museProxyHome(t)
	cfg := repoConfig(home)
	cfg.ProxyCommand = p.refreshCommand()

	if _, err := Tick(context.Background(), cfg); err != nil {
		t.Fatalf("Tick: %v\nproxy: %v", err, p.events())
	}
	ev := strings.Join(p.events(), ", ")
	if !strings.Contains(ev, "ALLOW gen=1, DENY 407 gen=2, ALLOW gen=2") {
		t.Errorf("proxy events = %s, want a gen 1 tunnel, a 407, then a retry with gen 2", ev)
	}
}

// The command comes from $PILOT_PROXY_CMD or ~/.pilot/config.json
// "proxy_cmd" when Config.ProxyCommand is empty: the daemon registers
// skillinject with a zero Config.
func TestProxyRefresh_CommandFromDaemonSettings(t *testing.T) {
	for _, src := range []string{"env", "config.json"} {
		t.Run(src, func(t *testing.T) {
			repo := newProxyRepo(t)
			p := newRotatingProxy(t, repo.srv.Listener.Addr().String())
			trustRepo(t, repo)
			launchEnv(t, p.url(1))
			home := museProxyHome(t)
			if src == "env" {
				t.Setenv(EnvProxyCommand, p.refreshCommand())
			} else {
				conf, _ := json.Marshal(map[string]any{"transport": "compat", "proxy_cmd": p.refreshCommand()})
				if err := os.WriteFile(filepath.Join(home, ".pilot", "config.json"), conf, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p.rotate()
			p.rotate()
			if _, err := Tick(context.Background(), repoConfig(home)); err != nil {
				t.Fatalf("Tick after two rotations: %v\nproxy: %v", err, p.events())
			}
			if n := p.count("DENY"); n != 0 {
				t.Errorf("stale credentials were sent: %v", p.events())
			}
		})
	}
}

// A refresh command that fails keeps the last good settings (here the
// environment's), and nothing it prints ends up in an error.
func TestProxyRefresh_FailingCommandFallsBackToEnvironment(t *testing.T) {
	repo := newProxyRepo(t)
	p := newRotatingProxy(t, repo.srv.Listener.Addr().String())
	trustRepo(t, repo)
	launchEnv(t, p.url(1))
	home := museProxyHome(t)

	cfg := repoConfig(home)
	cfg.ProxyCommand = "exit 3"
	if _, err := Tick(context.Background(), cfg); err != nil {
		t.Fatalf("Tick with a failing command and current env credentials: %v", err)
	}

	// Stale environment and a command printing garbage that holds a
	// secret: the tick fails, and neither secret is in the error.
	p.rotate()
	cfg.ProxyCommand = "printf %s 'not-a-url " + p.password(2) + "'"
	_, err := Tick(context.Background(), cfg)
	if err == nil {
		t.Fatal("Tick succeeded with stale credentials and an unusable command")
	}
	noSecrets(t, p, err)
	if strings.Contains(err.Error(), "not-a-url") {
		t.Errorf("error quotes the command's output: %v", err)
	}
}

// With no command, a 407 the host's own transport reports as a
// *netproxy.ConnectError is retried once: that is how pilot-daemon's
// http.DefaultTransport answers a 407 after refreshing its credentials.
func TestFetch_RetriesProxyAuthRejectionOnce(t *testing.T) {
	for _, refreshes := range []bool{true, false} {
		t.Run(fmt.Sprintf("host refreshes=%v", refreshes), func(t *testing.T) {
			repo := newProxyRepo(t)
			p := newRotatingProxy(t, repo.srv.Listener.Addr().String())
			launchEnv(t, "")
			home := museProxyHome(t)
			var cur atomic.Pointer[url.URL]
			u, _ := url.Parse(p.url(1))
			cur.Store(u)
			p.rotate()

			pool := x509.NewCertPool()
			pool.AddCert(repo.srv.Certificate())
			tr := &http.Transport{
				Proxy:           func(*http.Request) (*url.URL, error) { return cur.Load(), nil },
				TLSClientConfig: &tls.Config{RootCAs: pool},
				OnProxyConnectResponse: func(_ context.Context, _ *url.URL, req *http.Request, res *http.Response) error {
					if res.StatusCode == http.StatusOK {
						return nil
					}
					if refreshes {
						fresh, _ := url.Parse(p.url(p.gen.Load()))
						cur.Store(fresh)
					}
					return &netproxy.ConnectError{Target: req.Host, StatusCode: res.StatusCode}
				},
			}
			cfg := repoConfig(home)
			cfg.HTTPClient = &http.Client{Transport: tr, Timeout: 10 * time.Second}

			_, err := Tick(context.Background(), cfg)
			if refreshes {
				if err != nil {
					t.Fatalf("Tick: %v\nproxy: %v", err, p.events())
				}
				if got := p.count("DENY 407"); got != 1 {
					t.Errorf("407s = %d, want 1: %v", got, p.events())
				}
				return
			}
			if err == nil || !proxyAuthRejected(err) {
				t.Fatalf("Tick = %v, want the 407", err)
			}
			if got := p.count("DENY 407"); got != 2 {
				t.Errorf("407s = %d, want 2 (one retry, no loop): %v", got, p.events())
			}
		})
	}
}

// si42-proxycmd-flag-only-unparseable-rejection-no-retry: with a transport
// setup does not own (pilot-daemon's http.DefaultTransport, when the daemon
// got -proxy-cmd only as a flag), a rejection the proxy garbles ("HTTP/1.1
// 4O7") never reaches the transport's CONNECT hook, so nothing refreshes on
// it. get waits a moment and retries once: when the transport's resolver
// re-read the credentials in the background meanwhile (as netproxy's does
// once its interval has passed, from the lookup that failed), the retry
// succeeds; when nothing refreshed, it fails after exactly one retry.
func TestFetch_RetriesUnreadableProxyReplyOnce(t *testing.T) {
	old := unreadableReplyRetryDelay
	unreadableReplyRetryDelay = 100 * time.Millisecond
	t.Cleanup(func() { unreadableReplyRetryDelay = old })
	for _, background := range []bool{true, false} {
		t.Run(fmt.Sprintf("background_refresh=%v", background), func(t *testing.T) {
			repo := newProxyRepo(t)
			p := newRotatingProxy(t, repo.srv.Listener.Addr().String())
			p.garble.Store(true)
			launchEnv(t, "")
			home := museProxyHome(t)
			var cur atomic.Pointer[url.URL]
			u, _ := url.Parse(p.url(1))
			cur.Store(u)
			p.rotate()

			var refreshing atomic.Bool
			pool := x509.NewCertPool()
			pool.AddCert(repo.srv.Certificate())
			tr := &http.Transport{
				Proxy: func(*http.Request) (*url.URL, error) {
					// A resolver whose refresh interval has passed starts
					// a background re-read on a lookup and answers with
					// the settings in hand.
					if background && refreshing.CompareAndSwap(false, true) {
						go func() {
							time.Sleep(20 * time.Millisecond)
							fresh, _ := url.Parse(p.url(p.gen.Load()))
							cur.Store(fresh)
						}()
					}
					return cur.Load(), nil
				},
				TLSClientConfig: &tls.Config{RootCAs: pool},
				// The hook of pilot-daemon's transport; a garbled answer
				// never reaches it.
				OnProxyConnectResponse: func(_ context.Context, _ *url.URL, req *http.Request, res *http.Response) error {
					if res.StatusCode == http.StatusOK {
						return nil
					}
					return &netproxy.ConnectError{Target: req.Host, StatusCode: res.StatusCode}
				},
			}
			cfg := repoConfig(home)
			cfg.HTTPClient = &http.Client{Transport: tr, Timeout: 10 * time.Second}

			_, err := Tick(context.Background(), cfg)
			if background {
				if err != nil {
					t.Fatalf("Tick: %v\nproxy: %v", err, p.events())
				}
				if got := p.count("DENY 407"); got != 1 {
					t.Errorf("rejections = %d, want 1: %v", got, p.events())
				}
				return
			}
			if err == nil || !unreadableProxyReply(err) {
				t.Fatalf("Tick = %v, want the garbled rejection", err)
			}
			if got := p.count("DENY 407"); got != 2 {
				t.Errorf("rejections = %d, want 2 (one retry, no loop): %v", got, p.events())
			}
		})
	}
}

func TestProxyCommand_Precedence(t *testing.T) {
	type env map[string]string
	cases := []struct {
		name   string
		env    env
		config map[string]any
		cfg    string
		want   string
	}{
		{name: "nothing set", want: ""},
		{name: "config proxy_cmd", config: map[string]any{"proxy_cmd": "cfg-cmd"}, want: "cfg-cmd"},
		{name: "env beats config", env: env{EnvProxyCommand: "env-cmd"}, config: map[string]any{"proxy_cmd": "cfg-cmd"}, want: "env-cmd"},
		{name: "Config beats env", env: env{EnvProxyCommand: "env-cmd"}, cfg: "field-cmd", want: "field-cmd"},
		{name: "blank env falls through", env: env{EnvProxyCommand: "  "}, config: map[string]any{"proxy_cmd": "cfg-cmd"}, want: "cfg-cmd"},
		{name: "PILOT_PROXY=off", env: env{EnvProxy: "off", EnvProxyCommand: "env-cmd"}, want: ""},
		{name: "PILOT_PROXY=Direct", env: env{EnvProxy: "Direct"}, config: map[string]any{"proxy_cmd": "cfg-cmd"}, want: ""},
		{name: "config proxy none", config: map[string]any{"proxy": "none", "proxy_cmd": "cfg-cmd"}, want: ""},
		{name: "PILOT_PROXY URL beats config off", env: env{EnvProxy: "http://relay:3128"}, config: map[string]any{"proxy": "off", "proxy_cmd": "cfg-cmd"}, want: "cfg-cmd"},
		{name: "Config beats off", env: env{EnvProxy: "off"}, cfg: "field-cmd", want: "field-cmd"},
		{name: "non-string proxy_cmd", config: map[string]any{"proxy_cmd": []string{"x"}}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvProxy, "")
			t.Setenv(EnvProxyCommand, "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			home := t.TempDir()
			if tc.config != nil {
				b, _ := json.Marshal(tc.config)
				if err := os.MkdirAll(filepath.Join(home, ".pilot"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, ".pilot", "config.json"), b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := proxyCommand(Config{Home: home, ProxyCommand: tc.cfg}); got != tc.want {
				t.Errorf("proxyCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

// Without a command the default client is the one it always was: no
// transport of its own, so it follows http.DefaultTransport, and a
// Config.HTTPClient is used as given.
func TestDefaultClient_WithoutCommandIsPlain(t *testing.T) {
	t.Setenv(EnvProxy, "")
	t.Setenv(EnvProxyCommand, "")
	f := newFetcher(Config{Home: t.TempDir()})
	if f.ownsTransport || f.httpClient.Transport != nil || f.httpClient.Timeout != fetchTimeout {
		t.Errorf("default client = %+v (owns %v), want a plain client on DefaultTransport", f.httpClient, f.ownsTransport)
	}
	given := &http.Client{}
	t.Setenv(EnvProxyCommand, "true")
	if f := newFetcher(Config{HTTPClient: given}); f.httpClient != given || f.ownsTransport {
		t.Error("Config.HTTPClient was not used as given")
	}
	// An HTTPS_PROXY netproxy cannot use: the plain client, which reports
	// the bad setting on every fetch as it always did.
	t.Setenv("HTTPS_PROXY", "socks5://127.0.0.1:1080")
	if f := newFetcher(Config{Home: t.TempDir()}); f.ownsTransport || f.httpClient.Transport != nil {
		t.Errorf("unusable HTTPS_PROXY: client = %+v (owns %v), want the plain one", f.httpClient, f.ownsTransport)
	}
}
