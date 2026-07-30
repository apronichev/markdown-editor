// Package app wires configuration, sessions and handlers into one http.Handler.
package app

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/art-pro/markdown-editor/pkg/auth"
	"github.com/art-pro/markdown-editor/pkg/config"
	"github.com/art-pro/markdown-editor/pkg/github"
	"github.com/art-pro/markdown-editor/pkg/httpx"
)

// App holds the request-independent state of the service.
type App struct {
	cfg      *config.Config
	sessions *auth.Manager
	oauth    *auth.Handlers
	mux      *http.ServeMux
}

// New builds the application handler.
func New(cfg *config.Config) *App {
	sessions := auth.NewManager(cfg)
	a := &App{
		cfg:      cfg,
		sessions: sessions,
		oauth:    auth.NewHandlers(cfg, sessions),
		mux:      http.NewServeMux(),
	}
	a.routes()
	return a
}

// ServeHTTP implements http.Handler.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httpx.SecurityHeaders(a.mux).ServeHTTP(w, r)
}

func (a *App) routes() {
	m := a.mux

	// Public endpoints.
	m.HandleFunc("GET /api/health", a.handleHealth)
	m.HandleFunc("GET /api/config", a.handleConfig)
	m.HandleFunc("GET /api/formats", a.handleFormats)
	m.HandleFunc("GET /api/assets/document.css", a.handleDocumentCSS)
	m.HandleFunc("GET /api/assets/highlight.css", a.handleHighlightCSS)

	// Authentication.
	m.HandleFunc("GET /api/auth/login", a.oauth.Login)
	m.HandleFunc("GET /api/auth/callback", a.oauth.Callback)
	m.HandleFunc("POST /api/auth/logout", a.oauth.Logout)
	m.HandleFunc("GET /api/me", a.handleMe)

	// Repositories.
	m.HandleFunc("GET /api/repos", a.authed(a.handleListRepos))
	m.HandleFunc("GET /api/repos/{owner}/{repo}", a.authed(a.handleGetRepo))
	m.HandleFunc("GET /api/repos/{owner}/{repo}/branches", a.authed(a.handleBranches))
	m.HandleFunc("GET /api/repos/{owner}/{repo}/tree", a.authed(a.handleTree))
	m.HandleFunc("GET /api/repos/{owner}/{repo}/raw", a.authed(a.handleRaw))

	// Files. Reads are GET; every write goes through the mutating path, which
	// additionally requires a valid CSRF token and a same-origin request.
	m.HandleFunc("GET /api/repos/{owner}/{repo}/file", a.authed(a.handleGetFile))
	m.HandleFunc("PUT /api/repos/{owner}/{repo}/file", a.mutating(a.handlePutFile))
	m.HandleFunc("POST /api/repos/{owner}/{repo}/delete", a.mutating(a.handleDeletePath))
	m.HandleFunc("POST /api/repos/{owner}/{repo}/move", a.mutating(a.handleMovePath))
	m.HandleFunc("POST /api/repos/{owner}/{repo}/commit", a.mutating(a.handleBatchCommit))

	// Rendering and export do not touch GitHub, but they stay behind the session
	// so the deployment cannot be used as an open Markdown-rendering service.
	m.HandleFunc("POST /api/render", a.authed(a.handleRender))
	m.HandleFunc("POST /api/export", a.mutating(a.handleExport))

	m.HandleFunc("/", a.handleNotFound)
}

func (a *App) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		httpx.Fail(w, r, httpx.Errorf(http.StatusNotFound, "no such endpoint: %s %s", r.Method, r.URL.Path))
		return
	}
	// Static files are served by the platform (or by cmd/server locally); a hit
	// here means the SPA shell is missing.
	httpx.Fail(w, r, httpx.Errorf(http.StatusNotFound, "not found"))
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"configured": a.cfg.Ready(),
		"time":       time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"configured": a.cfg.Ready(),
		"scopes":     a.cfg.GitHubScopes,
	})
}

// handleMe reports the signed-in user and hands the SPA its CSRF token.
func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, err := a.sessions.Read(r)
	if err != nil {
		httpx.JSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
			"configured":    a.cfg.Ready(),
		})
		return
	}
	// Slide the expiry so an active user is not logged out mid-session.
	if err := a.sessions.Issue(w, sess); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"configured":    true,
		"login":         sess.Login,
		"name":          sess.Name,
		"avatar_url":    sess.AvatarURL,
		"csrf_token":    sess.CSRF,
		// Lets the UI build links to GitHub that also work on Enterprise.
		"github_url": a.cfg.GitHubWebURL(),
	})
}

// handler is a session-aware handler.
type handler func(w http.ResponseWriter, r *http.Request, sess *auth.Session)

// authed requires a valid session.
func (a *App) authed(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := a.sessions.Read(r)
		if err != nil {
			httpx.Fail(w, r, httpx.Errorf(http.StatusUnauthorized, "please sign in with GitHub"))
			return
		}
		next(w, r, sess)
	}
}

// mutating requires a valid session, a same-origin request and a CSRF token.
func (a *App) mutating(next handler) http.HandlerFunc {
	return a.authed(func(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
		if err := a.checkOrigin(r); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		if err := auth.CheckCSRF(r, sess); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		next(w, r, sess)
	})
}

// checkOrigin rejects cross-site writes. Browsers always send Origin on
// state-changing fetches, so a mismatch is a forgery attempt.
func (a *App) checkOrigin(r *http.Request) error {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return httpx.Errorf(http.StatusForbidden, "cross-site request refused")
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	got, err := url.Parse(origin)
	if err != nil {
		return httpx.Errorf(http.StatusForbidden, "invalid Origin header")
	}
	want, err := url.Parse(a.cfg.BaseURLFor(r))
	if err != nil {
		return httpx.Errorf(http.StatusInternalServerError, "cannot determine own origin")
	}
	if !strings.EqualFold(got.Host, want.Host) {
		return httpx.Errorf(http.StatusForbidden, "cross-origin request refused")
	}
	return nil
}

// clientFor builds a GitHub client for this session's token, honouring a custom
// API root when the deployment points at GitHub Enterprise Server.
func (a *App) clientFor(sess *auth.Session) *github.Client {
	return github.NewWithBase(sess.Token, a.cfg.GitHubAPIBaseURL)
}

// failGitHub translates a GitHub client error into our own API error.
func failGitHub(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := github.Status(err)
	httpx.Fail(w, r, httpx.Wrap(status, err, msg))
}
