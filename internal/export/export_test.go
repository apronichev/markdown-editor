package export

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"strings"
	"testing"
)

const sample = `# Release Notes

Some **bold** and *italic* text, plus ` + "`inline code`" + ` and a [link](https://example.com).

## Lists

- first
- second
  - nested
1. one
2. two

- [x] shipped
- [ ] pending

> A quotation.

| Column | Value |
|--------|------:|
| a      |     1 |
| b      |     2 |

` + "```go\nfunc main() {}\n```" + `

---
`

func TestFormatsAreLookupable(t *testing.T) {
	for _, d := range Formats() {
		got, ok := Lookup(d.Format)
		if !ok || got.Extension != d.Extension {
			t.Errorf("Lookup(%q) = %+v, %v", d.Format, got, ok)
		}
	}
	if _, ok := Lookup(Format("nonsense")); ok {
		t.Error("Lookup accepted an unknown format")
	}
}

func TestRunMarkdownIsByteIdentical(t *testing.T) {
	result, err := Run([]byte(sample), FormatMarkdown, Options{Title: "notes.md"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(result.Body) != sample {
		t.Error("Markdown export changed the source")
	}
	if result.Filename != "notes.md" {
		t.Errorf("Filename = %q, want notes.md", result.Filename)
	}
}

func TestRunStyledHTMLIsSelfContained(t *testing.T) {
	result, err := Run([]byte(sample), FormatStyledHTML, Options{Title: "notes.md"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body := string(result.Body)

	for _, want := range []string{
		"<!DOCTYPE html>",
		"<title>notes</title>",
		".markdown-body",
		".chroma",
		"<h1 id=\"release-notes\">Release Notes</h1>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("styled HTML missing %q", want)
		}
	}
	if strings.Contains(body, "<link") {
		t.Error("styled HTML should not reference external stylesheets")
	}
	if result.Filename != "notes.html" {
		t.Errorf("Filename = %q, want notes.html", result.Filename)
	}
}

func TestRunStyledHTMLEscapesTitle(t *testing.T) {
	result, err := Run([]byte("hi"), FormatStyledHTML, Options{Title: `</title><script>alert(1)</script>`})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(string(result.Body), "<script>alert(1)</script>") {
		t.Errorf("title was not escaped:\n%s", result.Body)
	}
}

func TestRunPlainText(t *testing.T) {
	result, err := Run([]byte(sample), FormatText, Options{Title: "notes"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	text := string(result.Body)

	for _, want := range []string{"Release Notes", "- first", "A quotation.", "func main() {}"} {
		if !strings.Contains(text, want) {
			t.Errorf("plain text missing %q\ngot:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"<p>", "<strong>", "**bold**"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("plain text still contains %q", unwanted)
		}
	}
}

// TestRunDOCXIsValidPackage checks the shape a Word reader depends on: the
// required parts are present and every XML part actually parses.
func TestRunDOCXIsValidPackage(t *testing.T) {
	result, err := Run([]byte(sample), FormatDOCX, Options{Title: "notes.md"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Filename != "notes.docx" {
		t.Errorf("Filename = %q, want notes.docx", result.Filename)
	}

	reader, err := zip.NewReader(bytes.NewReader(result.Body), int64(len(result.Body)))
	if err != nil {
		t.Fatalf("not a zip archive: %v", err)
	}

	parts := map[string]string{}
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open %s: %v", file.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", file.Name, err)
		}
		parts[file.Name] = string(data)

		if strings.HasSuffix(file.Name, ".xml") || strings.HasSuffix(file.Name, ".rels") {
			if err := xml.Unmarshal(data, new(struct{ XMLName xml.Name })); err != nil {
				t.Errorf("%s is not well-formed XML: %v", file.Name, err)
			}
		}
	}

	for _, required := range []string{
		"[Content_Types].xml",
		"_rels/.rels",
		"word/document.xml",
		"word/styles.xml",
		"word/_rels/document.xml.rels",
	} {
		if _, ok := parts[required]; !ok {
			t.Errorf("package is missing %s", required)
		}
	}

	doc := parts["word/document.xml"]
	for _, want := range []string{
		`<w:pStyle w:val="Heading1"/>`,
		`<w:pStyle w:val="Heading2"/>`,
		`<w:pStyle w:val="Quote"/>`,
		`<w:pStyle w:val="CodeBlock"/>`,
		`<w:pStyle w:val="ListParagraph"/>`,
		"<w:b/>",  // bold run
		"<w:i/>",  // italic run
		"<w:tbl>", // the table
		"Release Notes",
		"☑", // checked task list item
		"☐", // unchecked task list item
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("document.xml missing %q", want)
		}
	}

	// The hyperlink in the sample must produce a matching external relationship.
	if !strings.Contains(doc, "<w:hyperlink r:id=\"rId1\">") {
		t.Errorf("document.xml has no hyperlink run:\n%s", doc)
	}
	rels := parts["word/_rels/document.xml.rels"]
	if !strings.Contains(rels, `Id="rId1"`) || !strings.Contains(rels, `Target="https://example.com"`) ||
		!strings.Contains(rels, `TargetMode="External"`) {
		t.Errorf("relationships missing the hyperlink target:\n%s", rels)
	}
}

func TestRunDOCXEscapesText(t *testing.T) {
	result, err := Run([]byte("A < B & C > D"), FormatDOCX, Options{Title: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(result.Body), int64(len(result.Body)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}
		rc, _ := file.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		if err := xml.Unmarshal(data, new(struct{ XMLName xml.Name })); err != nil {
			t.Fatalf("document.xml broke on special characters: %v", err)
		}
		if !strings.Contains(string(data), "&amp;") {
			t.Errorf("ampersand was not escaped:\n%s", data)
		}
	}
}

// docxRuns returns the concatenated text of each paragraph in a .docx, together
// with its indentation, so structure can be asserted without a Word install.
func docxParagraphs(t *testing.T, body []byte) []struct {
	Indent string
	Text   string
} {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	var doc string
	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		doc = string(data)
	}
	if doc == "" {
		t.Fatal("no word/document.xml")
	}

	var out []struct {
		Indent string
		Text   string
	}
	for _, para := range regexp.MustCompile(`(?s)<w:p>.*?</w:p>`).FindAllString(doc, -1) {
		indent := ""
		if m := regexp.MustCompile(`w:ind w:left="(\d+)"`).FindStringSubmatch(para); m != nil {
			indent = m[1]
		}
		var text strings.Builder
		for _, run := range regexp.MustCompile(`<w:t[^>]*>(.*?)</w:t>`).FindAllStringSubmatch(para, -1) {
			text.WriteString(run[1])
		}
		out = append(out, struct {
			Indent string
			Text   string
		}{indent, text.String()})
	}
	return out
}

// TestRunDOCXKeepsListTextAndSpacing guards two regressions: list item text used
// to be dropped entirely, and the space between adjacent runs (after </strong>,
// </code>, a hard break or a task-list checkbox) used to be lost or doubled.
func TestRunDOCXKeepsListTextAndSpacing(t *testing.T) {
	source := "Combine them for ***bold italic*** text. Use `inline code` for terms.\n\n" +
		"- first item\n- second item\n  - nested one\n\n" +
		"1. one\n2. two\n\n" +
		"- [x] shipped\n- [ ] pending\n"

	result, err := Run([]byte(source), FormatDOCX, Options{Title: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	paragraphs := docxParagraphs(t, result.Body)

	byText := map[string]string{} // text -> indent
	for _, p := range paragraphs {
		byText[p.Text] = p.Indent
	}

	for _, want := range []string{
		"Combine them for bold italic text. Use inline code for terms.",
		"• first item",
		"• second item",
		"• nested one",
		"1. one",
		"2. two",
		"• ☑ shipped",
		"• ☐ pending",
	} {
		if _, ok := byText[want]; !ok {
			t.Errorf("missing paragraph %q\nall paragraphs: %q", want, paragraphs)
		}
	}

	// A nested item must sit deeper than its parent.
	if byText["• nested one"] == byText["• second item"] {
		t.Errorf("nested item was not indented deeper: %q vs %q",
			byText["• nested one"], byText["• second item"])
	}

	// No paragraph should contain a doubled space.
	for _, p := range paragraphs {
		if strings.Contains(p.Text, "  ") {
			t.Errorf("paragraph has a doubled space: %q", p.Text)
		}
	}
}

func TestRunEmptyDocumentStillProducesFiles(t *testing.T) {
	for _, format := range []Format{FormatMarkdown, FormatHTML, FormatStyledHTML, FormatText, FormatDOCX} {
		result, err := Run([]byte(""), format, Options{})
		if err != nil {
			t.Fatalf("Run(%q) on empty input: %v", format, err)
		}
		// A bare Markdown or HTML-fragment export of an empty document is
		// legitimately empty; the wrapping formats always have a skeleton.
		if (format == FormatStyledHTML || format == FormatText || format == FormatDOCX) && len(result.Body) == 0 {
			t.Errorf("Run(%q) produced an empty file", format)
		}
		if !strings.HasPrefix(result.Filename, "document") {
			t.Errorf("Run(%q) filename = %q, want a document.* fallback", format, result.Filename)
		}
	}
}

func TestTitleFallsBackToFirstHeading(t *testing.T) {
	result, err := Run([]byte("## My **Great** Doc\n\ntext\n"), FormatHTML, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Filename != "My Great Doc.html" {
		t.Errorf("Filename = %q, want %q", result.Filename, "My Great Doc.html")
	}
}

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		"notes":            "notes",
		"notes.md":         "notes",
		"a/b/c.md":         "c",
		`../../etc/passwd`: "passwd",
		`bad"name*here?`:   "bad-name-here",
		"  spaced  out  ":  "spaced out",
		"":                 "document",
		"...":              "document",
		"Ünïcode ok":       "Ünïcode ok",
		// Control characters cannot reach the Content-Disposition header.
		"tab\tsep":           "tab-sep",
		"newline\ninjection": "newline-injection",
	}
	for input, want := range cases {
		if got := safeFilename(cleanTitle(input)); got != want {
			t.Errorf("safeFilename(cleanTitle(%q)) = %q, want %q", input, got, want)
		}
	}
}

func TestHTMLToTextCollapsesBlankLines(t *testing.T) {
	got := htmlToText("<p>one</p><p></p><p></p><p>two</p>")
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("expected at most one blank line, got %q", got)
	}
}
