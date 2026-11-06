// Package passkeys implements WebAuthn (passkey) registration and sign-in on
// top of a users table that keeps working with passwords. Passkeys are an
// additional way in, never a replacement, so losing a device can never lock an
// account out.
package passkeys

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config is the Relying Party identity. WebAuthn requires that the RP ID is a
// registrable domain suffix of every allowed origin, and browsers refuse to
// use a credential whose origin does not match exactly, so these values are
// configuration rather than being derived per-request from r.Host.
type Config struct {
	RPID          string
	RPOrigins     []string
	RPDisplayName string
}

// ErrConfig is returned for a configuration that WebAuthn would reject.
var ErrConfig = errors.New("invalid webauthn configuration")

// ConfigFromEnv reads WEBAUTHN_RP_ID and WEBAUTHN_RP_ORIGINS, falling back to
// PUBLIC_BASE_URL and finally to http://localhost for development.
func ConfigFromEnv() (Config, error) {
	return configFrom(os.Getenv)
}

func configFrom(getenv func(string) string) (Config, error) {
	base := strings.TrimSpace(getenv("PUBLIC_BASE_URL"))
	baseURL, _ := url.Parse(base)

	rpID := strings.TrimSpace(getenv("WEBAUTHN_RP_ID"))
	if rpID == "" && baseURL != nil && baseURL.Hostname() != "" {
		rpID = baseURL.Hostname()
	}

	origins := splitList(getenv("WEBAUTHN_RP_ORIGINS"))
	if len(origins) == 0 {
		switch {
		case base != "":
			origins = []string{strings.TrimSuffix(base, "/")}
		case rpID != "":
			origins = []string{"https://" + rpID}
		default:
			origins = []string{"http://localhost"}
		}
	}
	if rpID == "" {
		// Derive the dev fallback from the origin so they always agree.
		u, err := url.Parse(origins[0])
		if err != nil || u.Hostname() == "" {
			return Config{}, fmt.Errorf("%w: cannot determine RP ID", ErrConfig)
		}
		rpID = u.Hostname()
	}

	cfg := Config{
		RPID:          strings.ToLower(strings.TrimSuffix(rpID, ".")),
		RPOrigins:     origins,
		RPDisplayName: "Disjoint",
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks the invariants WebAuthn itself enforces at startup, so a
// typo fails loudly in the deploy log rather than silently breaking sign-in.
func (c Config) Validate() error {
	if c.RPID == "" {
		return fmt.Errorf("%w: RP ID is empty", ErrConfig)
	}
	if len(c.RPOrigins) == 0 {
		return fmt.Errorf("%w: no allowed origins", ErrConfig)
	}
	for _, origin := range c.RPOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%w: origin %q is not an absolute http(s) URL", ErrConfig, origin)
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if host != c.RPID && !strings.HasSuffix(host, "."+c.RPID) {
			return fmt.Errorf("%w: origin %q is not a subdomain of RP ID %q", ErrConfig, origin, c.RPID)
		}
		if u.Scheme == "https" && u.Port() != "" && u.Port() != "443" {
			return fmt.Errorf("%w: origin %q has a non-default https port", ErrConfig, origin)
		}
	}
	return nil
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, strings.TrimSuffix(part, "/"))
		}
	}
	return out
}
