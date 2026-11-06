// Package linkmeta resolves a small amount of metadata about a linked page —
// currently just its <title> — for threads created from a link.
//
// Fetching arbitrary user-supplied URLs server-side is an SSRF risk, so the
// fetcher is deliberately paranoid: http(s) only, no proxy, bounded time and
// body size, and every connection (including redirect targets) is rejected if
// it resolves to a loopback, private, link-local, multicast or unspecified
// address. The address check happens inside the dialer's Control hook, i.e. on
// the IP that is actually connected to, which also defeats DNS rebinding.
package linkmeta

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const (
	// maxBodyBytes caps how much of the response we read. Titles live in
	// <head>, so this is far more than enough and bounds memory use.
	maxBodyBytes = 256 << 10
	// maxRedirects bounds a redirect chain; each hop is re-validated by the
	// dialer because the Control hook runs per connection.
	maxRedirects = 3
	// fetchTimeout bounds the whole request, including redirects.
	fetchTimeout = 6 * time.Second
	// maxTitleLength caps the returned title in runes. The caller still
	// applies its own storage limit; this only keeps pathological titles out
	// of the rest of the system.
	maxTitleLength = 300
	// userAgent identifies the bot; some sites return a friendlier page to a
	// browser-like agent, but being honest is better than being blocked.
	userAgent = "Disjoint/1.0 (link title fetch)"
)

// ErrBlocked is returned when the URL targets a scheme, host, or address that
// the fetcher refuses to touch (including anything on the private network).
var ErrBlocked = errors.New("link points to a blocked address")

// ErrNoTitle is returned when the page loads but has no usable <title>.
var ErrNoTitle = errors.New("page has no title")

// FetchTitle retrieves the <title> of an http(s) URL.
func FetchTitle(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%w: not an http(s) URL", ErrBlocked)
	}
	// Reject literal IPs and hosts that cannot resolve early, for a clear
	// error instead of a dial timeout. The dialer still guards the real
	// connection (and every redirect), which is what actually matters.
	if addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname()); err == nil {
		for _, addr := range addrs {
			if !addressAllowed(addr) {
				return "", ErrBlocked
			}
		}
	}

	client := &http.Client{
		Timeout:   fetchTimeout,
		Transport: safeTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("%w: too many redirects", ErrBlocked)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return ErrBlocked
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("link returned HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" &&
		!strings.Contains(strings.ToLower(ct), "text/html") &&
		!strings.Contains(strings.ToLower(ct), "application/xhtml") {
		return "", fmt.Errorf("link is not an HTML page")
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", err
	}
	return titleFromHTML(string(body))
}

// safeTransport dials directly (never via a proxy) and refuses connections to
// addresses outside the public internet.
func safeTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   4 * time.Second,
		KeepAlive: -1,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return ErrBlocked
			}
			addr, err := netip.ParseAddr(host)
			if err != nil {
				return ErrBlocked
			}
			if !addressAllowed(addr) {
				return ErrBlocked
			}
			return nil
		},
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		MaxIdleConns:          1,
	}
}

// addressAllowed reports whether a resolved IP is on the public internet.
// It is a variable so tests can opt into loopback httptest servers; production
// code never reassigns it.
var addressAllowed = func(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() ||
		addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() {
		return false
	}
	// Carrier-grade NAT (100.64.0.0/10) is not covered by IsPrivate but is just
	// as internal, and cloud metadata endpoints live on link-local (blocked
	// above) or 169.254.169.254 (also link-local).
	if addr.Is4() {
		b := addr.As4()
		if b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
			return false
		}
	}
	return true
}

// titleFromHTML extracts and cleans the first <title>…</title> in the document.
func titleFromHTML(doc string) (string, error) {
	lower := strings.ToLower(doc)
	open := strings.Index(lower, "<title")
	if open < 0 {
		return "", ErrNoTitle
	}
	rest := lower[open+len("<title"):]
	// Tolerate attributes on the tag (e.g. <title lang="en">).
	gt := strings.Index(rest, ">")
	if gt < 0 {
		return "", ErrNoTitle
	}
	start := open + len("<title") + gt + 1
	end := strings.Index(lower[start:], "</title")
	if end < 0 {
		return "", ErrNoTitle
	}
	raw := doc[start : start+end]

	// Collapse all whitespace (newlines inside titles are common) and decode
	// entities so "&amp;" becomes "&" before it is escaped again at render.
	title := strings.Join(strings.Fields(html.UnescapeString(raw)), " ")
	title = strings.TrimSpace(title)
	if title == "" {
		return "", ErrNoTitle
	}

	runes := []rune(title)
	if len(runes) > maxTitleLength {
		title = strings.TrimSpace(string(runes[:maxTitleLength])) + "…"
	}
	return title, nil
}
