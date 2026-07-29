// Package config loads runtime configuration from the environment.
package config

import (
	"cmp"
	"crypto/sha256"
	"errors"
	"net/http"
	"os"
	"strings"
)

// Config holds everything the app needs to run. It is read once per cold start.
type Config struct {
	GitHubClientID     string
	GitHubClientSecret string
	GitHubScopes       string

	// SessionKey is a 32-byte AES-256 key derived from SESSION_SECRET.
	SessionKey []byte

	// BaseURL, when set, overrides the URL derived from the incoming request.
	BaseURL string

	// GitHubAPIBaseURL is the REST API root. Override it to point at a GitHub
	// Enterprise Server instance (for example https://github.acme.com/api/v3).
	GitHubAPIBaseURL string

	// Dev relaxes cookie flags so the app works over plain HTTP on localhost.
	Dev bool
}

// ErrNotConfigured means the OAuth app credentials or session secret are missing.
var ErrNotConfigured = errors.New("missing GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET or SESSION_SECRET")

// Load reads configuration from the environment. It returns a usable Config even
// when credentials are absent so the app can render a helpful setup page instead
// of crashing the whole function.
func Load() (*Config, error) {
	c := &Config{
		GitHubClientID:     strings.TrimSpace(os.Getenv("GITHUB_CLIENT_ID")),
		GitHubClientSecret: strings.TrimSpace(os.Getenv("GITHUB_CLIENT_SECRET")),
		BaseURL:            strings.TrimRight(strings.TrimSpace(os.Getenv("APP_BASE_URL")), "/"),
		// "repo" is the narrowest scope that can read *and* push to private repos.
		GitHubScopes: cmp.Or(strings.TrimSpace(os.Getenv("GITHUB_OAUTH_SCOPES")), "repo read:user"),
		GitHubAPIBaseURL: cmp.Or(
			strings.TrimRight(strings.TrimSpace(os.Getenv("GITHUB_API_BASE_URL")), "/"),
			"https://api.github.com",
		),
		Dev: os.Getenv("VERCEL") == "" && os.Getenv("MDE_DEV") != "0",
	}

	secret := os.Getenv("SESSION_SECRET")
	if len(strings.TrimSpace(secret)) < 32 {
		return c, ErrNotConfigured
	}
	// SHA-256 turns an arbitrary-length secret into an exact AES-256 key.
	sum := sha256.Sum256([]byte(secret))
	c.SessionKey = sum[:]

	if c.GitHubClientID == "" || c.GitHubClientSecret == "" {
		return c, ErrNotConfigured
	}
	return c, nil
}

// Ready reports whether the app has enough configuration to run OAuth.
func (c *Config) Ready() bool {
	return c.GitHubClientID != "" && c.GitHubClientSecret != "" && len(c.SessionKey) == 32
}

// Secure reports whether cookies should carry the Secure flag.
func (c *Config) Secure() bool { return !c.Dev }

// BaseURLFor resolves the externally visible origin for this request, preferring
// the explicit APP_BASE_URL so a spoofed Host header cannot rewrite OAuth URLs.
func (c *Config) BaseURLFor(r *http.Request) string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.Split(proto, ",")[0]
	} else if r.TLS == nil && isLoopback(r.Host) {
		scheme = "http"
	}
	host := cmp.Or(r.Header.Get("X-Forwarded-Host"), r.Host)
	return scheme + "://" + strings.Split(host, ",")[0]
}

func isLoopback(host string) bool {
	return strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "[::1]")
}
