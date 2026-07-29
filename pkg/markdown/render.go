// Package markdown renders Markdown to HTML and sanitizes the result.
//
// Rendering is deliberately server-side and shared by the live preview and every
// export, so what the user previews is exactly what they export. Because a
// Markdown file can come from any repository, the rendered HTML is always passed
// through a strict allow-list sanitizer before it reaches the browser.
package markdown

import (
	"bytes"
	"regexp"
	"strings"
	"sync"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// highlightStyle is the chroma theme used for both preview and exports.
const highlightStyle = "github"

// Options tunes a single render.
type Options struct {
	// ResolveAsset rewrites a relative image source (for example, into a
	// authenticated proxy URL for a private repository). Nil leaves them alone.
	ResolveAsset func(src string) string
}

var (
	engineOnce sync.Once
	engine     goldmark.Markdown

	policyOnce sync.Once
	policy     *bluemonday.Policy
)

func mdEngine() goldmark.Markdown {
	engineOnce.Do(func() {
		engine = goldmark.New(
			goldmark.WithExtensions(
				extension.GFM,      // tables, strikethrough, task lists, autolinks
				extension.Footnote, // [^1] style footnotes
				extension.DefinitionList,
				extension.Typographer, // smart quotes and dashes
				highlighting.NewHighlighting(
					highlighting.WithStyle(highlightStyle),
					// Emit classes instead of inline styles: inline styles would
					// need 'unsafe-inline' in the CSP and are stripped anyway.
					highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
				),
			),
			goldmark.WithParserOptions(
				parser.WithAutoHeadingID(), // stable anchors for in-page links
				parser.WithAttribute(),
			),
			goldmark.WithRendererOptions(
				// No WithHardWraps: GitHub renders a single newline inside a
				// paragraph as a space when it displays a file in a repository, and
				// the preview has to agree with what the file will look like there.
				// Explicit breaks (two trailing spaces or a backslash) still work.
				//
				// Raw HTML in the source is preserved here and filtered by the
				// sanitizer below, so benign inline HTML keeps working.
				html.WithUnsafe(),
			),
		)
	})
	return engine
}

// classPattern allows the short chroma token classes, goldmark's language hints
// and our own layout classes, while rejecting anything else.
var classPattern = regexp.MustCompile(`^(?:(?:chroma|line|cl|lnt|lntd|lntable|ln|hl|footnotes|footnote-ref|footnote-backref|task-list-item|contains-task-list|language-[A-Za-z0-9+#._-]{1,32}|[a-z]{1,3}[0-9]?)\s*)+$`)

// idPattern allows the slug-like anchors goldmark generates.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _:.\-]{0,127}$`)

func sanitizer() *bluemonday.Policy {
	policyOnce.Do(func() {
		p := bluemonday.UGCPolicy()

		headings := []string{"h1", "h2", "h3", "h4", "h5", "h6"}

		// Anchors and syntax-highlighting hooks.
		p.AllowAttrs("id").Matching(idPattern).OnElements(append(headings, "a", "li", "sup", "section", "div")...)
		p.AllowAttrs("class").Matching(classPattern).OnElements(
			"span", "code", "pre", "div", "ul", "ol", "li", "table", "td", "th", "p", "a", "section", "sup",
		)
		p.AllowAttrs("tabindex").Matching(regexp.MustCompile(`^-?[0-9]{1,3}$`)).OnElements("pre", "div")

		// GFM task lists render as disabled checkboxes.
		p.AllowElements("input")
		p.AllowAttrs("type").Matching(regexp.MustCompile(`^checkbox$`)).OnElements("input")
		p.AllowAttrs("checked", "disabled").OnElements("input")

		// Table alignment produced by the GFM extension.
		p.AllowAttrs("align").Matching(regexp.MustCompile(`^(?:left|right|center|justify)$`)).OnElements("th", "td", "p", "div")
		p.AllowAttrs("colspan", "rowspan").Matching(regexp.MustCompile(`^[0-9]{1,3}$`)).OnElements("th", "td")

		// Images: allow sizing hints, data: URIs (pasted images) and our proxy.
		p.AllowAttrs("width", "height").Matching(regexp.MustCompile(`^[0-9]{1,4}$`)).OnElements("img")
		p.AllowAttrs("loading").Matching(regexp.MustCompile(`^(?:lazy|eager)$`)).OnElements("img")
		p.AllowAttrs("decoding").Matching(regexp.MustCompile(`^(?:async|sync|auto)$`)).OnElements("img")
		p.AllowDataURIImages()

		// Harmless semantic markup people do write by hand in Markdown files.
		p.AllowElements("kbd", "samp", "var", "mark", "abbr", "dfn", "details", "summary", "figure", "figcaption")
		p.AllowAttrs("title").OnElements("abbr", "dfn", "a", "img")
		p.AllowAttrs("open").OnElements("details")

		p.AllowStandardURLs()
		p.RequireNoFollowOnLinks(true)
		p.AddTargetBlankToFullyQualifiedLinks(true)

		policy = p
	})
	return policy
}

// Render converts Markdown to sanitized HTML.
func Render(source []byte, opts Options) (string, error) {
	var buf bytes.Buffer
	if err := mdEngine().Convert(source, &buf); err != nil {
		return "", err
	}

	rendered := buf.Bytes()
	if opts.ResolveAsset != nil {
		var err error
		rendered, err = rewriteAssets(rendered, opts.ResolveAsset)
		if err != nil {
			return "", err
		}
	}
	return string(sanitizer().SanitizeBytes(rendered)), nil
}

// HighlightCSS returns the stylesheet matching the syntax highlighting classes.
// It is served as a static asset and inlined into styled exports.
func HighlightCSS() string {
	style := styles.Get(highlightStyle)
	if style == nil {
		style = styles.Fallback
	}
	formatter := chromahtml.New(chromahtml.WithClasses(true))
	var buf bytes.Buffer
	if err := formatter.WriteCSS(&buf, style); err != nil {
		return ""
	}
	return buf.String()
}

// rewriteAssets points relative <img> sources at the resolver so images stored
// next to a Markdown file in a private repository actually load in the preview.
func rewriteAssets(fragment []byte, resolve func(string) string) ([]byte, error) {
	nodes, err := xhtml.ParseFragment(bytes.NewReader(fragment), &xhtml.Node{
		Type:     xhtml.ElementNode,
		Data:     "div",
		DataAtom: atom.Div,
	})
	if err != nil {
		return nil, err
	}

	for _, n := range nodes {
		walk(n, func(node *xhtml.Node) {
			if node.Type != xhtml.ElementNode || node.Data != "img" {
				return
			}

			for i, attr := range node.Attr {
				if attr.Key != "src" || !isRelative(attr.Val) {
					continue
				}
				if resolved := resolve(attr.Val); resolved != "" {
					node.Attr[i].Val = resolved
				}
			}

			// Defer off-screen images. Every proxied image costs a request to our
			// own function, so a long post with many screenshots would otherwise
			// fire them all at once and starve the file and render endpoints.
			setAttrIfAbsent(node, "loading", "lazy")
			setAttrIfAbsent(node, "decoding", "async")
		})
	}

	var out bytes.Buffer
	for _, n := range nodes {
		if err := xhtml.Render(&out, n); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// setAttrIfAbsent adds an attribute only when the document does not set it,
// so an explicit loading="eager" in the source is respected.
func setAttrIfAbsent(node *xhtml.Node, key, value string) {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return
		}
	}
	node.Attr = append(node.Attr, xhtml.Attribute{Key: key, Val: value})
}

func walk(n *xhtml.Node, fn func(*xhtml.Node)) {
	fn(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		walk(child, fn)
	}
}

// isRelative reports whether src is a repository-relative path rather than an
// absolute URL, a protocol-relative URL, a data: URI or a fragment.
func isRelative(src string) bool {
	src = strings.TrimSpace(src)
	if src == "" || strings.HasPrefix(src, "#") || strings.HasPrefix(src, "/") || strings.HasPrefix(src, "//") {
		return false
	}
	if i := strings.IndexByte(src, ':'); i >= 0 {
		// Anything with a scheme before the first slash is absolute.
		if j := strings.IndexByte(src, '/'); j == -1 || i < j {
			return false
		}
	}
	return true
}
