package app

import (
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/art-pro/markdown-editor/internal/httpx"
)

// ownerRepoPattern matches the character set GitHub allows in owner and repo names.
var ownerRepoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,99})$`)

// refPattern matches branch, tag and commit-ish names conservatively.
var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)

// repoTarget identifies a repository from the request path.
type repoTarget struct {
	Owner string
	Repo  string
}

// target reads and validates {owner} and {repo} from the route.
func target(r *http.Request) (repoTarget, error) {
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	if !ownerRepoPattern.MatchString(owner) || !ownerRepoPattern.MatchString(repo) {
		return repoTarget{}, httpx.Errorf(http.StatusBadRequest, "invalid repository owner or name")
	}
	return repoTarget{Owner: owner, Repo: repo}, nil
}

// cleanRef validates a git ref, returning "" when none was supplied.
func cleanRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	if strings.Contains(ref, "..") || !refPattern.MatchString(ref) {
		return "", httpx.Errorf(http.StatusBadRequest, "invalid git ref %q", ref)
	}
	return ref, nil
}

// requireRef validates a ref that must be present, such as a push target.
func requireRef(ref string) (string, error) {
	cleaned, err := cleanRef(ref)
	if err != nil {
		return "", err
	}
	if cleaned == "" {
		return "", httpx.Errorf(http.StatusBadRequest, "a branch is required")
	}
	return cleaned, nil
}

// cleanPath normalizes a repository-relative path and rejects anything that
// tries to escape the repository or write to git's own metadata.
func cleanPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")

	if p == "" {
		return "", httpx.Errorf(http.StatusBadRequest, "a file path is required")
	}
	if len(p) > 512 {
		return "", httpx.Errorf(http.StatusBadRequest, "path is too long")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", httpx.Errorf(http.StatusBadRequest, "path contains control characters")
		}
	}

	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == "/" {
		return "", httpx.Errorf(http.StatusBadRequest, "a file path is required")
	}
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.HasPrefix(cleaned, "/") {
		return "", httpx.Errorf(http.StatusBadRequest, "path escapes the repository")
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", httpx.Errorf(http.StatusBadRequest, "path contains an empty or relative segment")
		}
		if strings.EqualFold(segment, ".git") {
			return "", httpx.Errorf(http.StatusBadRequest, "paths inside .git are not editable")
		}
	}
	return cleaned, nil
}

// markdownExtensions are the files the browser tree shows by default.
var markdownExtensions = map[string]bool{
	".md": true, ".markdown": true, ".mdown": true, ".mkd": true, ".mdx": true, ".text": true,
}

// isMarkdown reports whether a path looks like a Markdown document.
func isMarkdown(p string) bool {
	return markdownExtensions[strings.ToLower(path.Ext(p))]
}

// imageContentTypes maps image extensions to the type we will serve them as.
// Restricting the proxy to images is deliberate: streaming arbitrary repository
// files from our own origin would turn the preview into an XSS vector.
var imageContentTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".bmp":  "image/bmp",
	".ico":  "image/x-icon",
	".svg":  "image/svg+xml",
}

// imageContentType returns the content type for a proxied asset, if allowed.
func imageContentType(p string) (string, bool) {
	ct, ok := imageContentTypes[strings.ToLower(path.Ext(p))]
	return ct, ok
}

// resolveRelative resolves a link relative to the directory holding docPath.
func resolveRelative(docPath, target string) string {
	dir := path.Dir(docPath)
	if dir == "." || dir == "/" {
		dir = ""
	}
	joined := path.Join(dir, target)
	if strings.HasPrefix(joined, "../") || joined == ".." {
		return ""
	}
	return strings.TrimPrefix(joined, "/")
}
