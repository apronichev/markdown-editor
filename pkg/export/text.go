package export

import (
	"strings"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// blockElements end the current line when the walker leaves them.
var blockElements = map[string]bool{
	"p": true, "div": true, "section": true, "article": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"ul": true, "ol": true, "li": true, "pre": true, "blockquote": true,
	"table": true, "tr": true, "thead": true, "tbody": true, "hr": true,
	"dl": true, "dt": true, "dd": true,
}

// htmlToText flattens rendered HTML into readable plain text.
func htmlToText(fragment string) string {
	nodes, err := parseFragment(fragment)
	if err != nil {
		return fragment
	}

	var b strings.Builder
	for _, n := range nodes {
		writeText(&b, n)
	}
	return collapseBlankLines(b.String())
}

func writeText(b *strings.Builder, n *xhtml.Node) {
	switch n.Type {
	case xhtml.TextNode:
		b.WriteString(n.Data)
		return
	case xhtml.ElementNode:
		switch n.Data {
		case "br":
			b.WriteString("\n")
			return
		case "hr":
			b.WriteString("\n----------\n")
			return
		case "li":
			b.WriteString("- ")
		case "td", "th":
			// Separate cells so table rows stay readable.
			if n.PrevSibling != nil {
				b.WriteString("\t")
			}
		}
	}

	for child := n.FirstChild; child != nil; child = child.NextSibling {
		writeText(b, child)
	}

	if n.Type == xhtml.ElementNode && blockElements[n.Data] {
		b.WriteString("\n")
	}
}

func collapseBlankLines(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n")) + "\n"
}

// parseFragment parses an HTML fragment in a <div> context.
func parseFragment(fragment string) ([]*xhtml.Node, error) {
	return xhtml.ParseFragment(strings.NewReader(fragment), &xhtml.Node{
		Type:     xhtml.ElementNode,
		Data:     "div",
		DataAtom: atom.Div,
	})
}
