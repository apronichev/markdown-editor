package export

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	xhtml "golang.org/x/net/html"
)

// This file writes a minimal but valid WordprocessingML (.docx) package.
//
// A .docx is a ZIP holding a handful of XML parts. We emit the four that Word,
// Pages and Google Docs need: the content-type map, the package relationships,
// the document body, and a stylesheet defining the heading/quote/code styles the
// body refers to. Hyperlinks need one more part (document relationships), which
// is written only when the document actually contains links.

// docxRun is a stretch of text with inline formatting.
type docxRun struct {
	Text      string
	Bold      bool
	Italic    bool
	Strike    bool
	Code      bool
	LinkRelID string // non-empty when this run is part of a hyperlink
}

// docxParagraph is one block with a named Word style.
type docxParagraph struct {
	Style  string // Word style ID, e.g. "Heading1"
	Runs   []docxRun
	Indent int // list nesting level
	Bullet string
}

// docxBuilder accumulates paragraphs and hyperlink relationships.
type docxBuilder struct {
	paragraphs []docxParagraph
	links      []string // relationship ID N maps to links[N-1]
}

func (b *docxBuilder) addLink(target string) string {
	b.links = append(b.links, target)
	return "rId" + strconv.Itoa(len(b.links))
}

// buildDOCX converts sanitized HTML into a .docx package.
func buildDOCX(title, fragment string) ([]byte, error) {
	nodes, err := parseFragment(fragment)
	if err != nil {
		return nil, err
	}

	b := &docxBuilder{}
	for _, n := range nodes {
		b.walkBlock(n, inlineState{}, 0)
	}
	if len(b.paragraphs) == 0 {
		b.paragraphs = append(b.paragraphs, docxParagraph{Style: "Normal"})
	}

	return b.zip(title)
}

// inlineState carries the formatting inherited from enclosing inline elements.
type inlineState struct {
	bold, italic, strike, code bool
	linkRelID                  string
}

// blockStyle maps an HTML element to a Word style ID.
func blockStyle(tag string) (string, bool) {
	switch tag {
	case "h1":
		return "Heading1", true
	case "h2":
		return "Heading2", true
	case "h3":
		return "Heading3", true
	case "h4":
		return "Heading4", true
	case "h5":
		return "Heading5", true
	case "h6":
		return "Heading6", true
	case "p", "dd", "dt":
		return "Normal", true
	case "blockquote":
		return "Quote", true
	case "pre":
		return "CodeBlock", true
	case "li":
		return "ListParagraph", true
	}
	return "", false
}

// walkBlock descends the tree, emitting one paragraph per block element.
func (b *docxBuilder) walkBlock(n *xhtml.Node, inherited inlineState, depth int) {
	if n.Type == xhtml.TextNode {
		// Loose text between blocks (rare after rendering) becomes its own paragraph.
		if strings.TrimSpace(n.Data) != "" {
			b.paragraphs = append(b.paragraphs, docxParagraph{
				Style: "Normal",
				Runs:  []docxRun{{Text: n.Data}},
			})
		}
		return
	}
	if n.Type != xhtml.ElementNode {
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			b.walkBlock(child, inherited, depth)
		}
		return
	}

	switch n.Data {
	case "hr":
		b.paragraphs = append(b.paragraphs, docxParagraph{Style: "Normal", Runs: []docxRun{{Text: "———"}}})
		return
	case "table":
		b.walkTable(n)
		return
	case "ul", "ol", "dl":
		// Lists are containers; their <li> children become paragraphs.
		ordered := n.Data == "ol"
		index := 1
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == xhtml.ElementNode && (child.Data == "li" || child.Data == "dt" || child.Data == "dd") {
				bullet := "• "
				if ordered {
					bullet = strconv.Itoa(index) + ". "
					index++
				}
				b.walkListItem(child, inherited, depth, bullet)
				continue
			}
			b.walkBlock(child, inherited, depth)
		}
		return
	}

	style, isBlock := blockStyle(n.Data)
	if !isBlock {
		// An inline element at block level: gather it into a Normal paragraph.
		if hasBlockChild(n) {
			for child := n.FirstChild; child != nil; child = child.NextSibling {
				b.walkBlock(child, inherited, depth)
			}
			return
		}
		runs := collectRuns(b, n, inherited, n.Data == "pre")
		if len(runs) > 0 {
			b.paragraphs = append(b.paragraphs, docxParagraph{Style: "Normal", Runs: runs})
		}
		return
	}

	// A blockquote wrapping paragraphs should not swallow them into one run.
	if n.Data == "blockquote" && hasBlockChild(n) {
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			b.walkQuoted(child, inherited, depth)
		}
		return
	}

	preformatted := n.Data == "pre"
	runs := collectRuns(b, n, inherited, preformatted)
	if len(runs) == 0 && n.Data != "p" {
		return
	}
	b.paragraphs = append(b.paragraphs, docxParagraph{Style: style, Runs: runs, Indent: depth})
}

// walkQuoted emits child blocks of a blockquote using the Quote style.
func (b *docxBuilder) walkQuoted(n *xhtml.Node, inherited inlineState, depth int) {
	before := len(b.paragraphs)
	b.walkBlock(n, inherited, depth)
	for i := before; i < len(b.paragraphs); i++ {
		if b.paragraphs[i].Style == "Normal" {
			b.paragraphs[i].Style = "Quote"
		}
	}
}

// walkListItem emits a list item, keeping any nested list one level deeper.
func (b *docxBuilder) walkListItem(li *xhtml.Node, inherited inlineState, depth int, bullet string) {
	var inlineRuns []docxRun
	var nested []*xhtml.Node

	for child := li.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == xhtml.ElementNode && (child.Data == "ul" || child.Data == "ol") {
			nested = append(nested, child)
			continue
		}
		if child.Type == xhtml.ElementNode && child.Data == "input" {
			// GFM task list checkbox.
			if hasAttr(child, "checked") {
				inlineRuns = append(inlineRuns, docxRun{Text: "☑ "})
			} else {
				inlineRuns = append(inlineRuns, docxRun{Text: "☐ "})
			}
			continue
		}
		inlineRuns = append(inlineRuns, collectInline(b, child, inherited, false)...)
	}

	b.paragraphs = append(b.paragraphs, docxParagraph{
		Style:  "ListParagraph",
		Runs:   trimRuns(inlineRuns),
		Indent: depth,
		Bullet: bullet,
	})

	for _, sub := range nested {
		b.walkBlock(sub, inherited, depth+1)
	}
}

// docxTable is captured before serialization so column count is known up front.
type docxTable struct {
	rows [][]docxParagraph
}

func (b *docxBuilder) walkTable(table *xhtml.Node) {
	var t docxTable
	var walkRows func(*xhtml.Node)
	walkRows = func(n *xhtml.Node) {
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != xhtml.ElementNode {
				continue
			}
			switch child.Data {
			case "thead", "tbody", "tfoot":
				walkRows(child)
			case "tr":
				var row []docxParagraph
				for cell := child.FirstChild; cell != nil; cell = cell.NextSibling {
					if cell.Type != xhtml.ElementNode || (cell.Data != "td" && cell.Data != "th") {
						continue
					}
					style := "Normal"
					runs := collectRuns(b, cell, inlineState{bold: cell.Data == "th"}, false)
					row = append(row, docxParagraph{Style: style, Runs: runs})
				}
				if len(row) > 0 {
					t.rows = append(t.rows, row)
				}
			}
		}
	}
	walkRows(table)

	if len(t.rows) == 0 {
		return
	}
	// Tables are emitted as a sentinel paragraph carrying the serialized table,
	// keeping document order intact without a second pass.
	b.paragraphs = append(b.paragraphs, docxParagraph{Style: tableSentinel, Runs: nil, Bullet: renderTableXML(t)})
}

// tableSentinel marks a paragraph slot that actually holds pre-rendered table XML.
const tableSentinel = "\x00table"

// collectRuns flattens the inline content of n's children into formatted runs.
func collectRuns(b *docxBuilder, n *xhtml.Node, state inlineState, preformatted bool) []docxRun {
	var runs []docxRun
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		runs = append(runs, collectInline(b, child, state, preformatted)...)
	}
	return trimRuns(runs)
}

// collectInline flattens a node *including itself*, which is what callers that
// walk a list item's children one at a time need: the item's text is often a
// direct text child of <li>, and descending straight to grandchildren loses it.
func collectInline(b *docxBuilder, n *xhtml.Node, state inlineState, preformatted bool) []docxRun {
	var runs []docxRun

	var visit func(*xhtml.Node, inlineState)
	visit = func(node *xhtml.Node, st inlineState) {
		switch node.Type {
		case xhtml.TextNode:
			text := node.Data
			if !preformatted {
				text = normalizeSpace(text)
			}
			if text == "" {
				return
			}
			runs = append(runs, docxRun{
				Text: text, Bold: st.bold, Italic: st.italic,
				Strike: st.strike, Code: st.code, LinkRelID: st.linkRelID,
			})
			return
		case xhtml.ElementNode:
			next := st
			switch node.Data {
			case "strong", "b":
				next.bold = true
			case "em", "i":
				next.italic = true
			case "del", "s", "strike":
				next.strike = true
			case "code", "kbd", "samp", "tt":
				next.code = true
			case "br":
				runs = append(runs, docxRun{Text: "\n"})
				return
			case "img":
				if alt := attrValue(node, "alt"); alt != "" {
					runs = append(runs, docxRun{Text: "[image: " + alt + "]", Italic: true})
				}
				return
			case "input":
				return
			case "a":
				if href := attrValue(node, "href"); href != "" && isExternalTarget(href) {
					next.linkRelID = b.addLink(href)
				}
			}
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				visit(child, next)
			}
		default:
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				visit(child, st)
			}
		}
	}

	visit(n, state)
	return runs
}

// trimRuns removes whitespace-only runs at the edges of a paragraph, trims the
// remaining edge whitespace inside the first and last run, and collapses spaces
// that doubled up where two runs met.
func trimRuns(runs []docxRun) []docxRun {
	for len(runs) > 0 && strings.TrimSpace(runs[0].Text) == "" && runs[0].Text != "\n" {
		runs = runs[1:]
	}
	for len(runs) > 0 && strings.TrimSpace(runs[len(runs)-1].Text) == "" && runs[len(runs)-1].Text != "\n" {
		runs = runs[:len(runs)-1]
	}
	if len(runs) == 0 {
		return runs
	}

	runs[0].Text = strings.TrimLeft(runs[0].Text, " \t")
	last := len(runs) - 1
	if runs[last].Text != "\n" {
		runs[last].Text = strings.TrimRight(runs[last].Text, " \t")
	}

	// A hard break or a marker such as "☑ " already supplies the separation, so
	// a leading space on the next run would render as a double space.
	out := runs[:0]
	previousEndsWithSpace := false
	for _, run := range runs {
		if previousEndsWithSpace {
			run.Text = strings.TrimLeft(run.Text, " ")
		}
		if run.Text == "" {
			continue
		}
		previousEndsWithSpace = run.Text == "\n" || strings.HasSuffix(run.Text, " ")
		out = append(out, run)
	}
	return out
}

// normalizeSpace collapses runs of HTML whitespace to a single space the way a
// browser would. Leading and trailing spaces are kept: runs are concatenated
// across sibling nodes, so dropping them would glue "</strong> text" together.
// Paragraph edges are tidied later by trimRuns.
func normalizeSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			space = true
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	if space {
		b.WriteByte(' ')
	}
	return b.String()
}

func isExternalTarget(href string) bool {
	lower := strings.ToLower(href)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "mailto:")
}

func hasBlockChild(n *xhtml.Node) bool {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != xhtml.ElementNode {
			continue
		}
		if _, ok := blockStyle(child.Data); ok {
			return true
		}
		switch child.Data {
		case "ul", "ol", "table", "dl", "hr", "blockquote":
			return true
		}
	}
	return false
}

func attrValue(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *xhtml.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// --- serialization ---------------------------------------------------------

func esc(s string) string {
	var buf bytes.Buffer
	// xml.EscapeText also escapes the characters Word is picky about.
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// renderRun writes one <w:r>, splitting on newlines so hard breaks survive.
func renderRun(b *strings.Builder, run docxRun) {
	props := &strings.Builder{}
	if run.Bold {
		props.WriteString("<w:b/>")
	}
	if run.Italic {
		props.WriteString("<w:i/>")
	}
	if run.Strike {
		props.WriteString("<w:strike/>")
	}
	if run.Code {
		props.WriteString(`<w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:cs="Consolas"/><w:shd w:val="clear" w:fill="F3F4F6"/>`)
	}
	if run.LinkRelID != "" {
		props.WriteString(`<w:color w:val="0B7285"/><w:u w:val="single"/>`)
	}

	if run.LinkRelID != "" {
		fmt.Fprintf(b, `<w:hyperlink r:id="%s">`, run.LinkRelID)
	}
	for i, line := range strings.Split(run.Text, "\n") {
		if i > 0 {
			b.WriteString(`<w:r><w:br/></w:r>`)
		}
		if line == "" {
			continue
		}
		b.WriteString("<w:r>")
		if props.Len() > 0 {
			b.WriteString("<w:rPr>" + props.String() + "</w:rPr>")
		}
		// xml:space="preserve" keeps the leading/trailing spaces between runs.
		b.WriteString(`<w:t xml:space="preserve">` + esc(line) + `</w:t>`)
		b.WriteString("</w:r>")
	}
	if run.LinkRelID != "" {
		b.WriteString("</w:hyperlink>")
	}
}

func renderParagraph(b *strings.Builder, p docxParagraph) {
	if p.Style == tableSentinel {
		b.WriteString(p.Bullet) // holds pre-rendered table XML
		return
	}

	b.WriteString("<w:p><w:pPr>")
	fmt.Fprintf(b, `<w:pStyle w:val="%s"/>`, esc(p.Style))
	if p.Indent > 0 || p.Style == "ListParagraph" {
		// 360 twentieths of a point ≈ 0.25", the usual Word list step.
		fmt.Fprintf(b, `<w:ind w:left="%d" w:hanging="360"/>`, 720+p.Indent*360)
	}
	b.WriteString("</w:pPr>")

	if p.Bullet != "" {
		renderRun(b, docxRun{Text: p.Bullet})
	}
	for _, run := range p.Runs {
		renderRun(b, run)
	}
	b.WriteString("</w:p>")
}

func renderTableXML(t docxTable) string {
	cols := 0
	for _, row := range t.rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	if cols == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(`<w:tbl><w:tblPr><w:tblStyle w:val="TableGrid"/><w:tblW w:w="0" w:type="auto"/>`)
	b.WriteString(`<w:tblBorders>`)
	for _, side := range []string{"top", "left", "bottom", "right", "insideH", "insideV"} {
		fmt.Fprintf(&b, `<w:%s w:val="single" w:sz="4" w:space="0" w:color="D0D7DE"/>`, side)
	}
	b.WriteString(`</w:tblBorders></w:tblPr><w:tblGrid>`)
	width := 9360 / cols
	for range cols {
		fmt.Fprintf(&b, `<w:gridCol w:w="%d"/>`, width)
	}
	b.WriteString(`</w:tblGrid>`)

	for _, row := range t.rows {
		b.WriteString("<w:tr>")
		for i := range cols {
			b.WriteString(`<w:tc><w:tcPr>`)
			fmt.Fprintf(&b, `<w:tcW w:w="%d" w:type="dxa"/>`, width)
			b.WriteString(`</w:tcPr>`)
			if i < len(row) {
				renderParagraph(&b, row[i])
			} else {
				b.WriteString(`<w:p><w:pPr><w:pStyle w:val="Normal"/></w:pPr></w:p>`)
			}
			b.WriteString("</w:tc>")
		}
		b.WriteString("</w:tr>")
	}
	b.WriteString("</w:tbl>")
	// Word requires a paragraph after a table.
	b.WriteString(`<w:p><w:pPr><w:pStyle w:val="Normal"/></w:pPr></w:p>`)
	return b.String()
}

func (b *docxBuilder) documentXML() string {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	body.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><w:body>`)
	for _, p := range b.paragraphs {
		renderParagraph(&body, p)
	}
	body.WriteString(`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/>` +
		`<w:pgMar w:top="1134" w:right="1134" w:bottom="1134" w:left="1134" w:header="709" w:footer="709" w:gutter="0"/>` +
		`</w:sectPr></w:body></w:document>`)
	return body.String()
}

func (b *docxBuilder) relsXML() string {
	var s strings.Builder
	s.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	s.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	s.WriteString(`<Relationship Id="rIdStyles" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`)
	for i, target := range b.links {
		fmt.Fprintf(&s, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="%s" TargetMode="External"/>`,
			i+1, esc(target))
	}
	s.WriteString(`</Relationships>`)
	return s.String()
}

const contentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>
</Types>`

const packageRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`

// stylesXML defines the handful of styles the body references.
const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:docDefaults><w:rPrDefault><w:rPr>
<w:rFonts w:ascii="Calibri" w:hAnsi="Calibri" w:cs="Calibri"/><w:sz w:val="22"/><w:szCs w:val="22"/>
</w:rPr></w:rPrDefault><w:pPrDefault><w:pPr><w:spacing w:after="160" w:line="276" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults>
<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>
<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/>
<w:pPr><w:keepNext/><w:spacing w:before="360" w:after="160"/><w:outlineLvl w:val="0"/></w:pPr>
<w:rPr><w:b/><w:sz w:val="44"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Normal"/>
<w:pPr><w:keepNext/><w:spacing w:before="320" w:after="140"/><w:outlineLvl w:val="1"/></w:pPr>
<w:rPr><w:b/><w:sz w:val="34"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading3"><w:name w:val="heading 3"/><w:basedOn w:val="Normal"/>
<w:pPr><w:keepNext/><w:spacing w:before="280" w:after="120"/><w:outlineLvl w:val="2"/></w:pPr>
<w:rPr><w:b/><w:sz w:val="28"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading4"><w:name w:val="heading 4"/><w:basedOn w:val="Normal"/>
<w:pPr><w:keepNext/><w:outlineLvl w:val="3"/></w:pPr><w:rPr><w:b/><w:sz w:val="24"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading5"><w:name w:val="heading 5"/><w:basedOn w:val="Normal"/>
<w:pPr><w:keepNext/><w:outlineLvl w:val="4"/></w:pPr><w:rPr><w:b/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading6"><w:name w:val="heading 6"/><w:basedOn w:val="Normal"/>
<w:pPr><w:keepNext/><w:outlineLvl w:val="5"/></w:pPr><w:rPr><w:b/><w:i/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Quote"><w:name w:val="Quote"/><w:basedOn w:val="Normal"/>
<w:pPr><w:ind w:left="720"/><w:pBdr><w:left w:val="single" w:sz="18" w:space="12" w:color="D0D7DE"/></w:pBdr></w:pPr>
<w:rPr><w:i/><w:color w:val="57606A"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="CodeBlock"><w:name w:val="Code Block"/><w:basedOn w:val="Normal"/>
<w:pPr><w:spacing w:before="120" w:after="120" w:line="240" w:lineRule="auto"/><w:ind w:left="240"/>
<w:shd w:val="clear" w:fill="F6F8FA"/></w:pPr>
<w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:cs="Consolas"/><w:sz w:val="19"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="ListParagraph"><w:name w:val="List Paragraph"/><w:basedOn w:val="Normal"/>
<w:pPr><w:spacing w:after="80"/></w:pPr></w:style>
<w:style w:type="table" w:styleId="TableGrid"><w:name w:val="Table Grid"/></w:style>
</w:styles>`

// zip assembles the OOXML package.
func (b *docxBuilder) zip(title string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	parts := []struct{ name, content string }{
		{"[Content_Types].xml", contentTypesXML},
		{"_rels/.rels", packageRelsXML},
		{"word/document.xml", b.documentXML()},
		{"word/styles.xml", stylesXML},
		{"word/_rels/document.xml.rels", b.relsXML()},
	}
	for _, part := range parts {
		w, err := zw.Create(part.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(part.content)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	_ = title
	return buf.Bytes(), nil
}
