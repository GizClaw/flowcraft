package skill

import (
	"fmt"
	"os"
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
	// maxSkillFileBytes caps one SKILL.md document (frontmatter +
	// body). Discovery and the body readers refuse larger files, so a
	// third-party skill cannot inject an unbounded document into the
	// model context or memory.
	maxSkillFileBytes = 256 << 10 // 256 KiB
)

// Metadata is one skill parsed from a SKILL.md file.
type Metadata struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	ShortDescription string `json:"short_description,omitempty"` // metadata.short-description (non-standard extension)
	Path             string `json:"path"`                        // canonical path of SKILL.md
}

// ParseResult carries the parsed metadata plus non-fatal diagnostics
// (accepted shape issues such as a name that fell back to the
// directory name).
type ParseResult struct {
	Metadata Metadata
	Warnings []string
}

// ParseFile parses one SKILL.md: YAML frontmatter, validation and
// sanitization. A missing or invalid name falls back to the parent
// directory name with a warning; only malformed files fail.
func ParseFile(path string) (ParseResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ParseResult{}, err
	}
	if info.Size() > maxSkillFileBytes {
		return ParseResult{}, fmt.Errorf(
			"skill: SKILL.md exceeds the %d-byte limit", maxSkillFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ParseResult{}, err
	}
	return parseBytes(path, data)
}

// parseBytes parses SKILL.md content: frontmatter split, YAML decode,
// validation and sanitization. The decode is lenient on purpose —
// third-party skills carry extension fields (license, version,
// metadata.*) that must not fail the parse. A missing or invalid name
// falls back to the parent directory name with a warning; only
// malformed files fail.
func parseBytes(path string, data []byte) (ParseResult, error) {
	if len(data) > maxSkillFileBytes {
		return ParseResult{}, fmt.Errorf(
			"skill: %s exceeds the %d-byte SKILL.md limit",
			path, maxSkillFileBytes)
	}
	fm, body, err := splitFrontmatter(data)
	if err != nil {
		return ParseResult{}, err
	}
	if strings.TrimSpace(string(body)) == "" {
		return ParseResult{}, fmt.Errorf("skill: SKILL.md body is empty")
	}
	// sigs.k8s.io/yaml decodes with encoding/json semantics, so these
	// are json tags: a yaml tag would be ignored.
	var f struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Metadata    struct {
			ShortDescription string `json:"short-description"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(fm, &f); err != nil {
		return ParseResult{}, fmt.Errorf("skill: parse frontmatter: %w", err)
	}
	f.Name = sanitizeSingleLine(f.Name)
	f.Description = sanitizeSingleLine(f.Description)
	fallback := slugify(filepath.Base(filepath.Dir(path)))
	var warnings []string
	if !validName(f.Name) {
		note := "name missing or invalid"
		if f.Name != "" {
			note = fmt.Sprintf("name %q invalid", f.Name)
		}
		f.Name = fallback
		if f.Name == "" {
			return ParseResult{}, fmt.Errorf(
				"skill: %s; directory %q is not usable as a fallback",
				note, filepath.Base(filepath.Dir(path)))
		}
		warnings = append(warnings,
			fmt.Sprintf("%s; fell back to directory name %q", note, fallback))
	} else if f.Name != fallback && fallback != "" {
		// The standard requires name == directory name; third-party
		// skills commonly violate it, so warn instead of rejecting.
		warnings = append(warnings,
			fmt.Sprintf("name %q does not match directory name %q (accepted)", f.Name, fallback))
	}
	if f.Description == "" {
		return ParseResult{}, fmt.Errorf("skill: description is required")
	}
	if utf8.RuneCountInString(f.Description) > maxDescriptionLen {
		f.Description = truncateRunes(f.Description, maxDescriptionLen)
	}
	return ParseResult{
		Metadata: Metadata{
			Name:             f.Name,
			Description:      f.Description,
			ShortDescription: sanitizeSingleLine(f.Metadata.ShortDescription),
			Path:             filepath.Clean(path),
		},
		Warnings: warnings,
	}, nil
}

// splitFrontmatter extracts the YAML frontmatter (between the first
// two --- delimiters) and the Markdown body.
func splitFrontmatter(data []byte) (front []byte, body []byte, err error) {
	// Editors write a UTF-8 BOM often enough that refusing the file
	// would drop a usable skill; the mark is trimmed before the
	// delimiter is checked, in the same lenient spirit as the decode.
	s := strings.TrimPrefix(string(data), "\ufeff")
	if !strings.HasPrefix(s, "---") {
		return nil, nil, fmt.Errorf(
			"skill: missing YAML frontmatter (expected --- ... ---)")
	}
	rest := s[3:]
	if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	} else if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	}
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil, nil, fmt.Errorf("skill: unterminated YAML frontmatter")
	}
	front = []byte(rest[:idx])
	body = []byte(rest[idx+4:])
	return front, body, nil
}

var nameRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func validName(name string) bool {
	return name != "" &&
		utf8.RuneCountInString(name) <= maxNameLen &&
		nameRe.MatchString(name)
}

// slugify converts an arbitrary directory name into a valid skill
// name (lowercase, hyphens), the fallback for third-party skills
// without a usable name field.
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
	out := strings.Trim(b.String(), "-")
	if utf8.RuneCountInString(out) > maxNameLen {
		out = string([]rune(out)[:maxNameLen])
	}
	return out
}

// sanitizeSingleLine folds all whitespace runs into single spaces.
func sanitizeSingleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes shortens s to at most max runes. The limits are
// stated in characters, so a byte slice — which could cut a multi-byte
// rune in half or over-truncate CJK text — is not good enough here.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
