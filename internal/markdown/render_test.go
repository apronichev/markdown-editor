package markdown

import (
	"strings"
	"testing"
)

func TestRenderGFMFeatures(t *testing.T) {
	source := `# Title

Some **bold**, *italic* and ~~struck~~ text with ` + "`code`" + `.

- [x] done
- [ ] todo

| a | b |
|---|--:|
| 1 | 2 |

` + "```go\nfunc main() {}\n```" + `
`
	html, err := Render([]byte(source), Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, want := range []string{
		`<h1 id="title">Title</h1>`,
		"<strong>bold</strong>",
		"<em>italic</em>",
		"<del>struck</del>",
		`type="checkbox"`,
		"<table>",
		`class="chroma"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML missing %q\ngot: %s", want, html)
		}
	}
}

func TestRenderSanitizesDangerousHTML(t *testing.T) {
	// A Markdown file in someone's repository is untrusted input: it must not be
	// able to run script or exfiltrate the session in the preview pane.
	cases := []string{
		`<script>alert(1)</script>`,
		`<img src=x onerror="alert(1)">`,
		`<a href="javascript:alert(1)">click</a>`,
		`<iframe src="https://evil.example"></iframe>`,
		`<div style="position:fixed;inset:0">overlay</div>`,
		`<form action="https://evil.example"><input name="x"></form>`,
		`[link](javascript:alert(1))`,
		`<svg><use href="data:image/svg+xml;base64,PHN2Zz48L3N2Zz4="/></svg>`,
	}

	for _, source := range cases {
		html, err := Render([]byte(source), Options{})
		if err != nil {
			t.Fatalf("Render(%q): %v", source, err)
		}
		lower := strings.ToLower(html)
		for _, forbidden := range []string{"<script", "onerror", "javascript:", "<iframe", "style=", "<form"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("Render(%q) kept %q\ngot: %s", source, forbidden, html)
			}
		}
	}
}

func TestRenderKeepsSafeInlineHTML(t *testing.T) {
	html, err := Render([]byte("Some <kbd>Ctrl</kbd> and <sub>sub</sub> text."), Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(html, "<kbd>Ctrl</kbd>") {
		t.Errorf("expected <kbd> to survive, got %s", html)
	}
}

func TestRenderResolvesRelativeImages(t *testing.T) {
	source := `![local](images/diagram.png)
![absolute](https://example.com/x.png)
![rooted](/static/y.png)
![data](data:image/gif;base64,R0lGODlhAQABAAAAACw=)
`
	html, err := Render([]byte(source), Options{
		ResolveAsset: func(src string) string { return "/proxy?path=" + src },
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if !strings.Contains(html, `src="/proxy?path=images/diagram.png"`) {
		t.Errorf("relative image was not rewritten\ngot: %s", html)
	}
	for _, untouched := range []string{
		`src="https://example.com/x.png"`,
		`src="/static/y.png"`,
		"data:image/gif;base64",
	} {
		if !strings.Contains(html, untouched) {
			t.Errorf("expected %q to be left alone\ngot: %s", untouched, html)
		}
	}
}

func TestIsRelative(t *testing.T) {
	cases := map[string]bool{
		"images/a.png":              true,
		"./a.png":                   true,
		"a.png":                     true,
		"sub/dir/a.png":             true,
		"https://example.com/a.png": false,
		"//example.com/a.png":       false,
		"/absolute.png":             false,
		"data:image/png;base64,AA":  false,
		"#anchor":                   false,
		"":                          false,
		"mailto:a@b.c":              false,
		// A colon after the first slash is part of the filename, not a scheme.
		"dir/weird:name.png": true,
	}
	for input, want := range cases {
		if got := isRelative(input); got != want {
			t.Errorf("isRelative(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestHighlightCSSIsGenerated(t *testing.T) {
	css := HighlightCSS()
	if !strings.Contains(css, ".chroma") {
		t.Errorf("expected chroma classes in generated CSS, got %d bytes", len(css))
	}
}
