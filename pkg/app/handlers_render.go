package app

import (
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/art-pro/markdown-editor/pkg/auth"
	"github.com/art-pro/markdown-editor/pkg/export"
	"github.com/art-pro/markdown-editor/pkg/httpx"
	"github.com/art-pro/markdown-editor/pkg/markdown"
)

// maxDocumentBytes caps the Markdown source accepted for rendering or export.
const maxDocumentBytes = 2 << 20

// renderContext optionally tells the renderer which repository a document came
// from, so relative image paths can be resolved through the authenticated proxy.
type renderContext struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Ref   string `json:"ref"`
	Path  string `json:"path"`
}

type renderRequest struct {
	Markdown string         `json:"markdown"`
	Context  *renderContext `json:"context,omitempty"`
}

// handleRender converts Markdown to sanitized HTML for the live preview.
//
// Rendering server-side keeps one implementation shared with every export, so
// the preview is a faithful picture of the exported file.
func (a *App) handleRender(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req renderRequest
	if err := httpx.DecodeJSON(r, maxDocumentBytes+(1<<16), &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if len(req.Markdown) > maxDocumentBytes {
		httpx.Fail(w, r, httpx.Errorf(http.StatusRequestEntityTooLarge, "document is larger than 2 MiB"))
		return
	}

	opts := markdown.Options{}
	if resolver := a.assetResolver(req.Context); resolver != nil {
		opts.ResolveAsset = resolver
	}

	html, err := markdown.Render([]byte(req.Markdown), opts)
	if err != nil {
		httpx.Fail(w, r, httpx.Wrap(http.StatusUnprocessableEntity, err, "could not render this document"))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"html": html})
}

// assetResolver builds a rewriter that points relative image sources at the
// authenticated raw proxy. It returns nil when there is no repository context.
func (a *App) assetResolver(ctx *renderContext) func(string) string {
	if ctx == nil {
		return nil
	}
	if !ownerRepoPattern.MatchString(ctx.Owner) || !ownerRepoPattern.MatchString(ctx.Repo) {
		return nil
	}
	ref, err := cleanRef(ctx.Ref)
	if err != nil {
		return nil
	}
	docPath, err := cleanPath(ctx.Path)
	if err != nil {
		// A yet-unsaved document has no path; resolve against the repository root.
		docPath = "index.md"
	}

	base := "/api/repos/" + url.PathEscape(ctx.Owner) + "/" + url.PathEscape(ctx.Repo) + "/raw"
	return func(src string) string {
		// Strip any query or fragment before resolving against the tree.
		clean := src
		if i := strings.IndexAny(clean, "?#"); i >= 0 {
			clean = clean[:i]
		}
		if unescaped, err := url.PathUnescape(clean); err == nil {
			clean = unescaped
		}
		resolved := resolveRelative(docPath, clean)
		if resolved == "" {
			return ""
		}
		if _, ok := imageContentType(resolved); !ok {
			return ""
		}
		q := url.Values{}
		q.Set("path", resolved)
		if ref != "" {
			q.Set("ref", ref)
		}
		return base + "?" + q.Encode()
	}
}

type exportRequest struct {
	Markdown string        `json:"markdown"`
	Format   export.Format `json:"format"`
	Title    string        `json:"title"`
}

// handleExport renders the document into a downloadable file.
func (a *App) handleExport(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req exportRequest
	if err := httpx.DecodeJSON(r, maxDocumentBytes+(1<<16), &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if len(req.Markdown) > maxDocumentBytes {
		httpx.Fail(w, r, httpx.Errorf(http.StatusRequestEntityTooLarge, "document is larger than 2 MiB"))
		return
	}
	if _, ok := export.Lookup(req.Format); !ok {
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "unsupported export format %q", req.Format))
		return
	}

	result, err := export.Run([]byte(req.Markdown), req.Format, export.Options{Title: req.Title})
	if err != nil {
		httpx.Fail(w, r, httpx.Wrap(http.StatusUnprocessableEntity, err, "could not export this document"))
		return
	}

	h := w.Header()
	h.Set("Content-Type", result.ContentType)
	// mime.FormatMediaType handles the quoting and the UTF-8 filename* form, so a
	// document title with spaces or non-ASCII characters cannot break the header.
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": result.Filename}))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if _, err := w.Write(result.Body); err != nil {
		return
	}
}

// handleFormats lists the export formats for the "Export as" menu.
func (a *App) handleFormats(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{"formats": export.Formats()})
}

// The stylesheets are generated once per cold start and reused.
var (
	documentCSS  = sync.OnceValue(export.DocumentCSS)
	highlightCSS = sync.OnceValue(markdown.HighlightCSS)
)

func (a *App) handleDocumentCSS(w http.ResponseWriter, r *http.Request) {
	serveCSS(w, documentCSS())
}

func (a *App) handleHighlightCSS(w http.ResponseWriter, r *http.Request) {
	serveCSS(w, highlightCSS())
}

// serveCSS writes a generated stylesheet that clients may cache.
func serveCSS(w http.ResponseWriter, css string) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(css))
}
