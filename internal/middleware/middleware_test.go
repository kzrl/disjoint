package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterAllowsWithinLimit(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute)
	handler := rl.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 4: expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}
}

func TestRateLimiterTracksPerIP(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)
	handler := rl.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Simulate two clients (RemoteAddr = "ip:port").
	first := httptest.NewRequest(http.MethodPost, "/login", nil)
	first.RemoteAddr = "1.2.3.4:5678"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, first)
	if rec.Code != http.StatusOK {
		t.Fatalf("first from 1.2.3.4: expected 200, got %d", rec.Code)
	}

	// Same IP, different port: still limited.
	second := httptest.NewRequest(http.MethodPost, "/login", nil)
	second.RemoteAddr = "1.2.3.4:9999"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, second)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second from 1.2.3.4: expected 429, got %d", rec.Code)
	}

	// Different IP: allowed.
	third := httptest.NewRequest(http.MethodPost, "/login", nil)
	third.RemoteAddr = "5.6.7.8:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, third)
	if rec.Code != http.StatusOK {
		t.Fatalf("request from 5.6.7.8: expected 200, got %d", rec.Code)
	}
}

func TestRateLimiterUsesCaddyRealIP(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)
	handler := rl.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Caddy (loopback) proxies two distinct clients via X-Real-IP.
	first := httptest.NewRequest(http.MethodPost, "/login", nil)
	first.RemoteAddr = "127.0.0.1:443"
	first.Header.Set("X-Real-IP", "10.0.0.2")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, first)
	if rec.Code != http.StatusOK {
		t.Fatalf("first from 10.0.0.2: expected 200, got %d", rec.Code)
	}

	// Same client again: limited.
	second := httptest.NewRequest(http.MethodPost, "/login", nil)
	second.RemoteAddr = "127.0.0.1:443"
	second.Header.Set("X-Real-IP", "10.0.0.2")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, second)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second from 10.0.0.2: expected 429, got %d", rec.Code)
	}

	// A different client is unaffected.
	third := httptest.NewRequest(http.MethodPost, "/login", nil)
	third.RemoteAddr = "127.0.0.1:443"
	third.Header.Set("X-Real-IP", "10.0.0.3")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, third)
	if rec.Code != http.StatusOK {
		t.Fatalf("request from 10.0.0.3: expected 200, got %d", rec.Code)
	}
}

func TestRateLimiterIgnoresSpoofedRealIP(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)
	handler := rl.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// A non-loopback peer claiming to be someone else: the header must be
	// ignored so attackers cannot spread their attempts across fake keys.
	for i, addr := range []string{"8.8.8.8:5555", "8.8.8.8:6666"} {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = addr
		req.Header.Set("X-Real-IP", fmt.Sprintf("10.0.0.%d", i))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		want := http.StatusOK
		if i == 1 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("attempt %d from 8.8.8.8: expected %d, got %d", i+1, want, rec.Code)
		}
	}
}

func TestSecurityHeadersSetOnAllResponses(t *testing.T) {
	handler := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}
