package app

import "testing"

func TestCleanPathAccepts(t *testing.T) {
	cases := map[string]string{
		"readme.md":             "readme.md",
		"/readme.md":            "readme.md",
		"./readme.md":           "readme.md",
		"docs/readme.md":        "docs/readme.md",
		"docs\\readme.md":       "docs/readme.md",
		"  docs/readme.md  ":    "docs/readme.md",
		"a/b/c/d.md":            "a/b/c/d.md",
		"weird name (1).md":     "weird name (1).md",
		"Ünïcode/файл.md":       "Ünïcode/файл.md",
		"docs/./readme.md":      "docs/readme.md",
		"docs/sub/../readme.md": "docs/readme.md",
		// Duplicate separators are normalized away rather than rejected.
		"docs//readme.md": "docs/readme.md",
	}
	for input, want := range cases {
		got, err := cleanPath(input)
		if err != nil {
			t.Errorf("cleanPath(%q) returned error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("cleanPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCleanPathRejects(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"/",
		".",
		"..",
		"../secrets.md",
		"../../etc/passwd",
		"docs/../../escape.md",
		".git/config",
		"docs/.git/config",
		".GIT/config",
		"bad\x00name.md",
		"line\nbreak.md",
	}
	for _, input := range cases {
		if got, err := cleanPath(input); err == nil {
			t.Errorf("cleanPath(%q) = %q, want an error", input, got)
		}
	}
}

func TestCleanPathRejectsOverlongInput(t *testing.T) {
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := cleanPath(string(long) + ".md"); err == nil {
		t.Error("expected an over-long path to be rejected")
	}
}

func TestCleanRef(t *testing.T) {
	valid := []string{"main", "master", "feature/thing", "release-1.2.3", "v2", "a_b"}
	for _, ref := range valid {
		got, err := cleanRef(ref)
		if err != nil || got != ref {
			t.Errorf("cleanRef(%q) = %q, %v; want it accepted", ref, got, err)
		}
	}

	if got, err := cleanRef("  "); err != nil || got != "" {
		t.Errorf(`cleanRef("  ") = %q, %v; want "", nil`, got, err)
	}

	invalid := []string{"..", "a..b", "-leading", "with space", "semi;colon", "tilde~1", "caret^", "quote'", "back\\slash"}
	for _, ref := range invalid {
		if _, err := cleanRef(ref); err == nil {
			t.Errorf("cleanRef(%q) was accepted", ref)
		}
	}
}

func TestRequireRef(t *testing.T) {
	if _, err := requireRef(""); err == nil {
		t.Error("requireRef(\"\") should fail: a push needs a branch")
	}
	if got, err := requireRef("main"); err != nil || got != "main" {
		t.Errorf("requireRef(\"main\") = %q, %v", got, err)
	}
}

func TestIsMarkdown(t *testing.T) {
	yes := []string{"a.md", "a.MD", "a.markdown", "a.mdown", "a.mkd", "a.mdx", "docs/b.md", "a.text"}
	no := []string{"a.txt", "a.png", "a", "a.mdd", "README", "a.md.bak"}

	for _, p := range yes {
		if !isMarkdown(p) {
			t.Errorf("isMarkdown(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if isMarkdown(p) {
			t.Errorf("isMarkdown(%q) = true, want false", p)
		}
	}
}

func TestImageContentType(t *testing.T) {
	cases := map[string]string{
		"a.png":      "image/png",
		"a.PNG":      "image/png",
		"a.jpg":      "image/jpeg",
		"a.jpeg":     "image/jpeg",
		"a.svg":      "image/svg+xml",
		"dir/a.webp": "image/webp",
	}
	for input, want := range cases {
		got, ok := imageContentType(input)
		if !ok || got != want {
			t.Errorf("imageContentType(%q) = %q, %v; want %q", input, got, ok, want)
		}
	}

	// Anything that could carry active content must be refused, so the proxy
	// cannot be used to serve attacker HTML from our own origin.
	for _, input := range []string{"a.html", "a.htm", "a.js", "a.md", "a.json", "a", "a.pdf", "a.xml"} {
		if _, ok := imageContentType(input); ok {
			t.Errorf("imageContentType(%q) was allowed", input)
		}
	}
}

func TestResolveRelative(t *testing.T) {
	cases := []struct{ doc, target, want string }{
		{"docs/readme.md", "img/a.png", "docs/img/a.png"},
		{"docs/readme.md", "./img/a.png", "docs/img/a.png"},
		{"docs/readme.md", "../assets/a.png", "assets/a.png"},
		{"readme.md", "img/a.png", "img/a.png"},
		{"readme.md", "a.png", "a.png"},
		{"a/b/c.md", "../../top.png", "top.png"},
		// Escaping the repository root yields nothing.
		{"readme.md", "../outside.png", ""},
		{"docs/readme.md", "../../../outside.png", ""},
	}
	for _, tc := range cases {
		if got := resolveRelative(tc.doc, tc.target); got != tc.want {
			t.Errorf("resolveRelative(%q, %q) = %q, want %q", tc.doc, tc.target, got, tc.want)
		}
	}
}

func TestCommitMessage(t *testing.T) {
	if got := commitMessage("  ", "fallback"); got != "fallback" {
		t.Errorf("commitMessage(blank) = %q, want fallback", got)
	}
	if got := commitMessage("Fix typo", "fallback"); got != "Fix typo" {
		t.Errorf("commitMessage = %q", got)
	}
	long := make([]byte, 3000)
	for i := range long {
		long[i] = 'x'
	}
	if got := commitMessage(string(long), "fallback"); len(got) != 2000 {
		t.Errorf("long message length = %d, want 2000", len(got))
	}
}
