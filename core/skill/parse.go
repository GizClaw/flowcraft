package skill

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"sigs.k8s.io/yaml"
)

// Limits mirror the agentskills.io parser behaviour.
const (
	maxNameLen        = 64
	maxDescriptionLen = 1024
	// maxSkillFileBytes caps one SKILL.md document (frontmatter plus body), so
	// a third-party skill cannot inject an unbounded document into the context.
	maxSkillFileBytes = 256 << 10
)

// Metadata is one skill parsed from a SKILL.md file.
type Metadata struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	ShortDescription string `json:"short_description,omitempty"`
	// Path is the absolute path of the SKILL.md this metadata came from.
	Path string `json:"path"`
}

// ParseResult carries the parsed metadata plus non-fatal diagnostics.
type ParseResult struct {
	Metadata Metadata
	Warnings []string
}

// Parse parses one SKILL.md body. It takes bytes rather than a path so the
// caller decides where they came from — the discovery pass reads through fsx,
// a test passes a literal.
//
// A missing or invalid name falls back to the parent directory name with a
// warning; a missing description or a malformed document fails.
func Parse(path string, data []byte) (ParseResult, error) {
	if len(data) > maxSkillFileBytes {
		return ParseResult{}, fmt.Errorf(
			"skill: %s exceeds the %d-byte SKILL.md limit", path, maxSkillFileBytes)
	}
	fm, body, err := splitFrontmatter(data)
	if err != nil {
		return ParseResult{}, err
	}
	if strings.TrimSpace(string(body)) == "" {
		return ParseResult{}, fmt.Errorf("skill: %s has an empty body", path)
	}

	var front struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Metadata    struct {
			ShortDescription string `json:"short-description"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(fm, &front); err != nil {
		return ParseResult{}, fmt.Errorf("skill: parse frontmatter of %s: %w", path, err)
	}
	front.Name = sanitizeSingleLine(front.Name)
	front.Description = sanitizeSingleLine(front.Description)

	fallback := slugify(filepath.Base(filepath.Dir(path)))
	var warnings []string
	if !validName(front.Name) {
		note := "name missing or invalid"
		if front.Name != "" {
			note = fmt.Sprintf("name %q invalid", front.Name)
		}
		if fallback == "" {
			return ParseResult{}, fmt.Errorf(
				"skill: %s; directory %q is not usable as a fallback",
				note, filepath.Base(filepath.Dir(path)))
		}
		front.Name = fallback
		warnings = append(warnings,
			fmt.Sprintf("%s; fell back to directory name %q", note, fallback))
	} else if fallback != "" && front.Name != fallback {
		// The standard wants name == directory name. Third-party skills
		// commonly violate it, so warn instead of rejecting.
		warnings = append(warnings,
			fmt.Sprintf("name %q does not match directory name %q (accepted)", front.Name, fallback))
	}
	if front.Description == "" {
		return ParseResult{}, fmt.Errorf("skill: %s has no description", path)
	}
	front.Description = truncateRunes(front.Description, maxDescriptionLen)

	return ParseResult{
		Metadata: Metadata{
			Name:             front.Name,
			Description:      front.Description,
			ShortDescription: sanitizeSingleLine(front.Metadata.ShortDescription),
			Path:             filepath.Clean(path),
		},
		Warnings: warnings,
	}, nil
}

// splitFrontmatter extracts the YAML frontmatter between the first two ---
// delimiters and the Markdown body. A UTF-8 BOM is trimmed first: editors write
// one often enough that refusing the file would drop a usable skill.
func splitFrontmatter(data []byte) (front []byte, body []byte, err error) {
	s := strings.TrimPrefix(string(data), "\ufeff")
	if !strings.HasPrefix(s, "---") {
		return nil, nil, fmt.Errorf("skill: missing YAML frontmatter (expected --- ... ---)")
	}
	rest := s[3:]
	switch {
	case strings.HasPrefix(rest, "\r\n"):
		rest = rest[2:]
	case strings.HasPrefix(rest, "\n"):
		rest = rest[1:]
	}
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil, nil, fmt.Errorf("skill: unterminated YAML frontmatter")
	}
	return []byte(rest[:idx]), []byte(rest[idx+4:]), nil
}

var nameRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func validName(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= maxNameLen && nameRe.MatchString(name)
}

// slugify turns an arbitrary directory name into a valid skill name.
func slugify(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return truncateRunes(strings.Trim(b.String(), "-"), maxNameLen)
}

// sanitizeSingleLine folds every whitespace run into one space.
func sanitizeSingleLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncateRunes shortens by characters, not bytes: the limits are stated in
// characters, and a byte slice would cut a multi-byte rune in half.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
