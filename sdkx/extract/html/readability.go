package html

import (
	"bytes"
	"strings"

	"github.com/go-shiori/go-readability"

	nethtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ReadabilityResult contains the result of Readability extraction.
type ReadabilityResult struct {
	Title    string
	Text     string
	HTML     string
	Excerpt  string
	Byline   string
	SiteName string
	Length   int
}

// ExtractWithReadability extracts content using the Readability algorithm.
func ExtractWithReadability(data []byte) (*ReadabilityResult, error) {
	sanitized, err := StripHiddenHTML(bytes.NewReader(data), DefaultSanitizeConfig)
	if err != nil {
		sanitized = string(data)
	}

	sanitized = removeStyleTags(sanitized)

	article, err := readability.FromReader(strings.NewReader(sanitized), nil)
	if err != nil {
		return nil, err
	}

	return &ReadabilityResult{
		Title:    article.Title,
		Text:     article.TextContent,
		HTML:     article.Content,
		Excerpt:  article.Excerpt,
		Byline:   article.Byline,
		SiteName: article.SiteName,
		Length:   article.Length,
	}, nil
}

// removeStyleTags removes all <style>...</style> elements using the HTML tokenizer.
func removeStyleTags(htmlStr string) string {
	doc, err := nethtml.Parse(strings.NewReader(htmlStr))
	if err != nil {
		return htmlStr
	}

	var toRemove []*nethtml.Node
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode && n.DataAtom == atom.Style {
			toRemove = append(toRemove, n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	for _, node := range toRemove {
		if node.Parent != nil {
			node.Parent.RemoveChild(node)
		}
	}

	var sb strings.Builder
	if err := nethtml.Render(&sb, doc); err != nil {
		return htmlStr
	}
	return sb.String()
}
