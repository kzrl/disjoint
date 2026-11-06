package passkeys

import (
	"errors"
	"strings"
	"testing"
)

func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantID   string
		wantOrig []string
		wantErr  bool
	}{
		{
			name:     "explicit values",
			env:      map[string]string{"WEBAUTHN_RP_ID": "disjoint.kzrl.org", "WEBAUTHN_RP_ORIGINS": "https://disjoint.kzrl.org"},
			wantID:   "disjoint.kzrl.org",
			wantOrig: []string{"https://disjoint.kzrl.org"},
		},
		{
			name:     "derived from PUBLIC_BASE_URL",
			env:      map[string]string{"PUBLIC_BASE_URL": "https://forum.example.com/"},
			wantID:   "forum.example.com",
			wantOrig: []string{"https://forum.example.com"},
		},
		{
			name:     "multiple origins and trailing slash",
			env:      map[string]string{"WEBAUTHN_RP_ID": "example.com", "WEBAUTHN_RP_ORIGINS": "https://example.com/, https://www.example.com"},
			wantID:   "example.com",
			wantOrig: []string{"https://example.com", "https://www.example.com"},
		},
		{
			name:     "dev fallback",
			env:      map[string]string{},
			wantID:   "localhost",
			wantOrig: []string{"http://localhost"},
		},
		{
			name:    "rp id not a suffix of origin",
			env:     map[string]string{"WEBAUTHN_RP_ID": "example.com", "WEBAUTHN_RP_ORIGINS": "https://evil.example"},
			wantErr: true,
		},
		{
			name:    "relative origin",
			env:     map[string]string{"WEBAUTHN_RP_ID": "example.com", "WEBAUTHN_RP_ORIGINS": "example.com"},
			wantErr: true,
		},
		{
			name:    "ftp origin",
			env:     map[string]string{"WEBAUTHN_RP_ID": "example.com", "WEBAUTHN_RP_ORIGINS": "ftp://example.com"},
			wantErr: true,
		},
		{
			name:    "non-default https port",
			env:     map[string]string{"WEBAUTHN_RP_ID": "example.com", "WEBAUTHN_RP_ORIGINS": "https://example.com:8443"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := configFrom(envMap(tc.env))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", cfg)
				}
				if !errors.Is(err, ErrConfig) {
					t.Fatalf("error = %v, want ErrConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.RPID != tc.wantID {
				t.Errorf("RPID = %q, want %q", cfg.RPID, tc.wantID)
			}
			if strings.Join(cfg.RPOrigins, ",") != strings.Join(tc.wantOrig, ",") {
				t.Errorf("origins = %v, want %v", cfg.RPOrigins, tc.wantOrig)
			}
			if cfg.RPDisplayName == "" {
				t.Errorf("display name is empty")
			}
		})
	}
}

func TestUserHandleRoundTrip(t *testing.T) {
	for _, id := range []int64{0, 1, 42, 1<<40 + 7} {
		handle := userHandle(id)
		if len(handle) != 8 {
			t.Fatalf("handle for %d is %d bytes", id, len(handle))
		}
		got, ok := userIDFromHandle(handle)
		if !ok || got != id {
			t.Fatalf("round trip for %d = %d (ok=%v)", id, got, ok)
		}
	}
	if _, ok := userIDFromHandle([]byte{1, 2, 3}); ok {
		t.Fatalf("short handle must be rejected")
	}
}
