// Command server runs the editor locally with the same handler Vercel uses.
//
//	go run ./cmd/server
//
// It adds one thing the serverless deployment gets from the platform: serving
// the static files in public/.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/art-pro/markdown-editor/internal/app"
	"github.com/art-pro/markdown-editor/internal/config"
)

func main() {
	addr := flag.String("addr", defaultAddr(), "address to listen on")
	static := flag.String("static", "public", "directory holding the static frontend")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Printf("warning: %v", err)
		log.Printf("the UI will load but sign-in stays disabled until those are set (see .env.example)")
	}

	api := app.New(cfg)
	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	mux.Handle("/", staticHandler(*static))

	server := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	// Shut down cleanly on Ctrl-C so an in-flight push is not cut off.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("markdown editor listening on http://%s", *addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func defaultAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return "127.0.0.1:3000"
}

// staticHandler serves public/, falling back to index.html so a deep link keeps
// working the way Vercel's static hosting would.
func staticHandler(dir string) http.Handler {
	root, err := filepath.Abs(dir)
	if err != nil {
		log.Fatalf("static dir: %v", err)
	}
	files := http.FileServer(http.Dir(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "/" {
			clean = "index.html"
		}
		if _, err := os.Stat(filepath.Join(root, clean)); err != nil {
			// Unknown path: hand back the shell rather than a bare 404.
			r = r.Clone(r.Context())
			r.URL.Path = "/index.html"
		}
		// Static assets change on every edit during development.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
