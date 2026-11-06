package linkmeta

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// allowLoopback lets the tests talk to httptest servers (which listen on
// 127.0.0.1) without weakening the production policy.
func allowLoopback(t *testing.T) {
	t.Helper()
	prev := addressAllowed
	addressAllowed = func(netip.Addr) bool { return true }
	t.Cleanup(func() { addressAllowed = prev })
}

func TestFetchTitle(t *testing.T) {
	allowLoopback(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/good":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!doctype html><html><head><title>  Real
				Page   Title &amp; Friends </title></head><body>x</body></html>`))
		case "/attrs":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<title lang="en">Attribute Title</title>`))
		case "/none":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>no title here</body></html>`))
		case "/empty":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<title>   </title>`))
		case "/pdf":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte(`<title>ignored</title>`))
		case "/missing":
			http.NotFound(w, r)
		case "/redirect":
			http.Redirect(w, r, "/good", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cases := []struct {
		path    string
		want    string
		wantErr error
	}{
		{path: "/good", want: "Real Page Title & Friends"},
		{path: "/attrs", want: "Attribute Title"},
		{path: "/redirect", want: "Real Page Title & Friends"},
		{path: "/none", wantErr: ErrNoTitle},
		{path: "/empty", wantErr: ErrNoTitle},
	}
	for _, tc := range cases {
		got, err := FetchTitle(context.Background(), srv.URL+tc.path)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("FetchTitle(%s) err = %v, want %v", tc.path, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("FetchTitle(%s) err = %v", tc.path, err)
			continue
		}
		if got != tc.want {
			t.Errorf("FetchTitle(%s) = %q, want %q", tc.path, got, tc.want)
		}
	}

	// Non-2xx, non-HTML, and redirect loops are plain errors (not titles).
	for _, path := range []string{"/missing", "/pdf", "/loop"} {
		if got, err := FetchTitle(context.Background(), srv.URL+path); err == nil {
			t.Errorf("FetchTitle(%s) = %q, want an error", path, got)
		}
	}

	// Scheme and host validation happens before any request.
	for _, bad := range []string{"file:///etc/passwd", "ftp://example.com/x", "not a url", ""} {
		if _, err := FetchTitle(context.Background(), bad); !errors.Is(err, ErrBlocked) {
			t.Errorf("FetchTitle(%q) err = %v, want ErrBlocked", bad, err)
		}
	}
}

func TestFetchTitleBlocksPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<title>internal</title>`))
	}))
	defer srv.Close()

	// With the real policy, loopback must be refused before connecting. The
	// error may come from the pre-flight lookup or the dialer, so only assert
	// that nothing was fetched successfully.
	if got, err := FetchTitle(context.Background(), srv.URL); err == nil {
		t.Fatalf("FetchTitle(loopback) = %q, want an error", got)
	}

	for _, addr := range []string{
		"127.0.0.1", "::1", "10.0.0.5", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "::ffff:127.0.0.1",
		"224.0.0.1", "ff02::1",
	} {
		a, err := netip.ParseAddr(addr)
		if err != nil {
			t.Fatalf("parse %s: %v", addr, err)
		}
		if addressAllowed(a) {
			t.Errorf("addressAllowed(%s) = true, want false", addr)
		}
	}

	for _, addr := range []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946", "8.8.8.8"} {
		a, err := netip.ParseAddr(addr)
		if err != nil {
			t.Fatalf("parse %s: %v", addr, err)
		}
		if !addressAllowed(a) {
			t.Errorf("addressAllowed(%s) = false, want true", addr)
		}
	}
}

func TestTitleFromHTMLLimits(t *testing.T) {
	long := strings.Repeat("a", maxTitleLength+50)
	got, err := titleFromHTML("<title>" + long + "</title>")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long title was not truncated: %q", got)
	}
	if len([]rune(got)) > maxTitleLength+1 {
		t.Errorf("truncated title is still %d runes", len([]rune(got)))
	}

	// The scan takes the first <title> in the document; a string literal in an
	// earlier <script> would win. That is a known limit of a substring scan and
	// harmless (the value is escaped at render), but pin it so a future change
	// to a real parser notices the difference.
	got, err = titleFromHTML(`<script>var t = "<title>inline</title>";</script><title>Document</title>`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "inline" {
		t.Errorf("first <title> wins, got %q", got)
	}
}
