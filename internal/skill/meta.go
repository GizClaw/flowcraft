// Package skill provides the Skill system: metadata parsing, indexing,
// execution, and meta-tools for LLM-driven discovery.
package skill

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillMeta holds metadata extracted from a SKILL.md frontmatter.
type SkillMeta struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description"`
	Tags        []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Entry       string   `json:"entry,omitempty" yaml:"entry,omitempty"`
	Homepage    string   `json:"homepage,omitempty" yaml:"homepage,omitempty"`
	PrimaryEnv  string   `json:"primary_env,omitempty" yaml:"primary_env,omitempty"`
	Dir         string   `json:"dir,omitempty"`
	ReadmePath  string   `json:"readme_path,omitempty"`
	Builtin     bool     `json:"builtin,omitempty"`

	Requires *SkillRequires `json:"requires,omitempty"`
	Gating   *SkillGating   `json:"gating,omitempty"`
}

// SkillRequires declares runtime dependencies parsed from SKILL.md frontmatter.
type SkillRequires struct {
	Bins    []string `json:"bins,omitempty"`
	AnyBins []string `json:"any_bins,omitempty"`
	Env     []string `json:"env,omitempty"`
	OS      []string `json:"os,omitempty"`
}

// SkillGating holds the evaluated availability result for a skill.
type SkillGating struct {
	Available      bool     `json:"available"`
	MissingBins    []string `json:"missing_bins,omitempty"`
	MissingAnyBins []string `json:"missing_any_bins,omitempty"`
	MissingEnv     []string `json:"missing_env,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}

// ParseSkillMeta parses SKILL.md frontmatter into SkillMeta.
// It uses a dual-parse strategy: standard YAML library first, with a
// line-based fallback. Results are merged so that both simple and
// complex (multi-line, nested) frontmatter formats are handled correctly.
func ParseSkillMeta(content string) (*SkillMeta, error) {
	block, err := extractFrontmatter(content)
	if err != nil {
		return nil, err
	}

	yamlParsed := parseYAML(block)
	lineParsed := parseLine(block)

	merged := mergeFields(yamlParsed, lineParsed)

	if merged["name"] == "" {
		return nil, fmt.Errorf("skill: SKILL.md missing 'name' field")
	}

	meta := &SkillMeta{
		Name:        merged["name"],
		Description: merged["description"],
		Entry:       merged["entry"],
		Homepage:    merged["homepage"],
		PrimaryEnv:  merged["primary_env"],
		Tags:        parseTags(merged["tags"]),
	}

	meta.Requires = parseRequires(block)
	if meta.PrimaryEnv == "" {
		meta.PrimaryEnv = parsePrimaryEnv(block)
	}

	return meta, nil
}

// extractFrontmatter extracts the YAML block between --- delimiters.
func extractFrontmatter(content string) (string, error) {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return "", fmt.Errorf("skill: SKILL.md missing frontmatter")
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", fmt.Errorf("skill: SKILL.md frontmatter not closed")
	}
	return rest[:end], nil
}

// parseYAML tries to parse the frontmatter block with the YAML library.
// Returns nil on failure (malformed YAML).
func parseYAML(block string) map[string]string {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(block), &raw); err != nil {
		return nil
	}

	result := make(map[string]string, len(raw))
	for k, v := range raw {
		result[k] = coerceValue(v)
	}
	return result
}

// coerceValue converts any YAML value to a string representation.
func coerceValue(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", val)
	case float64:
		return fmt.Sprintf("%g", val)
	case []any:
		data, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(data)
	case map[string]any:
		data, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(data)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", val)
	}
}

var lineKeyRe = regexp.MustCompile(`^([\w-]+):\s*(.*)$`)

// parseLine does simple line-based key: value parsing as a fallback.
// Supports multi-line values via indentation detection.
func parseLine(block string) map[string]string {
	result := make(map[string]string)
	lines := strings.Split(block, "\n")

	for i := 0; i < len(lines); i++ {
		m := lineKeyRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		key := m[1]
		inlineVal := strings.TrimSpace(m[2])

		if inlineVal == "" && i+1 < len(lines) {
			next := lines[i+1]
			if len(next) > 0 && (next[0] == ' ' || next[0] == '\t') {
				val, consumed := extractMultiLineValue(lines, i)
				if val != "" {
					result[key] = val
				}
				i += consumed - 1
				continue
			}
		}

		result[key] = unquote(inlineVal)
	}
	return result
}

// extractMultiLineValue collects indented continuation lines.
func extractMultiLineValue(lines []string, startIdx int) (string, int) {
	var valLines []string
	i := startIdx + 1
	for i < len(lines) {
		line := lines[i]
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			break
		}
		valLines = append(valLines, line)
		i++
	}
	return strings.TrimSpace(strings.Join(valLines, "\n")), i - startIdx
}

// mergeFields merges YAML-parsed and line-parsed results.
// YAML result is the base; for values that look like JSON objects/arrays,
// the line-parsed version is preferred (preserves raw JSON strings that
// YAML would decompose into Go maps).
func mergeFields(yamlParsed, lineParsed map[string]string) map[string]string {
	if yamlParsed == nil {
		return lineParsed
	}

	merged := make(map[string]string, len(yamlParsed))
	for k, v := range yamlParsed {
		merged[k] = v
	}

	for k, v := range lineParsed {
		if strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") {
			merged[k] = v
		}
	}

	return merged
}

// FullReadme returns the full content of the SKILL.md including the
// body after the frontmatter.
func FullReadme(content string) string {
	return content
}

// unquote removes surrounding double or single quotes from a YAML scalar value.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// parsePrimaryEnv extracts primaryEnv from metadata.openclaw.primaryEnv.
func parsePrimaryEnv(block string) string {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(block), &raw); err != nil {
		return ""
	}
	if md, ok := raw["metadata"].(map[string]any); ok {
		if oc, ok := md["openclaw"].(map[string]any); ok {
			if pe, ok := oc["primaryEnv"].(string); ok {
				return pe
			}
		}
	}
	return ""
}

// parseRequires extracts SkillRequires from the raw YAML frontmatter block.
// Priority: metadata.openclaw.requires > top-level requires.
func parseRequires(block string) *SkillRequires {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(block), &raw); err != nil {
		return nil
	}

	if md, ok := raw["metadata"].(map[string]any); ok {
		if oc, ok := md["openclaw"].(map[string]any); ok {
			if req, ok := oc["requires"].(map[string]any); ok {
				return requiresFromMap(req)
			}
		}
	}

	if req, ok := raw["requires"].(map[string]any); ok {
		return requiresFromMap(req)
	}

	return nil
}

func requiresFromMap(m map[string]any) *SkillRequires {
	r := &SkillRequires{
		Bins:    toStringSlice(m["bins"]),
		AnyBins: toStringSlice(m["any_bins"]),
		Env:     toStringSlice(m["env"]),
		OS:      toStringSlice(m["os"]),
	}
	if len(r.Bins) == 0 && len(r.AnyBins) == 0 &&
		len(r.Env) == 0 && len(r.OS) == 0 {
		return nil
	}
	return r
}

func toStringSlice(v any) []string {
	switch val := v.(type) {
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return val
	default:
		return nil
	}
}

// copySkillMeta returns a deep copy of a SkillMeta, including pointer fields.
func copySkillMeta(meta *SkillMeta) *SkillMeta {
	cp := *meta
	if len(meta.Tags) > 0 {
		cp.Tags = make([]string, len(meta.Tags))
		copy(cp.Tags, meta.Tags)
	}
	if meta.Requires != nil {
		r := *meta.Requires
		r.Bins = copyStrings(meta.Requires.Bins)
		r.AnyBins = copyStrings(meta.Requires.AnyBins)
		r.Env = copyStrings(meta.Requires.Env)
		r.OS = copyStrings(meta.Requires.OS)
		cp.Requires = &r
	}
	if meta.Gating != nil {
		g := *meta.Gating
		g.MissingBins = copyStrings(meta.Gating.MissingBins)
		g.MissingAnyBins = copyStrings(meta.Gating.MissingAnyBins)
		g.MissingEnv = copyStrings(meta.Gating.MissingEnv)
		cp.Gating = &g
	}
	return &cp
}

func copyStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	cp := make([]string, len(s))
	copy(cp, s)
	return cp
}

func parseTags(val string) []string {
	if val == "" {
		return nil
	}

	// Try JSON array first (from YAML parser: ["a","b"])
	if strings.HasPrefix(val, "[") {
		var tags []string
		if err := json.Unmarshal([]byte(val), &tags); err == nil && len(tags) > 0 {
			return tags
		}
	}

	// Fallback: comma-separated, possibly with brackets
	val = strings.TrimPrefix(val, "[")
	val = strings.TrimSuffix(val, "]")
	parts := strings.Split(val, ",")
	var tags []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			tags = append(tags, p)
		}
	}
	return tags
}
