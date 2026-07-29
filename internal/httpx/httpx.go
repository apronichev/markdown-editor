// Package httpx holds small HTTP helpers shared by every handler.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
)

// Error is an error that carries the HTTP status the client should see.
type Error struct {
	Status  int
	Message string
	err     error
}

func (e *Error) Error() string {
	if e.err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.err)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.err }

// Errorf builds an *Error with the given status.
func Errorf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a status and a client-safe message to an internal error.
func Wrap(status int, err error, message string) *Error {
	return &Error{Status: status, Message: message, err: err}
}

// JSON writes v as an ok JSON response.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("httpx: encode response: %v", err)
	}
}

// Fail writes err as a JSON error body, defaulting to 500 for unknown errors.
// Internal detail is logged, never sent to the client.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	msg := "internal server error"

	var he *Error
	if errors.As(err, &he) {
		status = he.Status
		msg = he.Message
	}
	if status >= 500 {
		log.Printf("httpx: %s %s -> %d: %v", r.Method, r.URL.Path, status, err)
	}
	JSON(w, status, map[string]string{"error": msg})
}

// DecodeJSON reads a JSON request body with a size cap and rejects unknown fields.
func DecodeJSON(r *http.Request, maxBytes int64, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !hasJSONContentType(ct) {
		return Errorf(http.StatusUnsupportedMediaType, "expected application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return Errorf(http.StatusRequestEntityTooLarge, "request body too large")
		}
		return Wrap(http.StatusBadRequest, err, "malformed JSON body")
	}
	// Reject trailing content so "{}{}" cannot smuggle a second document.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return Errorf(http.StatusBadRequest, "unexpected trailing data in body")
	}
	return nil
}

func hasJSONContentType(ct string) bool {
	for i := 0; i < len(ct); i++ {
		if ct[i] == ';' {
			ct = ct[:i]
			break
		}
	}
	switch trimSpace(ct) {
	case "application/json", "text/json":
		return true
	}
	return false
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// SecurityHeaders applies a strict baseline policy to every response.
func SecurityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self'; " +
		"img-src 'self' data: https:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'none'; " +
		"object-src 'none'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}
