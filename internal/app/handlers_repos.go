package app

import (
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/art-pro/markdown-editor/internal/auth"
	"github.com/art-pro/markdown-editor/internal/github"
	"github.com/art-pro/markdown-editor/internal/httpx"
)

// handleListRepos returns one page of repositories the token can reach,
// optionally filtered by a substring of the full name.
func (a *App) handleListRepos(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}

	repos, hasMore, err := a.clientFor(sess).ListRepos(r.Context(), page, 100)
	if err != nil {
		failGitHub(w, r, err)
		return
	}

	if filter := strings.ToLower(strings.TrimSpace(q.Get("q"))); filter != "" {
		kept := repos[:0]
		for _, repo := range repos {
			if strings.Contains(strings.ToLower(repo.FullName), filter) ||
				strings.Contains(strings.ToLower(repo.Description), filter) {
				kept = append(kept, repo)
			}
		}
		repos = kept
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"repos":    repos,
		"page":     page,
		"has_more": hasMore,
	})
}

// handleGetRepo looks up a single repository, which is how "connect a repo by
// name" verifies that the user really has access to it.
func (a *App) handleGetRepo(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	t, err := target(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	repo, err := a.clientFor(sess).GetRepo(r.Context(), t.Owner, t.Repo)
	if err != nil {
		failGitHub(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, repo)
}

// handleBranches lists the repository's branches.
func (a *App) handleBranches(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	t, err := target(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	branches, err := a.clientFor(sess).ListBranches(r.Context(), t.Owner, t.Repo)
	if err != nil {
		failGitHub(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"branches": branches})
}

// treeNode is a file or folder in the response tree.
type treeNode struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
	SHA  string `json:"sha,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// handleTree lists the Markdown files in a ref, plus every folder that leads to
// one, so the sidebar can render a complete document tree in a single request.
func (a *App) handleTree(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	t, err := target(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	q := r.URL.Query()
	ref, err := cleanRef(q.Get("ref"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	client := a.clientFor(sess)
	if ref == "" {
		repo, err := client.GetRepo(r.Context(), t.Owner, t.Repo)
		if err != nil {
			failGitHub(w, r, err)
			return
		}
		ref = repo.DefaultBranch
	}

	tree, err := client.GetTree(r.Context(), t.Owner, t.Repo, ref)
	if err != nil {
		if github.IsNotFound(err) {
			// A brand-new repository has no commits and therefore no tree.
			httpx.JSON(w, http.StatusOK, map[string]any{
				"ref": ref, "nodes": []treeNode{}, "truncated": false, "empty": true,
			})
			return
		}
		failGitHub(w, r, err)
		return
	}

	includeAll := q.Get("all") == "1"
	nodes := make([]treeNode, 0, len(tree.Entries))
	dirs := map[string]bool{}

	for _, entry := range tree.Entries {
		if entry.Type != "blob" {
			continue
		}
		if !includeAll && !isMarkdown(entry.Path) && path.Base(entry.Path) != ".gitkeep" {
			continue
		}
		// .gitkeep only exists to hold an otherwise-empty folder; register the
		// folder but do not show the placeholder as a document.
		if path.Base(entry.Path) == ".gitkeep" {
			registerDirs(dirs, entry.Path)
			continue
		}
		nodes = append(nodes, treeNode{
			Path: entry.Path,
			Name: path.Base(entry.Path),
			Type: "file",
			SHA:  entry.SHA,
			Size: entry.Size,
		})
		registerDirs(dirs, entry.Path)
	}

	for dir := range dirs {
		nodes = append(nodes, treeNode{Path: dir, Name: path.Base(dir), Type: "dir"})
	}

	// Folders first, then files, alphabetically within each folder.
	sort.Slice(nodes, func(i, j int) bool {
		di, dj := path.Dir(nodes[i].Path), path.Dir(nodes[j].Path)
		if di != dj {
			return di < dj
		}
		if nodes[i].Type != nodes[j].Type {
			return nodes[i].Type == "dir"
		}
		return strings.ToLower(nodes[i].Path) < strings.ToLower(nodes[j].Path)
	})

	httpx.JSON(w, http.StatusOK, map[string]any{
		"ref":       ref,
		"nodes":     nodes,
		"truncated": tree.Truncated,
		"empty":     len(tree.Entries) == 0,
	})
}

// registerDirs records every ancestor folder of a file path.
func registerDirs(dirs map[string]bool, filePath string) {
	dir := path.Dir(filePath)
	for dir != "." && dir != "/" && dir != "" {
		dirs[dir] = true
		dir = path.Dir(dir)
	}
}

// handleRaw streams an image out of the repository so private-repo images show
// up in the preview. Only image types are served, always with a fixed
// Content-Type and nosniff, so this cannot be used to host active content.
func (a *App) handleRaw(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	t, err := target(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	q := r.URL.Query()
	filePath, err := cleanPath(q.Get("path"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ref, err := cleanRef(q.Get("ref"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	contentType, ok := imageContentType(filePath)
	if !ok {
		httpx.Fail(w, r, httpx.Errorf(http.StatusUnsupportedMediaType, "only images can be loaded through this endpoint"))
		return
	}

	client := a.clientFor(sess)
	file, err := client.GetFile(r.Context(), t.Owner, t.Repo, filePath, ref)
	if err != nil {
		failGitHub(w, r, err)
		return
	}
	data, err := client.GetBlobRaw(r.Context(), t.Owner, t.Repo, file.SHA)
	if err != nil {
		failGitHub(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", "inline")
	h.Set("X-Content-Type-Options", "nosniff")
	// Blob content is immutable for a given SHA, but the URL is keyed by ref, so
	// keep the cache private and short.
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if _, err := w.Write(data); err != nil {
		return
	}
}
