package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Cookie is one entry of the observation jar the kernel may attach to an
// http probe. The probe reads it and never writes it back.
type Cookie struct {
	Name   string
	Value  string
	Domain string
	Path   string
	Secure bool
}

const (
	httpDialTimeout   = 10 * time.Second
	httpMaxBodyBytes  = 4 * 1024 * 1024
	httpUserAgent     = "measurement-probe/1"
	errRedirectStatus = "redirects are not followed"
)

// resolver looks a host up; tests substitute one that answers from a map.
type resolver func(ctx context.Context, host string) ([]net.IPAddr, error)

func systemResolver(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// addressAllowed refuses every address that is not a public unicast one:
// private ranges, loopback, link-local (the cloud metadata service lives
// there), unspecified, multicast and unique-local IPv6.
func addressAllowed(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsLoopback(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsMulticast(), ip.IsUnspecified(), ip.IsPrivate(), ip.IsInterfaceLocalMulticast():
		return false
	}
	if ip.To4() != nil {
		// Carrier-grade NAT and the documentation/benchmark ranges are not
		// reachable services; refuse them too.
		if ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127 {
			return false
		}
		if ip[0] == 0 || ip[0] >= 224 {
			return false
		}
	}
	return true
}

// httpProbeResult carries the recorded fields; the body is never kept.
type httpProbeResult struct {
	status    int
	timeTotal time.Duration
	bytes     int64
	rotated   bool
	failure   string
}

// runHTTP performs one GET or HEAD with a dialer that resolves the host,
// checks every address, and connects to an allowed one only. Redirects are
// not followed; the response body is counted and discarded; Set-Cookie for
// a jar cookie marks the measurement rotated and is not written anywhere.
// httpTestHooks relax the address and certificate checks for tests that
// stand up a local TLS server. Production code never sets them.
type httpTestHooks struct {
	allowLoopback bool
	tlsConfig     *tls.Config
	port          string
}

func runHTTP(ctx context.Context, plan Plan, jar []Cookie, lookup resolver) (httpProbeResult, execResult) {
	return runHTTPWith(ctx, plan, jar, lookup, httpTestHooks{})
}

func runHTTPWith(ctx context.Context, plan Plan, jar []Cookie, lookup resolver, hooks httpTestHooks) (httpProbeResult, execResult) {
	host := plan.Spec.Hosts[0]
	if len(plan.Spec.Hosts) > 1 {
		if want, ok := plan.Args["host"]; ok {
			host = want
		}
	}
	allowed := false
	for _, candidate := range plan.Spec.Hosts {
		if candidate == host {
			allowed = true
		}
	}
	if !allowed {
		return httpProbeResult{}, execResult{exitCode: -1, failure: "refused: host is not in the probe's list"}
	}
	timeout := time.Duration(plan.Spec.Timeout()) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dialer := &net.Dialer{Timeout: httpDialTimeout}
	var dialFailure error
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialHost, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if dialHost != host {
				return nil, fmt.Errorf("dial to %q refused: not the probe's host", dialHost)
			}
			if port != "443" && hooks.port == "" {
				return nil, fmt.Errorf("dial to port %s refused: probes speak https on 443 only", port)
			}
			addresses, err := lookup(ctx, dialHost)
			if err != nil {
				return nil, err
			}
			if len(addresses) == 0 {
				return nil, errors.New("host resolved to no address")
			}
			for _, address := range addresses {
				if hooks.allowLoopback && address.IP.IsLoopback() {
					continue
				}
				if !addressAllowed(address.IP) {
					dialFailure = fmt.Errorf("refused: %s resolves to a private, loopback or link-local address", dialHost)
					return nil, dialFailure
				}
			}
			if hooks.port != "" {
				port = hooks.port
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
		},
		TLSClientConfig:       tlsConfigFor(host, hooks),
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	url := "https://" + host + plan.Args["path"]
	request, err := http.NewRequestWithContext(ctx, plan.Args["method"], url, nil)
	if err != nil {
		return httpProbeResult{}, execResult{exitCode: -1, failure: err.Error()}
	}
	request.Header.Set("User-Agent", httpUserAgent)
	request.Header.Set("Accept", "*/*")
	jarNames := map[string]bool{}
	if plan.Spec.Cookies == CookiesObservationJar {
		for _, cookie := range jar {
			if !cookieApplies(cookie, host, plan.Args["path"]) {
				continue
			}
			request.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
			jarNames[cookie.Name] = true
		}
	}
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		if dialFailure != nil {
			return httpProbeResult{}, execResult{exitCode: -1, failure: dialFailure.Error()}
		}
		return httpProbeResult{}, execResult{exitCode: -1, failure: safeHTTPError(err), timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	}
	defer response.Body.Close()
	counted, _ := io.Copy(io.Discard, io.LimitReader(response.Body, httpMaxBodyBytes))
	result := httpProbeResult{status: response.StatusCode, timeTotal: time.Since(started), bytes: counted}
	for _, set := range response.Cookies() {
		if jarNames[set.Name] {
			result.rotated = true
		}
	}
	fields := plan.Spec.Returns
	if len(fields) == 0 {
		fields = []string{"status", "time_total", "bytes"}
	}
	var parts []string
	for _, field := range fields {
		switch field {
		case "status":
			parts = append(parts, fmt.Sprintf("status=%d", result.status))
		case "time_total":
			parts = append(parts, fmt.Sprintf("time_total=%.3f", result.timeTotal.Seconds()))
		case "bytes":
			parts = append(parts, fmt.Sprintf("bytes=%d", result.bytes))
		}
	}
	if result.rotated {
		parts = append(parts, "rotated=true")
	}
	return result, execResult{output: strings.Join(parts, " ") + "\n", total: len(strings.Join(parts, " ")) + 1}
}

func tlsConfigFor(host string, hooks httpTestHooks) *tls.Config {
	if hooks.tlsConfig != nil {
		config := hooks.tlsConfig.Clone()
		config.ServerName = host
		return config
	}
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// cookieApplies follows the jar's own domain and path rules, read-only.
func cookieApplies(cookie Cookie, host, path string) bool {
	domain := strings.TrimPrefix(strings.ToLower(cookie.Domain), ".")
	if domain == "" {
		return false
	}
	if host != domain && !strings.HasSuffix(host, "."+domain) {
		return false
	}
	cookiePath := cookie.Path
	if cookiePath == "" {
		cookiePath = "/"
	}
	return strings.HasPrefix(path, cookiePath)
}

// safeHTTPError drops anything after the scheme separator so that a URL
// with a query never reaches the record through an error message.
func safeHTTPError(err error) string {
	message := err.Error()
	if at := strings.Index(message, "?"); at >= 0 {
		message = message[:at] + "?…"
	}
	return message
}
