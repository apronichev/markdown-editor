// Package auth implements GitHub OAuth and stateless, encrypted cookie sessions.
//
// There is no database: the GitHub access token lives inside an AES-256-GCM
// sealed cookie that only the server can open. The browser never sees it.
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/art-pro/markdown-editor/internal/config"
	"github.com/art-pro/markdown-editor/internal/httpx"
)

const (
	// SessionCookie holds the sealed session. HttpOnly, so JS cannot read it.
	SessionCookie = "mde_session"
	// StateCookie holds the short-lived OAuth CSRF state.
	StateCookie = "mde_oauth_state"

	// CSRFHeader is the header the browser must echo on mutating requests.
	CSRFHeader = "X-CSRF-Token"

	sessionTTL = 7 * 24 * time.Hour
	stateTTL   = 10 * time.Minute
)

// ErrNoSession means the request carried no usable session.
var ErrNoSession = errors.New("no session")

// Session is the decrypted contents of the session cookie.
type Session struct {
	Token     string `json:"tok"`
	Login     string `json:"login"`
	Name      string `json:"name,omitempty"`
	AvatarURL string `json:"avatar,omitempty"`
	CSRF      string `json:"csrf"`
	ExpiresAt int64  `json:"exp"`
}

// Expired reports whether the session is past its lifetime.
func (s *Session) Expired() bool { return time.Now().Unix() >= s.ExpiresAt }

// Manager seals and opens sessions using the configured key.
type Manager struct{ cfg *config.Config }

// NewManager builds a session manager.
func NewManager(cfg *config.Config) *Manager { return &Manager{cfg: cfg} }

// NewCSRFToken returns a fresh random CSRF token.
func NewCSRFToken() (string, error) { return randomToken(32) }

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (m *Manager) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(m.cfg.SessionKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal encrypts v into a URL-safe base64 string of nonce||ciphertext.
func (m *Manager) seal(v any) (string, error) {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	aead, err := m.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// open reverses seal. A tampered value fails GCM authentication.
func (m *Manager) open(value string, dst any) error {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	aead, err := m.aead()
	if err != nil {
		return err
	}
	if len(raw) < aead.NonceSize() {
		return errors.New("sealed value too short")
	}
	nonce, ciphertext := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(plaintext, dst)
}

// Issue seals sess into the response as the session cookie.
func (m *Manager) Issue(w http.ResponseWriter, sess *Session) error {
	sess.ExpiresAt = time.Now().Add(sessionTTL).Unix()
	value, err := m.seal(sess)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.cfg.Secure(),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(sess.ExpiresAt, 0),
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return nil
}

// Read opens the session cookie on r.
func (m *Manager) Read(r *http.Request) (*Session, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil, ErrNoSession
	}
	var sess Session
	if err := m.open(c.Value, &sess); err != nil {
		return nil, ErrNoSession
	}
	if sess.Token == "" || sess.Expired() {
		return nil, ErrNoSession
	}
	return &sess, nil
}

// Clear removes the session and state cookies.
func (m *Manager) Clear(w http.ResponseWriter) {
	for _, name := range []string{SessionCookie, StateCookie} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			Secure:   m.cfg.Secure(),
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Unix(0, 0),
			MaxAge:   -1,
		})
	}
}

// state is the sealed OAuth state cookie payload.
type state struct {
	Nonce     string `json:"n"`
	Redirect  string `json:"r,omitempty"`
	ExpiresAt int64  `json:"exp"`
}

// issueState mints an OAuth state nonce, storing it in a sealed cookie so the
// callback can prove the round-trip started on this site.
func (m *Manager) issueState(w http.ResponseWriter, redirect string) (string, error) {
	nonce, err := randomToken(24)
	if err != nil {
		return "", err
	}
	value, err := m.seal(&state{
		Nonce:     nonce,
		Redirect:  redirect,
		ExpiresAt: time.Now().Add(stateTTL).Unix(),
	})
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     StateCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.cfg.Secure(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(stateTTL.Seconds()),
	})
	return nonce, nil
}

// consumeState validates the state parameter against the sealed cookie.
func (m *Manager) consumeState(w http.ResponseWriter, r *http.Request, got string) (*state, error) {
	c, err := r.Cookie(StateCookie)
	if err != nil || c.Value == "" {
		return nil, httpx.Errorf(http.StatusBadRequest, "login session expired, please try again")
	}
	// The cookie is single-use regardless of the outcome.
	http.SetCookie(w, &http.Cookie{
		Name: StateCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: m.cfg.Secure(), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})

	var st state
	if err := m.open(c.Value, &st); err != nil {
		return nil, httpx.Errorf(http.StatusBadRequest, "invalid login state")
	}
	if time.Now().Unix() >= st.ExpiresAt {
		return nil, httpx.Errorf(http.StatusBadRequest, "login took too long, please try again")
	}
	if subtle.ConstantTimeCompare([]byte(st.Nonce), []byte(got)) != 1 {
		return nil, httpx.Errorf(http.StatusBadRequest, "login state mismatch")
	}
	return &st, nil
}

// CheckCSRF verifies the X-CSRF-Token header against the session-bound token.
// Because the expected value lives inside the encrypted cookie, an attacker who
// can neither read the cookie nor call /api/me cannot forge it.
func CheckCSRF(r *http.Request, sess *Session) error {
	got := r.Header.Get(CSRFHeader)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRF)) != 1 {
		return httpx.Errorf(http.StatusForbidden, "invalid or missing CSRF token")
	}
	return nil
}
