package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/art-pro/markdown-editor/internal/auth"
	"github.com/art-pro/markdown-editor/internal/config"
)

const testOrigin = "https://editor.example"

func newTestApp(t *testing.T) (*App, *auth.Manager) {
	t.Helper()
	t.Setenv("GITHUB_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_CLIENT_SECRET", "client-secret")
	t.Setenv("SESSION_SECRET", strings.Repeat("k", 48))
	t.Setenv("APP_BASE_URL", testOrigin)
	t.Setenv("VERCEL", "1")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return New(cfg), auth.NewManager(cfg)
}

// sessionCookie mints a valid session cookie with a known CSRF token.
func sessionCookie(t *testing.T, m *auth.Manager, csrf string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	err := m.Issue(rec, &auth.Session{Token: "gho_test", Login: "octocat", CSRF: csrf})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie issued")
	return nil
}

func do(app *App, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

func TestHealthAndConfigArePublic(t *testing.T) {
	app, _ := newTestApp(t)

	for _, path := range []string{"/api/health", "/api/config", "/api/formats"} {
		rec := do(app, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	app, _ := newTestApp(t)
	rec := do(app, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP is missing %q; got %q", directive, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP must not allow unsafe sources: %q", csp)
	}
}

func TestMeReportsUnauthenticatedWithoutCookie(t *testing.T) {
	app, _ := newTestApp(t)
	rec := do(app, httptest.NewRequest(http.MethodGet, "/api/me", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["authenticated"] != false {
		t.Errorf("authenticated = %v, want false", body["authenticated"])
	}
	if _, leaked := body["csrf_token"]; leaked {
		t.Error("a CSRF token was handed out without a session")
	}
}

func TestMeReturnsCSRFTokenWithSession(t *testing.T) {
	app, manager := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(sessionCookie(t, manager, "the-csrf-token"))

	rec := do(app, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["authenticated"] != true || body["csrf_token"] != "the-csrf-token" {
		t.Errorf("unexpected body: %v", body)
	}
	if body["login"] != "octocat" {
		t.Errorf("login = %v, want octocat", body["login"])
	}
	// The access token must never be part of the JSON handed to the browser.
	if strings.Contains(rec.Body.String(), "gho_test") {
		t.Errorf("/api/me leaked the access token: %s", rec.Body.String())
	}
}

// TestProtectedEndpointsRequireSession walks every authenticated route.
func TestProtectedEndpointsRequireSession(t *testing.T) {
	app, _ := newTestApp(t)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/repos"},
		{http.MethodGet, "/api/repos/octocat/notes"},
		{http.MethodGet, "/api/repos/octocat/notes/branches"},
		{http.MethodGet, "/api/repos/octocat/notes/tree"},
		{http.MethodGet, "/api/repos/octocat/notes/file?path=a.md"},
		{http.MethodGet, "/api/repos/octocat/notes/raw?path=a.png"},
		{http.MethodPut, "/api/repos/octocat/notes/file"},
		{http.MethodPost, "/api/repos/octocat/notes/delete"},
		{http.MethodPost, "/api/repos/octocat/notes/move"},
		{http.MethodPost, "/api/repos/octocat/notes/commit"},
		{http.MethodPost, "/api/render"},
		{http.MethodPost, "/api/export"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := do(app, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// TestMutatingEndpointsRequireCSRF proves a session cookie alone is not enough.
func TestMutatingEndpointsRequireCSRF(t *testing.T) {
	app, manager := newTestApp(t)
	cookie := sessionCookie(t, manager, "real-token")

	cases := []struct{ method, path string }{
		{http.MethodPut, "/api/repos/octocat/notes/file"},
		{http.MethodPost, "/api/repos/octocat/notes/delete"},
		{http.MethodPost, "/api/repos/octocat/notes/move"},
		{http.MethodPost, "/api/repos/octocat/notes/commit"},
		{http.MethodPost, "/api/export"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			// No CSRF header at all.
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(cookie)
			if rec := do(app, req); rec.Code != http.StatusForbidden {
				t.Errorf("without CSRF header = %d, want 403", rec.Code)
			}

			// A wrong CSRF header.
			req = httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(auth.CSRFHeader, "forged-token")
			req.AddCookie(cookie)
			if rec := do(app, req); rec.Code != http.StatusForbidden {
				t.Errorf("with forged CSRF header = %d, want 403", rec.Code)
			}
		})
	}
}

func TestMutatingEndpointsRejectCrossOrigin(t *testing.T) {
	app, manager := newTestApp(t)
	cookie := sessionCookie(t, manager, "real-token")

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"foreign Origin", map[string]string{"Origin": "https://evil.example"}},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site but not same-origin", map[string]string{"Sec-Fetch-Site": "same-site"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/export", strings.NewReader(`{"markdown":"hi","format":"html"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(auth.CSRFHeader, "real-token")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			req.AddCookie(cookie)

			if rec := do(app, req); rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestRenderReturnsSanitizedHTML(t *testing.T) {
	app, manager := newTestApp(t)
	body := `{"markdown":"# Hi\n\n<script>alert(1)</script>\n\n**bold**"}`

	req := httptest.NewRequest(http.MethodPost, "/api/render", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(sessionCookie(t, manager, "tok"))

	rec := do(app, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct{ HTML string }
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(payload.HTML, "<strong>bold</strong>") {
		t.Errorf("expected rendered Markdown, got %q", payload.HTML)
	}
	if strings.Contains(payload.HTML, "<script") {
		t.Errorf("script tag survived: %q", payload.HTML)
	}
}

func TestRenderRejectsUnknownFields(t *testing.T) {
	app, manager := newTestApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/render", strings.NewReader(`{"markdown":"x","evil":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookie(t, manager, "tok"))

	if rec := do(app, req); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestExportProducesAttachment(t *testing.T) {
	app, manager := newTestApp(t)
	body := `{"markdown":"# Title\n\ntext","format":"styled-html","title":"my notes.md"}`

	req := httptest.NewRequest(http.MethodPost, "/api/export", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeader, "tok")
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(sessionCookie(t, manager, "tok"))

	rec := do(app, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment") || !strings.Contains(disposition, "my notes.html") {
		t.Errorf("Content-Disposition = %q", disposition)
	}
	if !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
		t.Error("expected a standalone HTML document")
	}
}

func TestExportRejectsUnknownFormat(t *testing.T) {
	app, manager := newTestApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/export", strings.NewReader(`{"markdown":"x","format":"exe"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeader, "tok")
	req.AddCookie(sessionCookie(t, manager, "tok"))

	if rec := do(app, req); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestInvalidRepoNamesAreRejectedBeforeGitHub(t *testing.T) {
	app, manager := newTestApp(t)
	cookie := sessionCookie(t, manager, "tok")

	// A path-traversal attempt in the owner segment must not reach the API layer.
	for _, path := range []string{
		"/api/repos/..%2f..%2fetc/notes/tree",
		"/api/repos/-bad/notes/tree",
		"/api/repos/octocat/-bad/tree",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := do(app, req)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 400 or 404", path, rec.Code)
		}
	}
}

func TestRawEndpointRejectsNonImages(t *testing.T) {
	app, manager := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/api/repos/octocat/notes/raw?path=index.html", nil)
	req.AddCookie(sessionCookie(t, manager, "tok"))

	// Rejected on content type before any GitHub call, so no network is needed.
	if rec := do(app, req); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", rec.Code)
	}
}

func TestUnknownAPIRouteIs404JSON(t *testing.T) {
	app, _ := newTestApp(t)
	rec := do(app, httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
}

func TestStylesheetsAreServed(t *testing.T) {
	app, _ := newTestApp(t)

	for _, path := range []string{"/api/assets/document.css", "/api/assets/highlight.css"} {
		rec := do(app, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
			t.Errorf("%s Content-Type = %q", path, ct)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s served an empty stylesheet", path)
		}
	}
}

func TestUnconfiguredAppStillAnswersConfig(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "")
	t.Setenv("GITHUB_CLIENT_SECRET", "")
	t.Setenv("SESSION_SECRET", "")

	cfg, err := config.Load()
	if err == nil {
		t.Fatal("expected config.Load to report the missing values")
	}
	app := New(cfg)

	rec := do(app, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d, want 200 so the UI can explain the problem", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["configured"] != false {
		t.Errorf("configured = %v, want false", body["configured"])
	}

	// Login must fail loudly rather than redirect to a broken GitHub URL.
	rec = do(app, httptest.NewRequest(http.MethodGet, "/api/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /api/auth/login = %d, want 503", rec.Code)
	}
}

func TestLoginRedirectsToGitHub(t *testing.T) {
	app, _ := newTestApp(t)
	rec := do(app, httptest.NewRequest(http.MethodGet, "/api/auth/login", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "https://github.com/login/oauth/authorize?") {
		t.Fatalf("Location = %q", location)
	}
	wantRedirect := "redirect_uri=" + url.QueryEscape(testOrigin+"/api/auth/callback")
	for _, want := range []string{"client_id=client-id", "state=", "scope=repo", wantRedirect} {
		if !strings.Contains(location, want) {
			t.Errorf("Location missing %q: %s", want, location)
		}
	}
	// The state must be pinned in a cookie so the callback can verify it.
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.StateCookie && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Error("login did not set the OAuth state cookie")
	}
}

func TestCallbackRejectsMissingState(t *testing.T) {
	app, _ := newTestApp(t)
	rec := do(app, httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state=xyz", nil))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
