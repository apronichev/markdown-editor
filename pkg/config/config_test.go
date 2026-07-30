package config

import (
	"strings"
	"testing"
)

func TestGitHubWebURL(t *testing.T) {
	cases := map[string]string{
		// The default and the explicit public API both mean github.com.
		"":                        "https://github.com",
		"https://api.github.com":  "https://github.com",
		"https://api.github.com/": "https://github.com",
		// GitHub Enterprise Server: strip the API suffix back to the host.
		"https://git.acme.com/api/v3":  "https://git.acme.com",
		"https://git.acme.com/api/v3/": "https://git.acme.com",
		"http://localhost:3000/api/v3": "http://localhost:3000",
	}

	for apiBase, want := range cases {
		t.Setenv("GITHUB_CLIENT_ID", "id")
		t.Setenv("GITHUB_CLIENT_SECRET", "secret")
		t.Setenv("SESSION_SECRET", strings.Repeat("s", 48))
		t.Setenv("GITHUB_API_BASE_URL", apiBase)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.GitHubWebURL(); got != want {
			t.Errorf("GITHUB_API_BASE_URL=%q -> GitHubWebURL() = %q, want %q", apiBase, got, want)
		}
	}
}

func TestScopeAndAPIDefaults(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "id")
	t.Setenv("GITHUB_CLIENT_SECRET", "secret")
	t.Setenv("SESSION_SECRET", strings.Repeat("s", 48))
	t.Setenv("GITHUB_OAUTH_SCOPES", "")
	t.Setenv("GITHUB_API_BASE_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubScopes != "repo read:user" {
		t.Errorf("GitHubScopes = %q", cfg.GitHubScopes)
	}
	if cfg.GitHubAPIBaseURL != "https://api.github.com" {
		t.Errorf("GitHubAPIBaseURL = %q", cfg.GitHubAPIBaseURL)
	}

	t.Setenv("GITHUB_OAUTH_SCOPES", "public_repo read:user")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubScopes != "public_repo read:user" {
		t.Errorf("an explicit scope should win, got %q", cfg.GitHubScopes)
	}
}
