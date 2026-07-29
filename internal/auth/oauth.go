package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/art-pro/markdown-editor/internal/config"
	"github.com/art-pro/markdown-editor/internal/github"
	"github.com/art-pro/markdown-editor/internal/httpx"
)

const (
	authorizeURL = "https://github.com/login/oauth/authorize"
	tokenURL     = "https://github.com/login/oauth/access_token"
)

// Handlers serves the OAuth endpoints.
type Handlers struct {
	cfg      *config.Config
	sessions *Manager
	client   *http.Client
}

// NewHandlers builds the OAuth handler set.
func NewHandlers(cfg *config.Config, sessions *Manager) *Handlers {
	return &Handlers{
		cfg:      cfg,
		sessions: sessions,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// Login starts the OAuth dance by redirecting to GitHub.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Ready() {
		httpx.Fail(w, r, httpx.Errorf(http.StatusServiceUnavailable,
			"the app is not configured: set GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET and SESSION_SECRET"))
		return
	}

	nonce, err := h.sessions.issueState(w, safeRedirect(r.URL.Query().Get("redirect")))
	if err != nil {
		httpx.Fail(w, r, httpx.Wrap(http.StatusInternalServerError, err, "could not start login"))
		return
	}

	q := url.Values{}
	q.Set("client_id", h.cfg.GitHubClientID)
	q.Set("redirect_uri", h.cfg.BaseURLFor(r)+"/api/auth/callback")
	q.Set("scope", h.cfg.GitHubScopes)
	q.Set("state", nonce)
	// Ask GitHub to always show the account picker rather than silently reusing one.
	q.Set("allow_signup", "false")

	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, authorizeURL+"?"+q.Encode(), http.StatusFound)
}

// Callback completes the OAuth dance and issues the session cookie.
func (h *Handlers) Callback(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Ready() {
		httpx.Fail(w, r, httpx.Errorf(http.StatusServiceUnavailable, "the app is not configured"))
		return
	}
	q := r.URL.Query()

	if authErr := q.Get("error"); authErr != "" {
		desc := q.Get("error_description")
		if desc == "" {
			desc = authErr
		}
		h.redirectWithError(w, r, desc)
		return
	}

	st, err := h.sessions.consumeState(w, r, q.Get("state"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	code := q.Get("code")
	if code == "" {
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "missing authorization code"))
		return
	}

	token, err := h.exchange(r.Context(), code, h.cfg.BaseURLFor(r)+"/api/auth/callback")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	user, err := github.NewWithBase(token, h.cfg.GitHubAPIBaseURL).CurrentUser(r.Context())
	if err != nil {
		httpx.Fail(w, r, httpx.Wrap(http.StatusBadGateway, err, "could not read your GitHub profile"))
		return
	}

	csrf, err := NewCSRFToken()
	if err != nil {
		httpx.Fail(w, r, httpx.Wrap(http.StatusInternalServerError, err, "could not create session"))
		return
	}

	sess := &Session{
		Token:     token,
		Login:     user.Login,
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
		CSRF:      csrf,
	}
	if err := h.sessions.Issue(w, sess); err != nil {
		httpx.Fail(w, r, httpx.Wrap(http.StatusInternalServerError, err, "could not create session"))
		return
	}

	dest := st.Redirect
	if dest == "" {
		dest = "/"
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, dest, http.StatusFound)
}

// Logout drops the session cookie.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	h.sessions.Clear(w)
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// exchange trades the authorization code for an access token.
func (h *Handlers) exchange(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{}
	form.Set("client_id", h.cfg.GitHubClientID)
	form.Set("client_secret", h.cfg.GitHubClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return "", httpx.Wrap(http.StatusBadGateway, err, "could not reach GitHub")
	}
	defer resp.Body.Close()

	var payload struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", httpx.Wrap(http.StatusBadGateway, err, "unexpected response from GitHub")
	}
	if payload.Error != "" {
		msg := payload.ErrorDesc
		if msg == "" {
			msg = payload.Error
		}
		return "", httpx.Errorf(http.StatusBadGateway, "GitHub rejected the login: %s", msg)
	}
	if payload.AccessToken == "" {
		return "", httpx.Errorf(http.StatusBadGateway, "GitHub returned no access token")
	}
	return payload.AccessToken, nil
}

func (h *Handlers) redirectWithError(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/login.html?error="+url.QueryEscape(msg), http.StatusFound)
}

// safeRedirect keeps post-login redirects on this site: a single leading slash,
// never a protocol-relative "//evil.com" or a backslash variant.
func safeRedirect(dest string) string {
	if dest == "" || !strings.HasPrefix(dest, "/") {
		return ""
	}
	if strings.HasPrefix(dest, "//") || strings.HasPrefix(dest, "/\\") {
		return ""
	}
	if u, err := url.Parse(dest); err != nil || u.Host != "" || u.Scheme != "" {
		return ""
	}
	return dest
}
