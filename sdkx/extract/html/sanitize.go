package html

import (
	"io"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// SanitizeConfig defines which sanitization rules to apply for StripHiddenHTML.
type SanitizeConfig struct {
	RemoveHiddenElements bool
	RemoveScripts        bool
	RemoveStyles         bool
}

// DefaultSanitizeConfig is the standard config for hidden element stripping.
var DefaultSanitizeConfig = SanitizeConfig{
	RemoveHiddenElements: true,
	RemoveScripts:        true,
	RemoveStyles:         true,
}

// nonTextTags are tags that never contain visible text.
var nonTextTags = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Noscript: true,
	atom.Template: true,
	atom.Svg:      true,
	atom.Canvas:   true,
	atom.Iframe:   true,
	atom.Object:   true,
	atom.Embed:    true,
}

// StripHiddenHTML removes hidden elements and non-text tags from HTML.
func StripHiddenHTML(r io.Reader, cfg SanitizeConfig) (string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", err
	}

	var toRemove []*html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if cfg.RemoveHiddenElements && isHiddenElement(n) {
				toRemove = append(toRemove, n)
				return
			}
			if cfg.RemoveScripts && n.DataAtom == atom.Script {
				toRemove = append(toRemove, n)
				return
			}
			if cfg.RemoveStyles && n.DataAtom == atom.Style {
				toRemove = append(toRemove, n)
				return
			}
			if nonTextTags[n.DataAtom] {
				toRemove = append(toRemove, n)
				return
			}
		}
		if n.Type == html.CommentNode {
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
	if err := html.Render(&sb, doc); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// isHiddenElement checks 14 ways an element can be hidden.
func isHiddenElement(n *html.Node) bool {
	for _, attr := range n.Attr {
		if attr.Key == "hidden" {
			return true
		}
		if attr.Key == "aria-hidden" && attr.Val == "true" {
			return true
		}
		if attr.Key == "type" && n.DataAtom == atom.Input && attr.Val == "hidden" {
			return true
		}
		if attr.Key == "style" {
			if matchesHiddenStyle(attr.Val) {
				return true
			}
		}
	}
	return false
}

var opacityZeroRe = regexp.MustCompile(`opacity\s*:\s*0(?:[;\s"}]|$)`)

func matchesHiddenStyle(style string) bool {
	s := strings.ToLower(strings.ReplaceAll(style, " ", ""))
	patterns := []string{
		"display:none",
		"visibility:hidden",
		"font-size:0",
		"clip-path:inset(100%)",
		"clip:rect(0,0,0,0)",
		"transform:scale(0)",
		"text-indent:-9999",
	}
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	if opacityZeroRe.MatchString(strings.ToLower(style)) {
		return true
	}
	if strings.Contains(s, "width:0") && strings.Contains(s, "height:0") && strings.Contains(s, "overflow:hidden") {
		return true
	}
	if (strings.Contains(s, "position:absolute") || strings.Contains(s, "position:fixed")) &&
		(strings.Contains(s, "left:-9999") || strings.Contains(s, "top:-9999")) {
		return true
	}
	return false
}

// getAttr gets an attribute value by name.
func getAttr(n *html.Node, name string) string {
	for _, attr := range n.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}
