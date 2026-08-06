// Package knowledge implements deterministic knowledge-line derivation.
package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

const (
	// KindDocument is the canonical document input artifact kind.
	KindDocument component.ArtifactKind = "document"
	// KindResource is the root of one immutable document revision hierarchy.
	KindResource component.ArtifactKind = "knowledge_resource"
	// KindSection is one Markdown heading section (or the stable root section).
	KindSection component.ArtifactKind = "knowledge_section"
	// KindChunk is one rune-safe section-local retrieval fragment.
	KindChunk component.ArtifactKind = "document_chunk"
	// KindSummary is an optional deterministic hierarchy summary.
	KindSummary component.ArtifactKind = "knowledge_summary"
	// KindDocumentChunk is the deterministic chunk output artifact kind.
	KindDocumentChunk = KindChunk
	// AlgorithmVersion participates in derivation policy identity.
	AlgorithmVersion = "2.0.0"
)

// ChunkerConfig controls deterministic rune-safe chunking.
type ChunkerConfig struct {
	MaxRunes     int
	OverlapRunes int
	Summary      SummaryConfig
}

// SummaryConfig explicitly enables deterministic extractive summaries.
type SummaryConfig struct {
	Document bool
	Sections bool
	MaxRunes int
}

// Chunker splits document text at paragraph boundaries where possible.
type Chunker struct {
	maxRunes     int
	overlapRunes int
	summary      SummaryConfig
}

var _ component.Deriver = (*Chunker)(nil)

func NewChunker(config ChunkerConfig) (*Chunker, error) {
	if config.MaxRunes <= 0 {
		return nil, errors.New("knowledge line: max_runes must be positive")
	}
	if config.OverlapRunes < 0 || config.OverlapRunes >= config.MaxRunes {
		return nil, errors.New("knowledge line: overlap_runes must be non-negative and less than max_runes")
	}
	if (config.Summary.Document || config.Summary.Sections) && config.Summary.MaxRunes <= 0 {
		return nil, errors.New("knowledge line: summary max_runes must be positive when summaries are enabled")
	}
	return &Chunker{maxRunes: config.MaxRunes, overlapRunes: config.OverlapRunes, summary: config.Summary}, nil
}

func (chunker *Chunker) Derive(ctx context.Context, input component.Artifact) ([]component.Artifact, error) {
	if chunker == nil {
		return nil, errors.New("knowledge line: chunker is required")
	}
	if ctx == nil {
		return nil, errors.New("knowledge line: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input.Kind != KindDocument {
		return nil, fmt.Errorf("knowledge line: input kind %q, want %q", input.Kind, KindDocument)
	}
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("knowledge line: input: %w", err)
	}
	text := normalizeDocumentText(input.Content.Text())
	if text == "" {
		return []component.Artifact{}, nil
	}
	sources := append([]sdkmemory.SourceRef(nil), input.Sources...)
	identitySources := make([]sdkmemory.SourceRef, 0, len(sources))
	for _, source := range sources {
		if source.Kind == sdkmemory.SourceDocument {
			source.Locator = ""
			identitySources = append(identitySources, source)
		}
	}
	if len(identitySources) == 0 {
		return nil, errors.New("knowledge line: document source provenance is required")
	}
	sort.Slice(identitySources, func(i, j int) bool {
		return sourceLess(identitySources[i], identitySources[j])
	})
	sourceDigest, err := identityDigest(identitySources)
	if err != nil {
		return nil, err
	}
	transformSignature := signature(chunker.maxRunes, chunker.overlapRunes, chunker.summary)
	resourceID, err := hierarchyID(identitySources, "resource", "0", text)
	if err != nil {
		return nil, err
	}
	resourceMetadata := hierarchyMetadata(input.Metadata, "resource", 0, "", 0, "", sourceDigest, transformSignature)
	output := []component.Artifact{{
		Kind: KindResource, ID: resourceID, Content: textContent(text),
		Sources: append([]sdkmemory.SourceRef(nil), sources...), Metadata: resourceMetadata,
	}}
	if chunker.summary.Document {
		summary := summarize(text, chunker.summary.MaxRunes)
		summaryID, idErr := hierarchyID(identitySources, "summary", "resource/summary", summary)
		if idErr != nil {
			return nil, idErr
		}
		output = append(output, component.Artifact{
			Kind: KindSummary, ID: summaryID, Content: textContent(summary),
			Sources: append([]sdkmemory.SourceRef(nil), sources...),
			Metadata: hierarchyMetadata(input.Metadata, "summary", 1, resourceID, 0,
				"", sourceDigest, transformSignature),
		})
	}
	sections := parseSections(text)
	for _, section := range sections {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parentID := resourceID
		if section.parent >= 0 {
			parentID = sections[section.parent].id
		}
		content := canonicalSectionContent(section)
		id, err := hierarchyID(identitySources, "section", section.position, content)
		if err != nil {
			return nil, err
		}
		section.id = id
		sections[section.index].id = id
		output = append(output, component.Artifact{
			Kind: KindSection, ID: id, Content: textContent(content),
			Sources: append([]sdkmemory.SourceRef(nil), sources...),
			Metadata: hierarchyMetadata(input.Metadata, "section", section.level, parentID,
				section.ordinal, section.title, sourceDigest, transformSignature),
		})
		if chunker.summary.Sections {
			summary := summarize(content, chunker.summary.MaxRunes)
			summaryID, idErr := hierarchyID(identitySources, "summary", section.position+"/summary", summary)
			if idErr != nil {
				return nil, idErr
			}
			output = append(output, component.Artifact{
				Kind: KindSummary, ID: summaryID, Content: textContent(summary),
				Sources: append([]sdkmemory.SourceRef(nil), sources...),
				Metadata: hierarchyMetadata(input.Metadata, "summary", section.level+1, id, 0,
					section.title, sourceDigest, transformSignature),
			})
		}
		for ordinal, part := range splitText([]rune(section.body), chunker.maxRunes, chunker.overlapRunes) {
			normalized := strings.TrimSpace(part)
			if normalized == "" {
				continue
			}
			chunkPosition := fmt.Sprintf("%s/chunk/%d", section.position, ordinal)
			chunkID, err := hierarchyID(identitySources, "chunk", chunkPosition, normalized)
			if err != nil {
				return nil, err
			}
			output = append(output, component.Artifact{
				Kind: KindChunk, ID: chunkID, Content: textContent(normalized),
				Sources: append([]sdkmemory.SourceRef(nil), sources...),
				Metadata: hierarchyMetadata(input.Metadata, "chunk", section.level+1, id,
					ordinal, "", sourceDigest, transformSignature),
			})
		}
	}
	return output, nil
}

func summarize(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:maxRunes]))
}

type parsedSection struct {
	index    int
	parent   int
	level    int
	ordinal  int
	title    string
	body     string
	position string
	id       string
}

func parseSections(text string) []parsedSection {
	lines := strings.Split(text, "\n")
	sections := make([]parsedSection, 0)
	current := -1
	stack := make([]int, 7)
	for index := range stack {
		stack[index] = -1
	}
	var preamble []string
	fence := ""
	for lineIndex := 0; lineIndex < len(lines); lineIndex++ {
		line := lines[lineIndex]
		if marker := markdownFence(line); marker != "" {
			if fence == "" {
				fence = marker
			} else if marker == fence {
				fence = ""
			}
		}
		level, title, heading := markdownHeading(line)
		heading = heading && fence == ""
		if !heading && fence == "" && lineIndex+1 < len(lines) {
			if setextLevel, ok := markdownSetextLevel(lines[lineIndex+1]); ok && strings.TrimSpace(line) != "" {
				level, title, heading = setextLevel, strings.TrimSpace(line), true
				lineIndex++
			}
		}
		if heading {
			parent := -1
			for parentLevel := level - 1; parentLevel >= 1; parentLevel-- {
				if stack[parentLevel] >= 0 {
					parent = stack[parentLevel]
					break
				}
			}
			ordinal := siblingOrdinal(sections, parent)
			position := fmt.Sprintf("section/%d", ordinal)
			if parent >= 0 {
				position = sections[parent].position + "/" + fmt.Sprint(ordinal)
			}
			sections = append(sections, parsedSection{
				index: len(sections), parent: parent, level: level, ordinal: ordinal,
				title: title, position: position,
			})
			current = len(sections) - 1
			stack[level] = current
			for deeper := level + 1; deeper < len(stack); deeper++ {
				stack[deeper] = -1
			}
			continue
		}
		if current < 0 {
			preamble = append(preamble, line)
		} else {
			sections[current].body += line + "\n"
		}
	}
	preambleText := strings.TrimSpace(strings.Join(preamble, "\n"))
	if len(sections) == 0 {
		return []parsedSection{{
			index: 0, parent: -1, level: 1, ordinal: 0, body: text, position: "section/0",
		}}
	}
	if preambleText != "" {
		root := parsedSection{
			index: 0, parent: -1, level: 1, ordinal: 0, body: preambleText, position: "section/0",
		}
		for index := range sections {
			sections[index].index++
			sections[index].ordinal++
			if sections[index].parent >= 0 {
				sections[index].parent++
			}
			sections[index].position = shiftRootPosition(sections[index].position)
		}
		sections = append([]parsedSection{root}, sections...)
	}
	for index := range sections {
		sections[index].body = strings.TrimSpace(sections[index].body)
	}
	return sections
}

func markdownSetextLevel(line string) (int, bool) {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 1 {
		return 0, false
	}
	marker := trimmed[0]
	if marker != '=' && marker != '-' {
		return 0, false
	}
	for _, char := range trimmed {
		if char != rune(marker) {
			return 0, false
		}
	}
	if marker == '=' {
		return 1, true
	}
	return 2, true
}

func markdownFence(line string) string {
	trimmed := strings.TrimSpace(line)
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, marker) {
			return marker
		}
	}
	return ""
}

func markdownHeading(line string) (int, string, bool) {
	leading := 0
	for leading < len(line) && line[leading] == ' ' {
		leading++
	}
	if leading > 3 || (leading < len(line) && line[leading] == '\t') {
		return 0, "", false
	}
	trimmed := strings.TrimSpace(line[leading:])
	level := 0
	for level < len(trimmed) && level < 6 && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level >= len(trimmed) || trimmed[level] != ' ' {
		return 0, "", false
	}
	title := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(trimmed[level+1:]), "#"))
	if title == "" {
		return 0, "", false
	}
	return level, title, true
}

func siblingOrdinal(sections []parsedSection, parent int) int {
	ordinal := 0
	for _, section := range sections {
		if section.parent == parent {
			ordinal++
		}
	}
	return ordinal
}

func shiftRootPosition(position string) string {
	parts := strings.Split(position, "/")
	if len(parts) > 1 {
		parts[1] = fmt.Sprint(parseSmallInt(parts[1]) + 1)
	}
	return strings.Join(parts, "/")
}

func parseSmallInt(value string) int {
	var result int
	_, _ = fmt.Sscan(value, &result)
	return result
}

func canonicalSectionContent(section parsedSection) string {
	if section.body != "" {
		return section.body
	}
	return section.title
}

func textContent(value string) sdkmessage.Content {
	return sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: value}}}
}

func hierarchyMetadata(base sdkmemory.Metadata, kind string, level int, parentID string, ordinal int,
	title, sourceDigest, transformSignature string,
) sdkmemory.Metadata {
	metadata := base.Clone()
	if metadata == nil {
		metadata = sdkmemory.Metadata{}
	}
	metadata["record_kind"] = kind
	metadata["level"] = fmt.Sprint(level)
	metadata["parent_id"] = parentID
	metadata["ordinal"] = fmt.Sprint(ordinal)
	metadata["title"] = title
	metadata["source_digest"] = sourceDigest
	metadata["transform_signature"] = transformSignature
	return metadata
}

func normalizeDocumentText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = strings.TrimRightFunc(lines[index], unicode.IsSpace)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func splitText(runes []rune, maxRunes, overlap int) []string {
	var chunks []string
	for start := 0; start < len(runes); {
		limit := min(start+maxRunes, len(runes))
		end := limit
		if limit < len(runes) {
			if boundary := paragraphBoundary(runes, start, limit); boundary > start+overlap {
				end = boundary
			}
		}
		chunks = append(chunks, string(runes[start:end]))
		if end == len(runes) {
			break
		}
		next := end - overlap
		if next <= start {
			next = start + 1
		}
		for next < end && unicode.IsSpace(runes[next]) {
			next++
		}
		start = next
	}
	return chunks
}

func paragraphBoundary(runes []rune, start, limit int) int {
	for index := limit - 1; index > start; index-- {
		if runes[index-1] == '\n' && runes[index] == '\n' {
			return index - 1
		}
	}
	return 0
}

func hierarchyID(sources []sdkmemory.SourceRef, kind, position, content string) (string, error) {
	payload := struct {
		Sources  []sdkmemory.SourceRef `json:"sources"`
		Kind     string                `json:"kind"`
		Position string                `json:"position"`
		Content  string                `json:"content"`
	}{sources, kind, position, content}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("knowledge line: encode hierarchy identity: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func identityDigest(sources []sdkmemory.SourceRef) (string, error) {
	data, err := json.Marshal(sources)
	if err != nil {
		return "", fmt.Errorf("knowledge line: encode source identity: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func signature(maxRunes, overlap int, summary SummaryConfig) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%t\x00%t\x00%d",
		AlgorithmVersion, maxRunes, overlap, summary.Document, summary.Sections, summary.MaxRunes)))
	return hex.EncodeToString(sum[:])
}

func sourceLess(left, right sdkmemory.SourceRef) bool {
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.ID != right.ID {
		return left.ID < right.ID
	}
	if left.Revision != right.Revision {
		return left.Revision < right.Revision
	}
	return left.Locator < right.Locator
}
