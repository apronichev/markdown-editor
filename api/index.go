// Package handler is the Vercel serverless entrypoint.
//
// Vercel builds every file under api/ into its own function and calls the
// exported Handler. vercel.json rewrites all of /api/* here, so this single
// function hosts the whole API and the routing lives in internal/app.
package handler

import (
	"log"
	"net/http"
	"sync"

	"github.com/art-pro/markdown-editor/pkg/app"
	"github.com/art-pro/markdown-editor/pkg/config"
)

// build is done once per cold start and reused by every warm invocation.
var build = sync.OnceValue(func() http.Handler {
	cfg, err := config.Load()
	if err != nil {
		// Not fatal: the app still serves /api/config and /api/health so the UI
		// can tell the user exactly which environment variable is missing.
		log.Printf("startup: %v", err)
	}
	return app.New(cfg)
})

// Handler is the entrypoint Vercel invokes for every request.
func Handler(w http.ResponseWriter, r *http.Request) {
	build().ServeHTTP(w, r)
}
