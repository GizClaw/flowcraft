package html

import (
	"bytes"
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/PuerkitoBio/goquery"
	"github.com/microcosm-cc/bluemonday"
)

// Segment represents a content segment extracted from HTML.
type Segment struct {
	Tag     string
	Content string
	Level   int // Heading level (1-6 for h1-h6, 0 for paragraphs)
}

// segmentSanitizer is the bluemonday policy for segment extraction (no links).
var segmentSanitizer = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements(
		"article", "section", "div", "p",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"ol", "ul", "li", "blockquote", "pre", "code",
		"span", "strong", "em", "b", "i", "br",
	)
	return p
}()

// markdownSanitizer is the bluemonday policy for markdown conversion (keeps links).
var markdownSanitizer = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements(
		"article", "section", "div", "p",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"ol", "ul", "li", "blockquote", "pre", "code",
		"span", "strong", "em", "b", "i", "br",
		"a",
	)
	p.AllowAttrs("href").OnElements("a")
	return p
}()

// SanitizeForSegments sanitizes HTML using the Segments whitelist.
func SanitizeForSegments(data []byte) string {
	stripped, err := StripHiddenHTML(bytes.NewReader(data), DefaultSanitizeConfig)
	if err != nil {
		stripped = string(data)
	}
	return segmentSanitizer.Sanitize(stripped)
}

// SanitizeForMarkdown sanitizes HTML using the Markdown whitelist (keeps links).
func SanitizeForMarkdown(data []byte) string {
	stripped, err := StripHiddenHTML(bytes.NewReader(data), DefaultSanitizeConfig)
	if err != nil {
		stripped = string(data)
	}
	return markdownSanitizer.Sanitize(stripped)
}

// CollectSegmentsFromHTML collects semantic segments from sanitized HTML.
// minLength sets the minimum character threshold for paragraph-like tags;
// heading and list item thresholds are fixed per-tag-type.
func CollectSegmentsFromHTML(data []byte, minLength int) ([]Segment, error) {
	if minLength <= 0 {
		minLength = 30
	}
	sanitized := SanitizeForSegments(data)

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(sanitized))
	if err != nil {
		return nil, err
	}

	var segments []Segment
	seen := make(map[string]bool)

	doc.Find("h1,h2,h3,h4,h5,h6,li,p,blockquote,pre").Each(func(i int, s *goquery.Selection) {
		node := s.Get(0)
		tag := strings.ToLower(node.Data)

		text := normalizeSegmentText(s.Text())
		if text == "" {
			return
		}

		// Per-tag minimum length thresholds (aligned with summarize article.ts)
		if strings.HasPrefix(tag, "h") {
			if len(text) < 10 {
				return
			}
		} else if tag == "li" {
			if len(text) < 20 {
				return
			}
			text = "• " + text
		} else {
			if len(text) < minLength {
				return
			}
		}

		key := text
		if len(key) > 100 {
			key = key[:100]
		}
		if seen[key] {
			return
		}
		seen[key] = true

		level := 0
		if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
			level = int(tag[1] - '0')
		}

		segments = append(segments, Segment{
			Tag:     tag,
			Content: text,
			Level:   level,
		})
	})

	if len(segments) == 0 {
		fallback := strings.TrimSpace(doc.Find("body").Text())
		if fallback == "" {
			fallback = strings.TrimSpace(doc.Text())
		}
		if fallback != "" {
			segments = append(segments, Segment{Tag: "body", Content: fallback})
		}
	}

	return segments, nil
}

func normalizeSegmentText(raw string) string {
	text := strings.Join(strings.Fields(raw), " ")
	return DecodeHtmlEntities(text)
}

// ExtractArticleContent extracts article content from HTML bytes.
func ExtractArticleContent(data []byte, minLength int) (string, error) {
	segments, err := CollectSegmentsFromHTML(data, minLength)
	if err == nil && len(segments) > 0 {
		return JoinSegments(segments), nil
	}
	return ExtractPlainText(data)
}

// ExtractPlainText extracts plain text from the entire HTML document.
func ExtractPlainText(data []byte) (string, error) {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return "", err
	}

	var body *html.Node
	var findBody func(n *html.Node)
	findBody = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Body {
			body = n
			return
		}
		for c := n.FirstChild; c != nil && body == nil; c = c.NextSibling {
			findBody(c)
		}
	}
	findBody(doc)

	if body == nil {
		return string(data), nil
	}

	return getTextWithNewlines(body), nil
}

func getTextWithNewlines(n *html.Node) string {
	if n.Type == html.TextNode {
		return strings.TrimSpace(n.Data)
	}
	if n.Type != html.ElementNode {
		return ""
	}

	isBlock := isBlockElement(n.DataAtom)
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		text := getTextWithNewlines(c)
		if text != "" {
			if sb.Len() > 0 && isBlock {
				sb.WriteString("\n")
			}
			sb.WriteString(text)
		}
	}
	return sb.String()
}

// JoinSegments joins multiple segments into a single text.
func JoinSegments(segments []Segment) string {
	if len(segments) == 0 {
		return ""
	}
	var parts []string
	for _, seg := range segments {
		if seg.Content != "" {
			parts = append(parts, seg.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// NormalizeReader reads all bytes from r for use as []byte.
func NormalizeReader(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}
