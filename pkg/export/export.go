// Package export turns a Markdown document into downloadable files.
//
// Every format is derived from the same sanitized HTML the live preview shows,
// so an export never disagrees with what the user saw on screen.
package export

import (
	"bytes"
	"embed"
	"fmt"
	"html"
	"path"
	"regexp"
	"strings"
	"text/template"

	"github.com/art-pro/markdown-editor/pkg/markdown"
)

//go:embed assets/document.css
var assets embed.FS

// Format identifies an output type.
type Format string

// Supported export formats.
const (
	FormatMarkdown   Format = "markdown"
	FormatHTML       Format = "html"
	FormatStyledHTML Format = "styled-html"
	FormatText       Format = "text"
	FormatDOCX       Format = "docx"
)

// Descriptor describes a format for the UI.
type Descriptor struct {
	Format      Format `json:"format"`
	Label       string `json:"label"`
	Extension   string `json:"extension"`
	ContentType string `json:"content_type"`
	Description string `json:"description"`
}

// Formats lists everything the server can produce, in menu order. PDF is
// produced in the browser's print pipeline instead, so it is not listed here.
func Formats() []Descriptor {
	return []Descriptor{
		{FormatMarkdown, "Markdown", ".md", "text/markdown; charset=utf-8", "The source document, unchanged"},
		{FormatHTML, "HTML", ".html", "text/html; charset=utf-8", "Bare HTML fragment, no styling"},
		{FormatStyledHTML, "Styled HTML", ".html", "text/html; charset=utf-8", "Standalone page with embedded CSS"},
		{FormatText, "Plain text", ".txt", "text/plain; charset=utf-8", "Formatting stripped"},
		{FormatDOCX, "Word (.docx)", ".docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "Opens in Word, Pages or Google Docs"},
	}
}

// Lookup finds a format descriptor by name.
func Lookup(f Format) (Descriptor, bool) {
	for _, d := range Formats() {
		if d.Format == f {
			return d, true
		}
	}
	return Descriptor{}, false
}

// Result is a rendered file ready to be sent to the browser.
type Result struct {
	Filename    string
	ContentType string
	Body        []byte
}

// Options configures an export.
type Options struct {
	// Title becomes the document title and the download filename stem.
	Title string
}

// Run produces the requested format from Markdown source.
func Run(source []byte, format Format, opts Options) (*Result, error) {
	desc, ok := Lookup(format)
	if !ok {
		return nil, fmt.Errorf("unsupported export format %q", format)
	}

	title := cleanTitle(opts.Title)
	if title == "" {
		title = firstHeading(source)
	}
	if title == "" {
		title = "document"
	}
	filename := safeFilename(title) + desc.Extension

	var body []byte
	switch format {
	case FormatMarkdown:
		body = source

	case FormatHTML, FormatStyledHTML, FormatText, FormatDOCX:
		rendered, err := markdown.Render(source, markdown.Options{})
		if err != nil {
			return nil, err
		}
		switch format {
		case FormatHTML:
			body = []byte(rendered)
		case FormatStyledHTML:
			page, err := styledDocument(title, rendered)
			if err != nil {
				return nil, err
			}
			body = page
		case FormatText:
			body = []byte(htmlToText(rendered))
		case FormatDOCX:
			doc, err := buildDOCX(title, rendered)
			if err != nil {
				return nil, err
			}
			body = doc
		}
	}

	return &Result{Filename: filename, ContentType: desc.ContentType, Body: body}, nil
}

// DocumentCSS returns the shared stylesheet used by the preview pane and by the
// standalone HTML export, so the two can never drift apart.
func DocumentCSS() string {
	data, err := assets.ReadFile("assets/document.css")
	if err != nil {
		return ""
	}
	return string(data)
}

var styledTemplate = template.Must(template.New("doc").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="generator" content="markdown-editor">
<title>{{.Title}}</title>
<style>
{{.CSS}}
{{.HighlightCSS}}
body { margin: 0; background: #ffffff; }
.markdown-body { max-width: 52rem; margin: 0 auto; padding: 3rem 1.5rem 6rem; }
@media print {
  .markdown-body { max-width: none; padding: 0; }
}
</style>
</head>
<body>
<article class="markdown-body">
{{.Content}}
</article>
</body>
</html>
`))

// styledDocument wraps rendered HTML in a standalone, self-contained page.
// text/template is used because Content and CSS are already trusted: the HTML
// came out of the sanitizer and the CSS is our own embedded asset. Only Title is
// attacker-influenced, so it is escaped explicitly.
func styledDocument(title, content string) ([]byte, error) {
	var buf bytes.Buffer
	err := styledTemplate.Execute(&buf, struct {
		Title        string
		CSS          string
		HighlightCSS string
		Content      string
	}{
		Title:        html.EscapeString(title),
		CSS:          DocumentCSS(),
		HighlightCSS: markdown.HighlightCSS(),
		Content:      content,
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var headingPattern = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*#*\s*$`)

// firstHeading pulls a title out of the document's first ATX heading.
func firstHeading(source []byte) string {
	if m := headingPattern.FindSubmatch(source); m != nil {
		// Strip the most common inline markers from the captured text.
		title := strings.NewReplacer("**", "", "__", "", "*", "", "_", "", "`", "").Replace(string(m[1]))
		return cleanTitle(title)
	}
	return ""
}

func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".md")
	s = strings.TrimSuffix(s, ".markdown")
	// Titles come from filenames too; keep only the base name.
	s = path.Base(s)
	if s == "." || s == "/" {
		return ""
	}
	return strings.TrimSpace(s)
}

var unsafeFilenameChars = regexp.MustCompile(`[^\p{L}\p{N} ._-]+`)

// safeFilename reduces a title to something safe for a Content-Disposition header.
func safeFilename(s string) string {
	s = unsafeFilenameChars.ReplaceAllString(s, "-")
	s = strings.Trim(strings.Join(strings.Fields(s), " "), "-. ")
	if s == "" {
		return "document"
	}
	if len(s) > 80 {
		s = strings.TrimRight(s[:80], "-. ")
	}
	return s
}
