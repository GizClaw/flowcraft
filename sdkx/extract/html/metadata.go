package html

import (
	"encoding/json"
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Metadata contains extracted page metadata.
type Metadata struct {
	Title       string
	Description string
	SiteName    string
	Image       string
	URL         string
	Author      string
	Published   string
	Type        string // og:type or JSON-LD @type
}

// JsonLdContent holds extracted JSON-LD structured data.
type JsonLdContent struct {
	Title       string
	Description string
	Type        string
}

// ToMap converts Metadata to a map for ExtractedContent.Metadata.
func (m *Metadata) ToMap() map[string]string {
	if m == nil {
		return nil
	}
	result := make(map[string]string)
	if m.Image != "" {
		result["og:image"] = m.Image
	}
	if m.Author != "" {
		result["author"] = m.Author
	}
	if m.Published != "" {
		result["published"] = m.Published
	}
	if m.Type != "" {
		result["og:type"] = m.Type
	}
	if m.URL != "" {
		result["og:url"] = m.URL
	}
	return result
}

// IsPodcastLikeType checks if the metadata type looks like podcast content.
func IsPodcastLikeType(typ string) bool {
	if typ == "" {
		return false
	}
	lower := strings.ToLower(typ)
	if strings.Contains(lower, "podcast") {
		return true
	}
	switch lower {
	case "audioobject", "episode", "radioepisode", "musicrecording", "music.song":
		return true
	}
	return false
}

// ExtractMetadataWithURL extracts metadata and fills SiteName from hostname as fallback.
func ExtractMetadataWithURL(r io.Reader, pageURL string) (*Metadata, *JsonLdContent) {
	meta, jsonLd := extractMetadataFull(r)
	if meta.SiteName == "" {
		meta.SiteName = safeHostname(pageURL)
	}
	if jsonLd != nil {
		if jsonLd.Description != "" {
			meta.Description = jsonLd.Description
		}
		if jsonLd.Title != "" && meta.Title == "" {
			meta.Title = jsonLd.Title
		}
		if jsonLd.Type != "" && meta.Type == "" {
			meta.Type = jsonLd.Type
		}
	}
	return meta, jsonLd
}

// ExtractMetadata extracts OpenGraph, meta tags, and JSON-LD from HTML.
func ExtractMetadata(r io.Reader) (*Metadata, error) {
	meta, _ := extractMetadataFull(r)
	return meta, nil
}

func extractMetadataFull(r io.Reader) (*Metadata, *JsonLdContent) {
	doc, err := html.Parse(r)
	if err != nil {
		return &Metadata{}, nil
	}

	meta := &Metadata{}
	meta.Title = findTitle(doc)

	var jsonLDs []string
	extractMetaTags(doc, meta, &jsonLDs)

	var jsonLd *JsonLdContent
	if len(jsonLDs) > 0 {
		jsonLd = extractJsonLdContent(jsonLDs)
	}

	return meta, jsonLd
}

func safeHostname(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	host = strings.TrimPrefix(host, "www.")
	return host
}

func findTitle(n *html.Node) string {
	if n.Type == html.ElementNode && n.DataAtom == atom.Title {
		return getTextContent(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if t := findTitle(c); t != "" {
			return t
		}
	}
	return ""
}

func extractMetaTags(n *html.Node, meta *Metadata, jsonLDs *[]string) {
	if n.Type == html.ElementNode {
		switch n.DataAtom {
		case atom.Meta:
			name := getAttr(n, "name")
			property := getAttr(n, "property")
			content := getAttr(n, "content")

			switch {
			case property == "og:title" || name == "twitter:title":
				meta.Title = content
			case property == "og:description" || name == "twitter:description":
				meta.Description = content
			case property == "og:site_name":
				meta.SiteName = content
			case property == "og:image" || name == "twitter:image":
				meta.Image = content
			case property == "og:url":
				meta.URL = content
			case name == "author":
				meta.Author = content
			case name == "description" && meta.Description == "":
				meta.Description = content
			case name == "publishdate" || name == "date":
				meta.Published = content
			case property == "og:type":
				meta.Type = content
			case name == "application-name" && meta.SiteName == "":
				meta.SiteName = content
			}

		case atom.Script:
			if getAttr(n, "type") == "application/ld+json" {
				text := getTextContent(n)
				if text != "" {
					*jsonLDs = append(*jsonLDs, text)
				}
			}
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		extractMetaTags(c, meta, jsonLDs)
	}
}

// extractJsonLdContent recursively extracts the best candidate from JSON-LD blocks.
func extractJsonLdContent(jsonLDs []string) *JsonLdContent {
	var candidates []JsonLdContent

	for _, raw := range jsonLDs {
		var data interface{}
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			continue
		}
		collectJsonLdCandidates(data, &candidates)
	}

	if len(candidates) == 0 {
		return nil
	}

	// Sort by description length descending, pick best
	best := candidates[0]
	for _, c := range candidates[1:] {
		if len(c.Description) > len(best.Description) {
			best = c
		}
	}
	return &best
}

func collectJsonLdCandidates(input interface{}, out *[]JsonLdContent) {
	if input == nil {
		return
	}

	switch v := input.(type) {
	case []interface{}:
		for _, item := range v {
			collectJsonLdCandidates(item, out)
		}
	case map[string]interface{}:
		if graph, ok := v["@graph"]; ok {
			collectJsonLdCandidates(graph, out)
		}

		typ := extractJsonLdType(v)
		if typ != "" {
			title := firstString(v, "name", "headline", "title")
			desc := firstString(v, "description", "summary")
			if title != "" || desc != "" {
				*out = append(*out, JsonLdContent{
					Title:       title,
					Description: desc,
					Type:        typ,
				})
			}
		}
	}
}

func extractJsonLdType(record map[string]interface{}) string {
	raw, ok := record["@type"]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.ToLower(v)
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				return strings.ToLower(s)
			}
		}
	}
	return ""
}

func firstString(record map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := record[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func getTextContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sb.WriteString(getTextContent(c))
	}
	return strings.TrimSpace(sb.String())
}
