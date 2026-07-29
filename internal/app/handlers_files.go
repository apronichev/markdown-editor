package app

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/art-pro/markdown-editor/internal/auth"
	"github.com/art-pro/markdown-editor/internal/github"
	"github.com/art-pro/markdown-editor/internal/httpx"
)

// maxBodyBytes caps every JSON request body. Files themselves are capped at
// 2 MiB by the GitHub client; the extra room here covers base64 and metadata.
const maxBodyBytes = 8 << 20

// handleGetFile loads a file's text and the SHA needed to update it later.
func (a *App) handleGetFile(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
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

	file, err := a.clientFor(sess).GetFile(r.Context(), t.Owner, t.Repo, filePath, ref)
	if err != nil {
		failGitHub(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"path":    file.Path,
		"sha":     file.SHA,
		"size":    file.Size,
		"content": file.Content,
		"ref":     ref,
	})
}

// putFileRequest is a single-file save.
type putFileRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Message string `json:"message"`
	Branch  string `json:"branch"`
	// SHA is the blob SHA the edit is based on. Empty means "create new file";
	// a stale value makes GitHub reject the write instead of clobbering someone.
	SHA string `json:"sha"`
}

// handlePutFile creates or updates one file and pushes the commit.
func (a *App) handlePutFile(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	t, err := target(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req putFileRequest
	if err := httpx.DecodeJSON(r, maxBodyBytes, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	filePath, err := cleanPath(req.Path)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	branch, err := requireRef(req.Branch)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	message := commitMessage(req.Message, defaultMessage(req.SHA, filePath))

	commit, err := a.clientFor(sess).PutFile(r.Context(), t.Owner, t.Repo, filePath, req.Content, message, branch, req.SHA)
	if err != nil {
		if github.IsConflict(err) {
			httpx.Fail(w, r, httpx.Wrap(http.StatusConflict, err,
				"this file changed on GitHub since you opened it — reload it, then reapply your edit"))
			return
		}
		failGitHub(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, commit)
}

func defaultMessage(sha, filePath string) string {
	if sha == "" {
		return fmt.Sprintf("Create %s", filePath)
	}
	return fmt.Sprintf("Update %s", filePath)
}

// deleteRequest removes a file or a whole folder.
type deleteRequest struct {
	Path    string `json:"path"`
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Branch  string `json:"branch"`
	// Recursive deletes every file beneath Path as one commit.
	Recursive bool `json:"recursive"`
}

// handleDeletePath deletes a file, or a folder and everything under it.
func (a *App) handleDeletePath(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	t, err := target(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req deleteRequest
	if err := httpx.DecodeJSON(r, maxBodyBytes, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	targetPath, err := cleanPath(req.Path)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	branch, err := requireRef(req.Branch)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	client := a.clientFor(sess)

	if !req.Recursive {
		message := commitMessage(req.Message, "Delete "+targetPath)
		sha := req.SHA
		if sha == "" {
			// Resolve the current SHA so the caller does not have to track it.
			file, err := client.GetFile(r.Context(), t.Owner, t.Repo, targetPath, branch)
			if err != nil {
				failGitHub(w, r, err)
				return
			}
			sha = file.SHA
		}
		commit, err := client.DeleteFile(r.Context(), t.Owner, t.Repo, targetPath, message, branch, sha)
		if err != nil {
			failGitHub(w, r, err)
			return
		}
		httpx.JSON(w, http.StatusOK, commit)
		return
	}

	// Folder delete: collect every blob under the prefix and drop them at once.
	tree, err := client.GetTree(r.Context(), t.Owner, t.Repo, branch)
	if err != nil {
		failGitHub(w, r, err)
		return
	}
	prefix := targetPath + "/"
	var changes []github.Change
	for _, entry := range tree.Entries {
		if entry.Type == "blob" && strings.HasPrefix(entry.Path, prefix) {
			changes = append(changes, github.Change{Path: entry.Path, Delete: true})
		}
	}
	if len(changes) == 0 {
		httpx.Fail(w, r, httpx.Errorf(http.StatusNotFound, "folder %q is empty or does not exist", targetPath))
		return
	}
	if tree.Truncated {
		httpx.Fail(w, r, httpx.Errorf(http.StatusUnprocessableEntity,
			"this repository's tree is too large to delete folders safely"))
		return
	}

	message := commitMessage(req.Message, fmt.Sprintf("Delete folder %s (%d files)", targetPath, len(changes)))
	commit, err := client.CommitChanges(r.Context(), t.Owner, t.Repo, branch, message, changes)
	if err != nil {
		failGitHub(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, commit)
}

// moveRequest renames or relocates a file or folder.
type moveRequest struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Message string `json:"message"`
	Branch  string `json:"branch"`
}

// handleMovePath moves a file or an entire folder in a single commit. Blobs are
// reused by SHA rather than re-uploaded, so a move is cheap and lossless.
func (a *App) handleMovePath(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	t, err := target(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req moveRequest
	if err := httpx.DecodeJSON(r, maxBodyBytes, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	from, err := cleanPath(req.From)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	to, err := cleanPath(req.To)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	branch, err := requireRef(req.Branch)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if from == to {
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "source and destination are the same"))
		return
	}
	if strings.HasPrefix(to+"/", from+"/") {
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "cannot move a folder into itself"))
		return
	}

	client := a.clientFor(sess)
	tree, err := client.GetTree(r.Context(), t.Owner, t.Repo, branch)
	if err != nil {
		failGitHub(w, r, err)
		return
	}
	if tree.Truncated {
		httpx.Fail(w, r, httpx.Errorf(http.StatusUnprocessableEntity,
			"this repository's tree is too large to move files safely"))
		return
	}

	existing := make(map[string]bool, len(tree.Entries))
	for _, entry := range tree.Entries {
		if entry.Type == "blob" {
			existing[entry.Path] = true
		}
	}

	var changes []github.Change
	moved := 0
	for _, entry := range tree.Entries {
		if entry.Type != "blob" {
			continue
		}
		var newPath string
		switch {
		case entry.Path == from:
			newPath = to
		case strings.HasPrefix(entry.Path, from+"/"):
			newPath = to + strings.TrimPrefix(entry.Path, from)
		default:
			continue
		}
		if existing[newPath] {
			httpx.Fail(w, r, httpx.Errorf(http.StatusConflict, "%s already exists", newPath))
			return
		}
		changes = append(changes,
			github.Change{Path: newPath, SHA: entry.SHA},
			github.Change{Path: entry.Path, Delete: true},
		)
		moved++
	}
	if moved == 0 {
		httpx.Fail(w, r, httpx.Errorf(http.StatusNotFound, "%q does not exist on %s", from, branch))
		return
	}

	fallback := fmt.Sprintf("Move %s to %s", from, to)
	if moved > 1 {
		fallback = fmt.Sprintf("Move %s to %s (%d files)", from, to, moved)
	}
	commit, err := client.CommitChanges(r.Context(), t.Owner, t.Repo, branch, commitMessage(req.Message, fallback), changes)
	if err != nil {
		failGitHub(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, commit)
}

// batchChange is one edit inside a multi-file commit.
type batchChange struct {
	Path    string  `json:"path"`
	Content *string `json:"content,omitempty"`
	Delete  bool    `json:"delete,omitempty"`
}

// batchRequest saves several documents in one push.
type batchRequest struct {
	Message string        `json:"message"`
	Branch  string        `json:"branch"`
	Changes []batchChange `json:"changes"`
}

// handleBatchCommit pushes several file writes and deletions as a single commit,
// which is what "save all open documents" uses.
func (a *App) handleBatchCommit(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	t, err := target(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req batchRequest
	if err := httpx.DecodeJSON(r, maxBodyBytes, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	branch, err := requireRef(req.Branch)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if len(req.Changes) == 0 {
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "no changes to commit"))
		return
	}
	if len(req.Changes) > 200 {
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "too many changes in one commit (max 200)"))
		return
	}

	seen := make(map[string]bool, len(req.Changes))
	changes := make([]github.Change, 0, len(req.Changes))
	for _, change := range req.Changes {
		filePath, err := cleanPath(change.Path)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		if seen[filePath] {
			httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "%s appears twice in one commit", filePath))
			return
		}
		seen[filePath] = true

		if change.Delete {
			changes = append(changes, github.Change{Path: filePath, Delete: true})
			continue
		}
		if change.Content == nil {
			httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "%s has no content", filePath))
			return
		}
		content := *change.Content
		changes = append(changes, github.Change{Path: filePath, Content: &content})
	}

	fallback := fmt.Sprintf("Update %d files", len(changes))
	if len(changes) == 1 {
		fallback = "Update " + changes[0].Path
	}
	commit, err := a.clientFor(sess).CommitChanges(r.Context(), t.Owner, t.Repo, branch, commitMessage(req.Message, fallback), changes)
	if err != nil {
		failGitHub(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, commit)
}

// commitMessage sanitizes a user-supplied message, falling back to a default.
func commitMessage(supplied, fallback string) string {
	msg := strings.TrimSpace(supplied)
	if msg == "" {
		return fallback
	}
	// Keep the subject line on one line and bounded.
	msg = strings.ReplaceAll(strings.ReplaceAll(msg, "\r\n", "\n"), "\r", "\n")
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	return msg
}
