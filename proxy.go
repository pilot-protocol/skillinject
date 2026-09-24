// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

// Egress proxies that rotate their credentials.
//
// Some sandboxes (Meta Muse) put the egress proxy's credentials in
// HTTPS_PROXY and replace them every few minutes. A new shell sees the
// current ones. The daemon this package runs in keeps the copy it was
// started with, and net/http reads the proxy environment only once per
// process, so after the first rotation the proxy answers every new CONNECT
// with 407 and every tick after the first one fails.
//
// pilot-daemon handles this with a refresh command, a shell command that
// prints the current proxy URL (common/netproxy WithRefreshCommand). The
// daemon takes it from its -proxy-cmd flag, $PILOT_PROXY_CMD, or "proxy_cmd"
// in ~/.pilot/config.json. On sandbox hosts the Pilot installer saves the
// sandbox command there, and pilotctl, pilot-up.sh and pilot-mcp hand it to
// the daemon as $PILOT_PROXY_CMD:
//
//	bash -c 'case $https_proxy in *@*) printf %s "$https_proxy";; *) printf %s "${HTTPS_PROXY:-$https_proxy}";; esac'
//
// When Config.HTTPClient is nil, the client newFetcher builds uses the same
// command (Config.ProxyCommand, then $PILOT_PROXY_CMD, then config.json):
// a netproxy.RefreshingTransport whose Resolver runs it when the tick starts,
// again once a minute while it runs, and when the proxy answers 407, after
// which the refused request is retried once. With no command the client is
// the plain one it always was, and a fetch the proxy refused with 407 is
// retried once if the process's transport reports the refusal as a
// *netproxy.ConnectError, which is what pilot-daemon's http.DefaultTransport
// does after refreshing its own credentials. A refusal the proxy garbles
// never reaches that transport's hook; it is retried once after
// unreadableReplyRetryDelay, by when the transport's resolver has usually
// re-read the credentials in the background.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pilot-protocol/common/netproxy"
)

// EnvProxyCommand is the environment variable holding the refresh command
// (see Config.ProxyCommand). It is common/netproxy's EnvRefreshCommand,
// which pilot-daemon and pilotctl read too.
const EnvProxyCommand = netproxy.EnvRefreshCommand

// EnvProxy is pilot-daemon's proxy setting. Only its "off" words matter
// here: with the daemon's proxy turned off, no refresh command is run.
const EnvProxy = "PILOT_PROXY"

// config.json keys pilot-daemon reads its proxy settings from.
const (
	configProxyKey    = "proxy"
	configProxyCmdKey = "proxy_cmd"
)

// fetchTimeout bounds one HTTP request of a tick.
const fetchTimeout = 30 * time.Second

// proxyOffWords are the settings of PILOT_PROXY / config.json "proxy" that
// turn pilot-daemon's proxy off.
var proxyOffWords = map[string]bool{"off": true, "none": true, "no": true, "false": true, "direct": true}

// proxyCommand returns the refresh command for the default client:
// cfg.ProxyCommand, else $PILOT_PROXY_CMD, else "proxy_cmd" in
// ~/.pilot/config.json. It returns "" when none is set, and when
// PILOT_PROXY (or, without it, config.json "proxy") turns the proxy off.
func proxyCommand(cfg Config) string {
	if c := strings.TrimSpace(cfg.ProxyCommand); c != "" {
		return c
	}
	conf := readProxyConfig(cfg.Home)
	setting, fromEnv := os.LookupEnv(EnvProxy)
	if !fromEnv || strings.TrimSpace(setting) == "" {
		setting = conf.proxy
	}
	if proxyOffWords[strings.ToLower(strings.TrimSpace(setting))] {
		return ""
	}
	if c := strings.TrimSpace(os.Getenv(EnvProxyCommand)); c != "" {
		return c
	}
	return strings.TrimSpace(conf.proxyCmd)
}

// proxyConfig is the proxy part of ~/.pilot/config.json.
type proxyConfig struct {
	proxy    string
	proxyCmd string
}

// readProxyConfig reads "proxy" and "proxy_cmd" from ~/.pilot/config.json.
// A missing or unreadable file, or a value that is not a string, reads as
// unset.
func readProxyConfig(home string) proxyConfig {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return proxyConfig{}
		}
		home = h
	}
	b, err := os.ReadFile(configFilePath(home))
	if err != nil {
		return proxyConfig{}
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return proxyConfig{}
	}
	str := func(key string) string {
		var s string
		if v, ok := raw[key]; ok && json.Unmarshal(v, &s) == nil {
			return s
		}
		return ""
	}
	return proxyConfig{proxy: str(configProxyKey), proxyCmd: str(configProxyCmdKey)}
}

// defaultHTTPClient builds the client used when Config.HTTPClient is nil.
// refreshing reports that it re-reads the proxy credentials with a refresh
// command (and so retries a 407 itself); it then owns its transport.
func defaultHTTPClient(cfg Config) (c *http.Client, refreshing bool) {
	cmd := proxyCommand(cfg)
	if cmd == "" {
		return &http.Client{Timeout: fetchTimeout}, false
	}
	r, err := netproxy.NewResolver(netproxy.ModeAuto,
		netproxy.WithRefreshCommand(cmd),
		netproxy.WithRefreshErrorHandler(func(err error) {
			slog.Warn("skillinject: proxy credential refresh failed; keeping the last proxy settings", "err", err)
		}))
	if err != nil {
		// An unusable HTTPS_PROXY. net/http rejects it as well; its error
		// then names the problem on every fetch.
		slog.Warn("skillinject: proxy settings unusable; fetching without the credential refresh", "err", err)
		return &http.Client{Timeout: fetchTimeout}, false
	}
	return &http.Client{
		Timeout:   fetchTimeout,
		Transport: netproxy.RefreshingTransport(proxyBaseTransport(), r),
	}, true
}

// proxyBaseTransport returns the transport the refreshing client is built
// on: a copy of http.DefaultTransport without its CONNECT response hook. A
// host that routes DefaultTransport through its own proxy resolver (as
// pilot-daemon does) installs a hook that turns a 407 into its own error,
// which would hide the refusal from RefreshingTransport's retry. A
// variable so tests can add their CA.
var proxyBaseTransport = func() *http.Transport {
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil // RefreshingTransport starts from a zero http.Transport
	}
	tr := dt.Clone()
	tr.OnProxyConnectResponse = nil
	return tr
}

// proxyAuthRejected reports whether err is a proxy refusing the
// credentials with 407, as a netproxy-aware transport reports it.
func proxyAuthRejected(err error) bool {
	var ce *netproxy.ConnectError
	return errors.As(err, &ce) && ce.StatusCode == http.StatusProxyAuthRequired
}

// unreadableReplyRetryDelay is how long get waits before retrying a fetch
// whose proxy answer could not be parsed (see unreadableProxyReply). A
// variable so tests can shorten it.
var unreadableReplyRetryDelay = 2 * time.Second

// unreadableProxyFaults are net/http's complaints about a response it
// cannot parse (the ones common/netproxy treats as a garbled CONNECT
// answer). Only these fixed descriptions are matched; the rest of such a
// message quotes the offending bytes.
var unreadableProxyFaults = []string{
	"malformed HTTP status code",
	"malformed HTTP response",
	"malformed HTTP version",
	"malformed MIME header",
}

// unreadableProxyReply reports whether err is net/http failing to parse a
// response ("malformed HTTP status code", ...), as it does for a proxy
// that rejects expired credentials with a garbled CONNECT answer (Meta
// Muse's). netproxy reports the same case from its own transports as
// "read CONNECT response: ... (response text withheld)".
func unreadableProxyReply(err error) bool {
	msg := err.Error()
	for _, fault := range unreadableProxyFaults {
		if strings.Contains(msg, fault) {
			return true
		}
	}
	return false
}

// sleepCtx waits d, or until ctx is done; it reports whether d passed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
