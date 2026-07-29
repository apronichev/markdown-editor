package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/art-pro/markdown-editor/pkg/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("GITHUB_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_CLIENT_SECRET", "client-secret")
	t.Setenv("SESSION_SECRET", strings.Repeat("s", 48))
	t.Setenv("APP_BASE_URL", "https://editor.example")
	// Pretend we are deployed, so cookie hardening matches production.
	t.Setenv("VERCEL", "1")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if !cfg.Ready() {
		t.Fatal("expected config to be ready")
	}
	return cfg
}

// issueTo seals a session and returns the resulting cookie.
func issueTo(t *testing.T, m *Manager, sess *Session) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := m.Issue(rec, sess); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie was set")
	return nil
}

func TestSessionRoundTrip(t *testing.T) {
	m := NewManager(testConfig(t))
	cookie := issueTo(t, m, &Session{Token: "gho_secret", Login: "octocat", Name: "Mona", CSRF: "csrf-value"})

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)

	got, err := m.Read(req)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Token != "gho_secret" || got.Login != "octocat" || got.CSRF != "csrf-value" {
		t.Errorf("round trip lost data: %+v", got)
	}
	if got.Expired() {
		t.Error("a freshly issued session should not be expired")
	}
}

func TestSessionCookieHardening(t *testing.T) {
	m := NewManager(testConfig(t))
	rec := httptest.NewRecorder()
	if err := m.Issue(rec, &Session{Token: "t", Login: "u", CSRF: "c"}); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	cookie := rec.Result().Cookies()[0]

	if !cookie.HttpOnly {
		t.Error("session cookie must be HttpOnly so scripts cannot read the token")
	}
	if !cookie.Secure {
		t.Error("session cookie must be Secure outside development")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if strings.Contains(cookie.Value, "gho_") || strings.Contains(cookie.Value, "t") && len(cookie.Value) < 20 {
		t.Error("cookie value looks unencrypted")
	}
}

// TestSessionCookieHidesToken is the property that matters most: the sealed
// cookie must not leak the access token to anything that can read the header.
func TestSessionCookieHidesToken(t *testing.T) {
	m := NewManager(testConfig(t))
	const token = "gho_verySecretAccessToken"
	cookie := issueTo(t, m, &Session{Token: token, Login: "octocat", CSRF: "c"})

	if strings.Contains(cookie.Value, token) {
		t.Fatalf("token appears verbatim in the cookie: %s", cookie.Value)
	}
	if strings.Contains(cookie.Value, "octocat") {
		t.Fatalf("login appears verbatim in the cookie: %s", cookie.Value)
	}
}

func TestSessionRejectsTampering(t *testing.T) {
	m := NewManager(testConfig(t))
	cookie := issueTo(t, m, &Session{Token: "tok", Login: "octocat", CSRF: "c"})

	// Flip one character of the sealed payload; GCM authentication must fail.
	mutated := []byte(cookie.Value)
	if mutated[len(mutated)-1] == 'A' {
		mutated[len(mutated)-1] = 'B'
	} else {
		mutated[len(mutated)-1] = 'A'
	}

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: string(mutated)})
	if _, err := m.Read(req); err == nil {
		t.Error("a tampered cookie was accepted")
	}
}

func TestSessionRejectsForeignKey(t *testing.T) {
	m := NewManager(testConfig(t))
	cookie := issueTo(t, m, &Session{Token: "tok", Login: "octocat", CSRF: "c"})

	// Re-load the config with a different secret, simulating another deployment.
	t.Setenv("SESSION_SECRET", strings.Repeat("x", 48))
	other, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	otherManager := NewManager(other)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)
	if _, err := otherManager.Read(req); err == nil {
		t.Error("a cookie sealed with a different key was accepted")
	}
}

func TestSessionRejectsExpired(t *testing.T) {
	m := NewManager(testConfig(t))
	// Seal a payload whose expiry is already in the past.
	value, err := m.seal(&Session{Token: "tok", Login: "u", CSRF: "c", ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: value})

	if _, err := m.Read(req); err == nil {
		t.Error("an expired session was accepted")
	}
}

func TestReadWithoutCookie(t *testing.T) {
	m := NewManager(testConfig(t))
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	if _, err := m.Read(req); err != ErrNoSession {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

func TestClearExpiresCookies(t *testing.T) {
	m := NewManager(testConfig(t))
	rec := httptest.NewRecorder()
	m.Clear(rec)

	cookies := rec.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("expected the session and state cookies to be cleared, got %d", len(cookies))
	}
	for _, c := range cookies {
		if c.MaxAge >= 0 {
			t.Errorf("cookie %s has MaxAge %d, want negative", c.Name, c.MaxAge)
		}
	}
}

func TestCheckCSRF(t *testing.T) {
	sess := &Session{CSRF: "expected-token"}

	cases := []struct {
		name   string
		header string
		wantOK bool
	}{
		{"matching", "expected-token", true},
		{"missing", "", false},
		{"wrong", "some-other-token", false},
		{"prefix only", "expected", false},
		{"case differs", "Expected-Token", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/render", nil)
			if tc.header != "" {
				req.Header.Set(CSRFHeader, tc.header)
			}
			err := CheckCSRF(req, sess)
			if (err == nil) != tc.wantOK {
				t.Errorf("CheckCSRF() error = %v, wantOK %v", err, tc.wantOK)
			}
		})
	}
}

func TestOAuthStateRoundTrip(t *testing.T) {
	m := NewManager(testConfig(t))

	rec := httptest.NewRecorder()
	nonce, err := m.issueState(rec, "/docs")
	if err != nil {
		t.Fatalf("issueState: %v", err)
	}
	stateCookie := rec.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback", nil)
	req.AddCookie(stateCookie)

	st, err := m.consumeState(httptest.NewRecorder(), req, nonce)
	if err != nil {
		t.Fatalf("consumeState: %v", err)
	}
	if st.Redirect != "/docs" {
		t.Errorf("Redirect = %q, want /docs", st.Redirect)
	}
}

func TestOAuthStateRejectsMismatch(t *testing.T) {
	m := NewManager(testConfig(t))
	rec := httptest.NewRecorder()
	if _, err := m.issueState(rec, ""); err != nil {
		t.Fatalf("issueState: %v", err)
	}
	stateCookie := rec.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback", nil)
	req.AddCookie(stateCookie)

	if _, err := m.consumeState(httptest.NewRecorder(), req, "attacker-supplied-state"); err == nil {
		t.Error("consumeState accepted a state value that did not match the cookie")
	}
}

func TestOAuthStateRejectsMissingCookie(t *testing.T) {
	m := NewManager(testConfig(t))
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback", nil)
	if _, err := m.consumeState(httptest.NewRecorder(), req, "anything"); err == nil {
		t.Error("consumeState accepted a callback with no state cookie")
	}
}

func TestSafeRedirect(t *testing.T) {
	cases := map[string]string{
		"/":                        "/",
		"/docs/readme.md":          "/docs/readme.md",
		"":                         "",
		"//evil.example":           "",
		`/\evil.example`:           "",
		"https://evil.example":     "",
		"http://evil.example/path": "",
		"evil.example":             "",
		"javascript:alert(1)":      "",
		"../up":                    "",
	}
	for input, want := range cases {
		if got := safeRedirect(input); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNewCSRFTokenIsRandom(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		token, err := NewCSRFToken()
		if err != nil {
			t.Fatalf("NewCSRFToken: %v", err)
		}
		if len(token) < 32 {
			t.Fatalf("token %q is too short", token)
		}
		if seen[token] {
			t.Fatalf("duplicate token %q", token)
		}
		seen[token] = true
	}
}

func TestDevModeRelaxesSecureFlag(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "id")
	t.Setenv("GITHUB_CLIENT_SECRET", "secret")
	t.Setenv("SESSION_SECRET", strings.Repeat("s", 48))
	t.Setenv("VERCEL", "") // not on Vercel -> development

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Secure() {
		t.Error("expected Secure() to be false in development so http://localhost works")
	}

	t.Setenv("VERCEL", "1")
	prod, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if !prod.Secure() {
		t.Error("expected Secure() to be true when running on Vercel")
	}
}
