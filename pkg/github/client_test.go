package github

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stub records requests and replies from a route table, letting the client be
// exercised end to end without touching the real GitHub API.
type stub struct {
	t        *testing.T
	server   *httptest.Server
	requests []recorded
	routes   map[string]func(w http.ResponseWriter, r *http.Request)
}

type recorded struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
	Header http.Header
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}}

	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone()}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.Body)
		}
		s.requests = append(s.requests, rec)

		if handler, ok := s.routes[r.Method+" "+r.URL.Path]; ok {
			handler(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stub) on(method, path string, status int, body string) {
	s.routes[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// client returns a Client pointed at the stub instead of api.github.com.
func (s *stub) client() *Client {
	c := New("test-token")
	c.baseURL = s.server.URL
	return c
}

func (s *stub) find(method, path string) *recorded {
	for i := range s.requests {
		if s.requests[i].Method == method && s.requests[i].Path == path {
			return &s.requests[i]
		}
	}
	return nil
}

func TestCurrentUserSendsAuthHeaders(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodGet, "/user", http.StatusOK, `{"login":"octocat","name":"Mona","avatar_url":"https://x/y.png"}`)

	user, err := s.client().CurrentUser(t.Context())
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if user.Login != "octocat" || user.Name != "Mona" {
		t.Errorf("unexpected user: %+v", user)
	}

	req := s.find(http.MethodGet, "/user")
	if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept = %q", got)
	}
	if got := req.Header.Get("X-GitHub-Api-Version"); got != apiVersion {
		t.Errorf("X-GitHub-Api-Version = %q", got)
	}
}

func TestListReposMapsFieldsAndPagination(t *testing.T) {
	s := newStub(t)
	s.routes["GET /user/repos"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://api.github.com/user/repos?page=2>; rel="next"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"full_name":"octocat/notes","name":"notes","private":true,"default_branch":"main",
			 "owner":{"login":"octocat"},"permissions":{"push":true,"admin":false}},
			{"full_name":"org/wiki","name":"wiki","private":false,"default_branch":"trunk",
			 "owner":{"login":"org"},"permissions":{"push":false,"admin":false}}
		]`))
	}

	repos, hasMore, err := s.client().ListRepos(t.Context(), 1, 100)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if !hasMore {
		t.Error("hasMore should follow the Link rel=next header")
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos", len(repos))
	}
	if repos[0].Owner != "octocat" || !repos[0].Private || !repos[0].CanPush {
		t.Errorf("first repo mapped wrong: %+v", repos[0])
	}
	if repos[1].CanPush {
		t.Error("a repo without push permission must report CanPush=false")
	}
}

func TestGetFileDecodesBase64(t *testing.T) {
	content := "# Hello\n\nWorld\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	// GitHub wraps base64 at 60 characters, sent as escaped \n inside the JSON
	// string; the client must strip those before decoding.
	wrapped := strings.Join([]string{encoded[:8], encoded[8:]}, `\n`)

	s := newStub(t)
	s.on(http.MethodGet, "/repos/octocat/notes/contents/docs/readme.md", http.StatusOK,
		`{"type":"file","path":"docs/readme.md","sha":"abc123","size":15,"encoding":"base64","content":"`+wrapped+`"}`)

	file, err := s.client().GetFile(t.Context(), "octocat", "notes", "docs/readme.md", "main")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if file.Content != content {
		t.Errorf("Content = %q, want %q", file.Content, content)
	}
	if file.SHA != "abc123" {
		t.Errorf("SHA = %q", file.SHA)
	}
	if q := s.find(http.MethodGet, "/repos/octocat/notes/contents/docs/readme.md").Query; q != "ref=main" {
		t.Errorf("Query = %q, want ref=main", q)
	}
}

func TestGetFileRejectsDirectories(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodGet, "/repos/octocat/notes/contents/docs", http.StatusOK, `{"type":"dir","path":"docs"}`)

	if _, err := s.client().GetFile(t.Context(), "octocat", "notes", "docs", ""); err == nil {
		t.Error("expected an error when the path is a directory")
	}
}

func TestGetFileRejectsOversizedFiles(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodGet, "/repos/octocat/notes/contents/big.md", http.StatusOK,
		`{"type":"file","path":"big.md","sha":"s","size":9999999,"encoding":"base64","content":""}`)

	_, err := s.client().GetFile(t.Context(), "octocat", "notes", "big.md", "")
	if err == nil || !strings.Contains(err.Error(), "2 MiB") {
		t.Errorf("err = %v, want a size complaint", err)
	}
}

func TestPutFileSendsBase64AndSHA(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodPut, "/repos/octocat/notes/contents/docs/readme.md", http.StatusOK,
		`{"content":{"sha":"newsha","path":"docs/readme.md"},"commit":{"sha":"c1","html_url":"https://github.com/x"}}`)

	commit, err := s.client().PutFile(t.Context(), "octocat", "notes",
		"docs/readme.md", "new body", "Update readme", "main", "oldsha")
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if commit.FileSHA != "newsha" || commit.SHA != "c1" || commit.HTMLURL != "https://github.com/x" {
		t.Errorf("commit mapped wrong: %+v", commit)
	}

	body := s.find(http.MethodPut, "/repos/octocat/notes/contents/docs/readme.md").Body
	if body["message"] != "Update readme" || body["branch"] != "main" || body["sha"] != "oldsha" {
		t.Errorf("request body = %v", body)
	}
	decoded, err := base64.StdEncoding.DecodeString(body["content"].(string))
	if err != nil || string(decoded) != "new body" {
		t.Errorf("content = %v (decoded %q, err %v)", body["content"], decoded, err)
	}
}

func TestPutFileOmitsSHAWhenCreating(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodPut, "/repos/octocat/notes/contents/new.md", http.StatusCreated,
		`{"content":{"sha":"s","path":"new.md"},"commit":{"sha":"c"}}`)

	if _, err := s.client().PutFile(t.Context(), "octocat", "notes", "new.md", "x", "Create", "main", ""); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if _, present := s.find(http.MethodPut, "/repos/octocat/notes/contents/new.md").Body["sha"]; present {
		t.Error("creating a file must not send a sha field")
	}
}

func TestDeleteFileRequiresSHA(t *testing.T) {
	s := newStub(t)
	if _, err := s.client().DeleteFile(t.Context(), "o", "r", "a.md", "msg", "main", ""); err == nil {
		t.Error("DeleteFile should refuse to run without a SHA")
	}
}

// TestCommitChangesBuildsATreeCommit walks the whole four-step git data dance
// and checks the payloads, including the explicit null SHA that deletes a path.
func TestCommitChangesBuildsATreeCommit(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodGet, "/repos/octocat/notes/git/ref/heads/main", http.StatusOK, `{"object":{"sha":"head1"}}`)
	s.on(http.MethodGet, "/repos/octocat/notes/git/commits/head1", http.StatusOK, `{"tree":{"sha":"tree1"}}`)
	s.on(http.MethodPost, "/repos/octocat/notes/git/trees", http.StatusCreated, `{"sha":"tree2"}`)
	s.on(http.MethodPost, "/repos/octocat/notes/git/commits", http.StatusCreated,
		`{"sha":"commit2","html_url":"https://github.com/c"}`)
	s.on(http.MethodPatch, "/repos/octocat/notes/git/refs/heads/main", http.StatusOK, `{}`)

	newBody := "moved content"
	changes := []Change{
		{Path: "docs/new.md", SHA: "blob1"},         // a move: reuse the blob
		{Path: "docs/old.md", Delete: true},         // remove the source
		{Path: "docs/edited.md", Content: &newBody}, // upload fresh text
	}

	commit, err := s.client().CommitChanges(t.Context(), "octocat", "notes", "main", "Reorganize", changes)
	if err != nil {
		t.Fatalf("CommitChanges: %v", err)
	}
	if commit.SHA != "commit2" || commit.Branch != "main" {
		t.Errorf("commit = %+v", commit)
	}

	tree := s.find(http.MethodPost, "/repos/octocat/notes/git/trees").Body
	if tree["base_tree"] != "tree1" {
		t.Errorf("base_tree = %v, want tree1", tree["base_tree"])
	}
	entries, ok := tree["tree"].([]any)
	if !ok || len(entries) != 3 {
		t.Fatalf("tree entries = %v", tree["tree"])
	}

	byPath := map[string]map[string]any{}
	for _, entry := range entries {
		m := entry.(map[string]any)
		byPath[m["path"].(string)] = m
	}

	if got := byPath["docs/new.md"]["sha"]; got != "blob1" {
		t.Errorf("move entry sha = %v, want blob1", got)
	}
	// A deletion must serialize "sha": null, not omit the field.
	deletion, present := byPath["docs/old.md"]["sha"]
	if !present {
		t.Error(`deletion entry must include an explicit "sha" key`)
	}
	if deletion != nil {
		t.Errorf("deletion sha = %v, want null", deletion)
	}
	if got := byPath["docs/edited.md"]["content"]; got != newBody {
		t.Errorf("content entry = %v", got)
	}
	if _, present := byPath["docs/edited.md"]["sha"]; present && byPath["docs/edited.md"]["sha"] != nil {
		t.Errorf("a content entry should not carry a sha: %v", byPath["docs/edited.md"])
	}
	for path, entry := range byPath {
		if entry["mode"] != "100644" || entry["type"] != "blob" {
			t.Errorf("%s has mode/type %v/%v", path, entry["mode"], entry["type"])
		}
	}

	commitBody := s.find(http.MethodPost, "/repos/octocat/notes/git/commits").Body
	if commitBody["tree"] != "tree2" || commitBody["message"] != "Reorganize" {
		t.Errorf("commit body = %v", commitBody)
	}
	parents, _ := commitBody["parents"].([]any)
	if len(parents) != 1 || parents[0] != "head1" {
		t.Errorf("parents = %v, want [head1]", commitBody["parents"])
	}

	// The ref update must not be forced: a concurrent push should win.
	refBody := s.find(http.MethodPatch, "/repos/octocat/notes/git/refs/heads/main").Body
	if refBody["sha"] != "commit2" || refBody["force"] != false {
		t.Errorf("ref update = %v, want sha=commit2 force=false", refBody)
	}
}

func TestCommitChangesRejectsEmptyChangeSet(t *testing.T) {
	s := newStub(t)
	if _, err := s.client().CommitChanges(t.Context(), "o", "r", "main", "m", nil); err == nil {
		t.Error("expected an error for an empty change set")
	}
}

func TestCommitChangesRejectsChangeWithoutContent(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodGet, "/repos/o/r/git/ref/heads/main", http.StatusOK, `{"object":{"sha":"h"}}`)
	s.on(http.MethodGet, "/repos/o/r/git/commits/h", http.StatusOK, `{"tree":{"sha":"t"}}`)

	changes := []Change{{Path: "a.md"}} // neither Content, SHA nor Delete
	if _, err := s.client().CommitChanges(t.Context(), "o", "r", "main", "m", changes); err == nil {
		t.Error("expected an error for a change with no content")
	}
}

func TestGetTreeRequestsRecursiveListing(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodGet, "/repos/o/r/git/trees/main", http.StatusOK,
		`{"sha":"t","truncated":true,"tree":[
			{"path":"a.md","mode":"100644","type":"blob","sha":"s1","size":10},
			{"path":"docs","mode":"040000","type":"tree","sha":"s2"}
		]}`)

	tree, err := s.client().GetTree(t.Context(), "o", "r", "main")
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	if !tree.Truncated || len(tree.Entries) != 2 {
		t.Errorf("tree = %+v", tree)
	}
	if q := s.find(http.MethodGet, "/repos/o/r/git/trees/main").Query; q != "recursive=1" {
		t.Errorf("Query = %q, want recursive=1", q)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		headers    map[string]string
		wantStatus int
		wantIs     func(error) bool
	}{
		{"unauthorized", http.StatusUnauthorized, `{"message":"Bad credentials"}`, nil, http.StatusUnauthorized, nil},
		{"not found", http.StatusNotFound, `{"message":"Not Found"}`, nil, http.StatusNotFound, IsNotFound},
		{"conflict", http.StatusConflict, `{"message":"is at abc but expected def"}`, nil, http.StatusConflict, IsConflict},
		{"unprocessable", http.StatusUnprocessableEntity, `{"message":"Invalid request"}`, nil, http.StatusConflict, IsConflict},
		{
			"rate limited", http.StatusForbidden, `{"message":"API rate limit exceeded"}`,
			map[string]string{"X-RateLimit-Remaining": "0"}, http.StatusTooManyRequests, nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.routes["GET /user"] = func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}

			_, err := s.client().CurrentUser(t.Context())
			if err == nil {
				t.Fatal("expected an error")
			}
			gotStatus, msg := Status(err)
			if gotStatus != tc.wantStatus {
				t.Errorf("Status() = %d, want %d (msg %q)", gotStatus, tc.wantStatus, msg)
			}
			if tc.wantIs != nil && !tc.wantIs(err) {
				t.Errorf("classification helper rejected %v", err)
			}
			if msg == "" {
				t.Error("Status() returned an empty message")
			}
		})
	}
}

func TestErrorIncludesFieldDetails(t *testing.T) {
	s := newStub(t)
	s.on(http.MethodGet, "/user", http.StatusUnprocessableEntity,
		`{"message":"Validation Failed","errors":[{"resource":"File","field":"path","code":"invalid","message":"path is bad"}]}`)

	_, err := s.client().CurrentUser(t.Context())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "path is bad") {
		t.Errorf("error should surface field details, got %v", err)
	}
}

func TestEscapePath(t *testing.T) {
	cases := map[string]string{
		"docs/readme.md":      "docs/readme.md",
		"docs/my file.md":     "docs/my%20file.md",
		"docs/a+b.md":         "docs/a+b.md",
		"folder/#hash.md":     "folder/%23hash.md",
		"folder/q?uery.md":    "folder/q%3Fuery.md",
		"feature/branch-name": "feature/branch-name",
		"Ünïcode/файл.md":     "%C3%9Cn%C3%AFcode/%D1%84%D0%B0%D0%B9%D0%BB.md",
	}
	for input, want := range cases {
		if got := escapePath(input); got != want {
			t.Errorf("escapePath(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestRawFileUsesOneRequest guards the performance fix: fetching an image used
// to cost a metadata request plus a blob request, and transferred the bytes
// twice because the first response was base64.
func TestRawFileUsesOneRequest(t *testing.T) {
	s := newStub(t)
	s.routes["GET /repos/o/r/contents/img/shot.png"] = func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github.raw" {
			t.Errorf("Accept = %q, want the raw media type", got)
		}
		w.Header().Set("ETag", `W/"abc123"`)
		_, _ = w.Write([]byte("\x89PNGrawbytes"))
	}

	raw, err := s.client().RawFile(t.Context(), "o", "r", "img/shot.png", "main", "")
	if err != nil {
		t.Fatalf("RawFile: %v", err)
	}
	if string(raw.Data) != "\x89PNGrawbytes" {
		t.Errorf("Data = %q", raw.Data)
	}
	if raw.ETag != `W/"abc123"` {
		t.Errorf("ETag = %q", raw.ETag)
	}
	if raw.NotModified {
		t.Error("NotModified should be false on a 200")
	}
	if len(s.requests) != 1 {
		t.Errorf("made %d requests, want exactly 1", len(s.requests))
	}
	if q := s.requests[0].Query; q != "ref=main" {
		t.Errorf("Query = %q, want ref=main", q)
	}
}

func TestRawFilePassesConditionalRequestThrough(t *testing.T) {
	s := newStub(t)
	s.routes["GET /repos/o/r/contents/a.png"] = func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != `W/"etag"` {
			t.Errorf("If-None-Match = %q, want it forwarded", got)
		}
		w.Header().Set("ETag", `W/"etag"`)
		w.WriteHeader(http.StatusNotModified)
	}

	raw, err := s.client().RawFile(t.Context(), "o", "r", "a.png", "", `W/"etag"`)
	if err != nil {
		t.Fatalf("RawFile: %v", err)
	}
	if !raw.NotModified {
		t.Error("expected NotModified for a 304")
	}
	if len(raw.Data) != 0 {
		t.Errorf("a 304 must carry no body, got %d bytes", len(raw.Data))
	}
}

func TestRawFileRejectsOversized(t *testing.T) {
	s := newStub(t)
	s.routes["GET /repos/o/r/contents/big.png"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, (2<<20)+10))
	}
	if _, err := s.client().RawFile(t.Context(), "o", "r", "big.png", "", ""); err == nil {
		t.Error("expected an oversized file to be rejected")
	}
}
