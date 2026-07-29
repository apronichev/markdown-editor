// Package github is a small, purpose-built client for the GitHub REST API.
//
// It covers exactly what the editor needs: listing repositories and branches,
// reading trees and blobs, and writing commits (single file, multi-file, moves
// and deletes) back to a branch.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	apiBase       = "https://api.github.com"
	apiVersion    = "2022-11-28"
	userAgent     = "markdown-editor/1.0"
	requestTimout = 20 * time.Second
)

// Client talks to the GitHub API as one authenticated user.
type Client struct {
	token string
	http  *http.Client
	// baseURL is the API root: api.github.com, a GitHub Enterprise Server
	// instance, or a stub in tests.
	baseURL string
}

// New builds a client bound to a user access token, talking to github.com.
func New(token string) *Client {
	return NewWithBase(token, apiBase)
}

// NewWithBase builds a client against a specific API root, which is how GitHub
// Enterprise Server deployments are supported. An empty baseURL means github.com.
func NewWithBase(token, baseURL string) *Client {
	if baseURL == "" {
		baseURL = apiBase
	}
	return &Client{
		token:   token,
		http:    &http.Client{Timeout: requestTimout},
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// APIError is a non-2xx response from GitHub.
type APIError struct {
	Status  int
	Message string
	// RateLimited is true when the failure was a rate/abuse limit.
	RateLimited bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %d %s", e.Status, e.Message)
}

// IsNotFound reports whether the resource is missing (or invisible to the token).
func IsNotFound(err error) bool {
	ae, ok := errors.AsType[*APIError](err)
	return ok && ae.Status == http.StatusNotFound
}

// IsConflict reports a stale-SHA write conflict.
func IsConflict(err error) bool {
	ae, ok := errors.AsType[*APIError](err)
	return ok && (ae.Status == http.StatusConflict || ae.Status == http.StatusUnprocessableEntity)
}

// Status maps a client error onto a sensible HTTP status for our own API.
func Status(err error) (int, string) {
	ae, ok := errors.AsType[*APIError](err)
	if !ok {
		return http.StatusBadGateway, "GitHub request failed"
	}
	switch {
	case ae.Status == http.StatusUnauthorized:
		return http.StatusUnauthorized, "your GitHub authorization expired, please sign in again"
	case ae.RateLimited:
		return http.StatusTooManyRequests, "GitHub rate limit reached, please wait a moment"
	case ae.Status == http.StatusNotFound:
		return http.StatusNotFound, "not found on GitHub, or your token cannot see it"
	case ae.Status == http.StatusForbidden:
		return http.StatusForbidden, "GitHub refused the request: " + ae.Message
	case ae.Status == http.StatusConflict, ae.Status == http.StatusUnprocessableEntity:
		return http.StatusConflict, "the file changed on GitHub since you loaded it: " + ae.Message
	default:
		return http.StatusBadGateway, "GitHub error: " + ae.Message
	}
}

// do performs a request against the API, decoding JSON into out when non-nil.
// It returns the response headers so callers can read pagination links.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Header, parseAPIError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.Header, fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.Header, nil
}

func parseAPIError(resp *http.Response) error {
	// Cap the error body; GitHub errors are small but the client shouldn't trust that.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var payload struct {
		Message string `json:"message"`
		Errors  []struct {
			Resource string `json:"resource"`
			Field    string `json:"field"`
			Code     string `json:"code"`
			Message  string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &payload)

	msg := payload.Message
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	for _, e := range payload.Errors {
		detail := e.Message
		if detail == "" {
			detail = strings.TrimSpace(e.Field + " " + e.Code)
		}
		if detail != "" {
			msg += " (" + detail + ")"
		}
	}

	rateLimited := resp.StatusCode == http.StatusForbidden &&
		(resp.Header.Get("X-RateLimit-Remaining") == "0" || strings.Contains(strings.ToLower(msg), "rate limit"))
	if resp.StatusCode == http.StatusTooManyRequests {
		rateLimited = true
	}

	return &APIError{Status: resp.StatusCode, Message: msg, RateLimited: rateLimited}
}

// User is the authenticated GitHub account.
type User struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// CurrentUser returns the account the token belongs to.
func (c *Client) CurrentUser(ctx context.Context) (*User, error) {
	var u User
	if _, err := c.do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// Repo is a repository the user can reach.
type Repo struct {
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	CanPush       bool   `json:"can_push"`
	Archived      bool   `json:"archived"`
	UpdatedAt     string `json:"updated_at"`
	Description   string `json:"description"`
}

type rawRepo struct {
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	UpdatedAt     string `json:"updated_at"`
	Description   string `json:"description"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
	Permissions struct {
		Push  bool `json:"push"`
		Admin bool `json:"admin"`
	} `json:"permissions"`
}

func (r rawRepo) toRepo() Repo {
	return Repo{
		FullName:      r.FullName,
		Name:          r.Name,
		Owner:         r.Owner.Login,
		Private:       r.Private,
		Archived:      r.Archived,
		DefaultBranch: r.DefaultBranch,
		CanPush:       r.Permissions.Push || r.Permissions.Admin,
		UpdatedAt:     r.UpdatedAt,
		Description:   r.Description,
	}
}

// ListRepos returns one page of repositories, including private ones the token
// can see. hasMore reports whether another page exists.
func (c *Client) ListRepos(ctx context.Context, page, perPage int) (repos []Repo, hasMore bool, err error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > 100 {
		perPage = 100
	}
	q := url.Values{}
	q.Set("per_page", strconv.Itoa(perPage))
	q.Set("page", strconv.Itoa(page))
	q.Set("sort", "updated")
	q.Set("affiliation", "owner,collaborator,organization_member")

	var raw []rawRepo
	header, err := c.do(ctx, http.MethodGet, "/user/repos?"+q.Encode(), nil, &raw)
	if err != nil {
		return nil, false, err
	}
	repos = make([]Repo, 0, len(raw))
	for _, r := range raw {
		repos = append(repos, r.toRepo())
	}
	return repos, strings.Contains(header.Get("Link"), `rel="next"`), nil
}

// GetRepo looks up a single repository.
func (c *Client) GetRepo(ctx context.Context, owner, repo string) (*Repo, error) {
	var raw rawRepo
	if _, err := c.do(ctx, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(repo), nil, &raw); err != nil {
		return nil, err
	}
	out := raw.toRepo()
	return &out, nil
}

// Branch is a named ref in a repository.
type Branch struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
}

// ListBranches returns up to 100 branches.
func (c *Client) ListBranches(ctx context.Context, owner, repo string) ([]Branch, error) {
	var branches []Branch
	path := "/repos/" + esc(owner) + "/" + esc(repo) + "/branches?per_page=100"
	if _, err := c.do(ctx, http.MethodGet, path, nil, &branches); err != nil {
		return nil, err
	}
	return branches, nil
}

// TreeEntry is one blob in a repository tree.
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

type treeResponse struct {
	SHA       string      `json:"sha"`
	Tree      []TreeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// Tree is a recursive listing of a ref.
type Tree struct {
	SHA       string      `json:"sha"`
	Entries   []TreeEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
}

// GetTree recursively lists every blob reachable from ref.
func (c *Client) GetTree(ctx context.Context, owner, repo, ref string) (*Tree, error) {
	path := "/repos/" + esc(owner) + "/" + esc(repo) + "/git/trees/" + esc(ref) + "?recursive=1"
	var resp treeResponse
	if _, err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &Tree{SHA: resp.SHA, Entries: resp.Tree, Truncated: resp.Truncated}, nil
}

// File is a decoded blob together with the SHA needed to update it.
type File struct {
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Size     int64  `json:"size"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// maxFileBytes caps how large a file the editor will load or write.
const maxFileBytes = 2 << 20 // 2 MiB

// GetFile reads a file's contents at ref.
func (c *Client) GetFile(ctx context.Context, owner, repo, path, ref string) (*File, error) {
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	endpoint := "/repos/" + esc(owner) + "/" + esc(repo) + "/contents/" + escapePath(path)
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}

	var payload struct {
		Type     string `json:"type"`
		Path     string `json:"path"`
		SHA      string `json:"sha"`
		Size     int64  `json:"size"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, nil, &payload); err != nil {
		return nil, err
	}
	if payload.Type != "file" {
		return nil, &APIError{Status: http.StatusBadRequest, Message: "path is not a file"}
	}
	if payload.Size > maxFileBytes {
		return nil, &APIError{Status: http.StatusRequestEntityTooLarge, Message: "file is larger than 2 MiB"}
	}

	// Files over ~1 MB come back with an empty content field; fetch the blob instead.
	if payload.Encoding != "base64" || payload.Content == "" {
		blob, err := c.getBlob(ctx, owner, repo, payload.SHA)
		if err != nil {
			return nil, err
		}
		return &File{Path: payload.Path, SHA: payload.SHA, Size: payload.Size, Content: blob, Encoding: "utf-8"}, nil
	}

	decoded, err := decodeBase64(payload.Content)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", payload.Path, err)
	}
	return &File{Path: payload.Path, SHA: payload.SHA, Size: payload.Size, Content: string(decoded), Encoding: "utf-8"}, nil
}

// RawContent is a file fetched as raw bytes, with GitHub's cache validator.
type RawContent struct {
	Data []byte
	// ETag is GitHub's validator, suitable for returning to a browser.
	ETag string
	// NotModified is true when the caller's ifNoneMatch still holds, in which
	// case Data is empty.
	NotModified bool
}

// RawFile fetches a file's bytes in a single request.
//
// Asking the contents API for the raw media type returns the bytes directly,
// which avoids both the two-step metadata-then-blob dance and the 33% base64
// overhead. That matters a lot when a document embeds many images: each one used
// to cost two GitHub round trips and roughly 2.3x its own size in transfer.
//
// Passing the browser's If-None-Match through lets GitHub answer 304, which
// costs no bandwidth and does not count against the rate limit.
func (c *Client) RawFile(ctx context.Context, owner, repo, path, ref, ifNoneMatch string) (*RawContent, error) {
	endpoint := c.baseURL + "/repos/" + esc(owner) + "/" + esc(repo) + "/contents/" + escapePath(path)
	if ref != "" {
		endpoint += "?ref=" + url.QueryEscape(ref)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github.raw")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return &RawContent{ETag: resp.Header.Get("ETag"), NotModified: true}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, parseAPIError(resp)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, &APIError{Status: http.StatusRequestEntityTooLarge, Message: "file is larger than 2 MiB"}
	}
	return &RawContent{Data: data, ETag: resp.Header.Get("ETag")}, nil
}

func (c *Client) getBlob(ctx context.Context, owner, repo, sha string) (string, error) {
	var payload struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	path := "/repos/" + esc(owner) + "/" + esc(repo) + "/git/blobs/" + esc(sha)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return "", err
	}
	if payload.Encoding != "base64" {
		return payload.Content, nil
	}
	decoded, err := decodeBase64(payload.Content)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// decodeBase64 tolerates the newline-wrapped base64 the contents API returns.
func decodeBase64(s string) ([]byte, error) {
	cleaned := strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(s)
	return base64.StdEncoding.DecodeString(cleaned)
}

// Commit describes a write that landed on a branch.
type Commit struct {
	SHA     string `json:"sha"`
	Path    string `json:"path,omitempty"`
	FileSHA string `json:"file_sha,omitempty"`
	HTMLURL string `json:"html_url,omitempty"`
	Branch  string `json:"branch"`
}

// PutFile creates or updates a single file. sha must be the current blob SHA
// when updating, and empty when creating; GitHub rejects a stale value, which is
// what protects concurrent edits from silently overwriting each other.
func (c *Client) PutFile(ctx context.Context, owner, repo, path, content, message, branch, sha string) (*Commit, error) {
	if len(content) > maxFileBytes {
		return nil, &APIError{Status: http.StatusRequestEntityTooLarge, Message: "file is larger than 2 MiB"}
	}
	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	}
	if branch != "" {
		body["branch"] = branch
	}
	if sha != "" {
		body["sha"] = sha
	}

	var payload struct {
		Content struct {
			SHA  string `json:"sha"`
			Path string `json:"path"`
		} `json:"content"`
		Commit struct {
			SHA     string `json:"sha"`
			HTMLURL string `json:"html_url"`
		} `json:"commit"`
	}
	endpoint := "/repos/" + esc(owner) + "/" + esc(repo) + "/contents/" + escapePath(path)
	if _, err := c.do(ctx, http.MethodPut, endpoint, body, &payload); err != nil {
		return nil, err
	}
	return &Commit{
		SHA:     payload.Commit.SHA,
		Path:    payload.Content.Path,
		FileSHA: payload.Content.SHA,
		HTMLURL: payload.Commit.HTMLURL,
		Branch:  branch,
	}, nil
}

// DeleteFile removes a single file at its known SHA.
func (c *Client) DeleteFile(ctx context.Context, owner, repo, path, message, branch, sha string) (*Commit, error) {
	if sha == "" {
		return nil, &APIError{Status: http.StatusBadRequest, Message: "deleting requires the file SHA"}
	}
	body := map[string]any{"message": message, "sha": sha}
	if branch != "" {
		body["branch"] = branch
	}

	var payload struct {
		Commit struct {
			SHA     string `json:"sha"`
			HTMLURL string `json:"html_url"`
		} `json:"commit"`
	}
	endpoint := "/repos/" + esc(owner) + "/" + esc(repo) + "/contents/" + escapePath(path)
	if _, err := c.do(ctx, http.MethodDelete, endpoint, body, &payload); err != nil {
		return nil, err
	}
	return &Commit{SHA: payload.Commit.SHA, Path: path, HTMLURL: payload.Commit.HTMLURL, Branch: branch}, nil
}

// Change is one edit inside a multi-file commit.
//
// Exactly one of Content or SHA is meaningful: Content uploads new text, SHA
// reuses an existing blob (that is how a move works), and Delete removes a path.
type Change struct {
	Path    string
	Content *string
	SHA     string
	Delete  bool
}

// treeEntryRequest mirrors the create-tree payload. SHA is a pointer without
// omitempty because GitHub deletes a path only when "sha" is explicitly null.
type treeEntryRequest struct {
	Path    string  `json:"path"`
	Mode    string  `json:"mode"`
	Type    string  `json:"type"`
	SHA     *string `json:"sha"`
	Content *string `json:"content,omitempty"`
}

// CommitChanges applies several changes as one commit using the git data API.
// This is how renames, folder moves and multi-file saves stay atomic: one new
// tree, one commit, one fast-forward ref update.
func (c *Client) CommitChanges(ctx context.Context, owner, repo, branch, message string, changes []Change) (*Commit, error) {
	if len(changes) == 0 {
		return nil, &APIError{Status: http.StatusBadRequest, Message: "no changes to commit"}
	}
	prefix := "/repos/" + esc(owner) + "/" + esc(repo)

	// 1. Where does the branch point right now?
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := c.do(ctx, http.MethodGet, prefix+"/git/ref/heads/"+escapePath(branch), nil, &ref); err != nil {
		return nil, err
	}
	headSHA := ref.Object.SHA

	var head struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if _, err := c.do(ctx, http.MethodGet, prefix+"/git/commits/"+esc(headSHA), nil, &head); err != nil {
		return nil, err
	}

	// 2. Build the tree delta on top of the current tree.
	entries := make([]treeEntryRequest, 0, len(changes))
	for _, ch := range changes {
		entry := treeEntryRequest{Path: ch.Path, Mode: "100644", Type: "blob"}
		switch {
		case ch.Delete:
			entry.SHA = nil // explicit null removes the path
		case ch.Content != nil:
			if len(*ch.Content) > maxFileBytes {
				return nil, &APIError{Status: http.StatusRequestEntityTooLarge, Message: "file is larger than 2 MiB: " + ch.Path}
			}
			entry.Content = new(*ch.Content)
		case ch.SHA != "":
			entry.SHA = new(ch.SHA)
		default:
			return nil, &APIError{Status: http.StatusBadRequest, Message: "change for " + ch.Path + " has no content"}
		}
		entries = append(entries, entry)
	}

	var newTree struct {
		SHA string `json:"sha"`
	}
	treeBody := map[string]any{"base_tree": head.Tree.SHA, "tree": entries}
	if _, err := c.do(ctx, http.MethodPost, prefix+"/git/trees", treeBody, &newTree); err != nil {
		return nil, err
	}

	// 3. Commit the new tree with the old head as its parent.
	var commit struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
	}
	commitBody := map[string]any{"message": message, "tree": newTree.SHA, "parents": []string{headSHA}}
	if _, err := c.do(ctx, http.MethodPost, prefix+"/git/commits", commitBody, &commit); err != nil {
		return nil, err
	}

	// 4. Fast-forward the branch. force stays false so a concurrent push wins
	//    and the user is told to reload rather than losing someone else's work.
	refBody := map[string]any{"sha": commit.SHA, "force": false}
	if _, err := c.do(ctx, http.MethodPatch, prefix+"/git/refs/heads/"+escapePath(branch), refBody, nil); err != nil {
		return nil, err
	}
	return &Commit{SHA: commit.SHA, HTMLURL: commit.HTMLURL, Branch: branch}, nil
}

// esc escapes a single path segment.
func esc(s string) string { return url.PathEscape(s) }

// escapePath escapes a multi-segment path, keeping the separators intact.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
