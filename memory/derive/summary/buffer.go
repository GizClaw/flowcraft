// Package summary contains deterministic summary derivation implementations.
package summary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/derive"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/model"
)

const (
	DefaultMaxRawMessages         = 32
	DefaultPreserveRecentMessages = 4
	DefaultMaxSummaryBytes        = 4096

	bufferAlgorithm = "summary_buffer"
	bufferVersion   = "v1"
)

// BufferSummarizer folds older recent-window messages into a deterministic
// summary-buffer node. It does not call an LLM and does not read stores.
type BufferSummarizer struct {
	Policy derive.SummaryPolicy
}

var _ derive.Summarizer = BufferSummarizer{}

// Summarize returns a new level-0 summary node for messages older than the
// preserved recent raw buffer. When there are no newly folded messages it
// returns nil to avoid writing duplicate summaries.
func (s BufferSummarizer) Summarize(ctx context.Context, input derive.SummaryInput) ([]viewrecent.SummaryNode, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	policy := normalizePolicy(mergePolicy(s.Policy, input.Policy))
	if len(input.Window.Messages) <= policy.MaxRawMessages {
		return nil, nil
	}

	foldBoundary := len(input.Window.Messages) - policy.MaxRawMessages
	if foldBoundary <= 0 {
		return nil, nil
	}
	foldCandidates := input.Window.Messages[:foldBoundary]

	previous := latestSummaryNode(input.Scope, input.Current)
	covered := coveredSourceRefs(previous)
	folded := make([]sourcemessage.Message, 0, len(foldCandidates))
	foldedRefs := make([]views.SourceRef, 0, len(foldCandidates))
	for i, msg := range foldCandidates {
		ref := sourceRefForWindowMessage(input.Window.SourceRefs, i, msg)
		key, err := ref.StableKeyE()
		if err != nil {
			return nil, err
		}
		if covered[key] {
			continue
		}
		folded = append(folded, msg)
		foldedRefs = append(foldedRefs, ref)
	}
	if len(folded) == 0 {
		return nil, nil
	}

	sourceRefs, err := mergedSourceRefs(previous, foldedRefs)
	if err != nil {
		return nil, err
	}
	revisions, err := mergedSourceRevisions(previous, folded, foldedRefs)
	if err != nil {
		return nil, err
	}

	transform := transformSignature(policy)
	text := buildSummaryText(previous, folded)
	text = truncateBytes(text, policy.MaxSummaryBytes)

	parentIDs := make([]viewrecent.NodeID, 0, 1)
	previousID := ""
	if previous != nil {
		parentIDs = append(parentIDs, previous.ID)
		previousID = string(previous.ID)
	}

	createdAt := summaryTime(previous, folded)
	node := viewrecent.SummaryNode{
		ID:         stableNodeID(input.Scope, previousID, foldedRefs, transform),
		Scope:      input.Scope,
		ParentIDs:  parentIDs,
		SourceRefs: sourceRefs,
		Summary:    text,
		Level:      0,
		Signature: views.ViewSignature{
			ViewID:             input.View.ID,
			SourceRevisions:    revisions,
			TransformSignature: transform,
			DiagnosticSignatures: map[string]string{
				"algorithm": bufferAlgorithm,
				"version":   bufferVersion,
			},
		},
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
		Metadata: map[string]any{
			"algorithm":                bufferAlgorithm,
			"version":                  bufferVersion,
			"max_raw_messages":         policy.MaxRawMessages,
			"preserve_recent_messages": policy.PreserveRecentMessages,
			"max_summary_bytes":        policy.MaxSummaryBytes,
			"folded_message_count":     len(folded),
			"previous_summary_id":      previousID,
			"transform_signature":      transform,
		},
	}
	return []viewrecent.SummaryNode{node}, nil
}

func mergePolicy(base, override derive.SummaryPolicy) derive.SummaryPolicy {
	out := base
	if override.MaxRawMessages != 0 {
		out.MaxRawMessages = override.MaxRawMessages
	}
	if override.PreserveRecentMessages != 0 {
		out.PreserveRecentMessages = override.PreserveRecentMessages
	}
	if override.MaxSummaryBytes != 0 {
		out.MaxSummaryBytes = override.MaxSummaryBytes
	}
	return out
}

func normalizePolicy(policy derive.SummaryPolicy) derive.SummaryPolicy {
	if policy.MaxRawMessages <= 0 {
		policy.MaxRawMessages = DefaultMaxRawMessages
	}
	if policy.PreserveRecentMessages <= 0 {
		policy.PreserveRecentMessages = DefaultPreserveRecentMessages
	}
	if policy.MaxRawMessages < policy.PreserveRecentMessages {
		policy.MaxRawMessages = policy.PreserveRecentMessages
	}
	if policy.MaxSummaryBytes <= 0 {
		policy.MaxSummaryBytes = DefaultMaxSummaryBytes
	}
	return policy
}

func transformSignature(policy derive.SummaryPolicy) string {
	return fmt.Sprintf(
		"%s:%s:max_raw=%d:preserve=%d:max_bytes=%d",
		bufferAlgorithm,
		bufferVersion,
		policy.MaxRawMessages,
		policy.PreserveRecentMessages,
		policy.MaxSummaryBytes,
	)
}

func latestSummaryNode(scope views.Scope, nodes []viewrecent.SummaryNode) *viewrecent.SummaryNode {
	candidates := make([]viewrecent.SummaryNode, 0, len(nodes))
	for _, node := range nodes {
		if node.Scope == scope {
			candidates = append(candidates, node)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.Level != right.Level {
			return left.Level < right.Level
		}
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.Before(right.UpdatedAt)
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.ID < right.ID
	})
	latest := candidates[len(candidates)-1]
	return &latest
}

func coveredSourceRefs(previous *viewrecent.SummaryNode) map[string]bool {
	covered := map[string]bool{}
	if previous == nil {
		return covered
	}
	for _, ref := range previous.SourceRefs {
		key, err := ref.StableKeyE()
		if err != nil {
			continue
		}
		covered[key] = true
	}
	return covered
}

func sourceRefForWindowMessage(refs []views.SourceRef, index int, msg sourcemessage.Message) views.SourceRef {
	if index >= 0 && index < len(refs) {
		return cloneSourceRef(refs[index])
	}
	return views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: msg.ConversationID,
			MessageID:      msg.ID,
		},
	}
}

func mergedSourceRefs(previous *viewrecent.SummaryNode, folded []views.SourceRef) ([]views.SourceRef, error) {
	var refs []views.SourceRef
	seen := map[string]bool{}
	appendRef := func(ref views.SourceRef) error {
		if ref.Kind != views.SourceMessage {
			return fmt.Errorf("summary buffer: source ref must reference message, got %q", ref.Kind)
		}
		key, err := ref.StableKeyE()
		if err != nil {
			return err
		}
		if seen[key] {
			return nil
		}
		seen[key] = true
		refs = append(refs, cloneSourceRef(ref))
		return nil
	}
	if previous != nil {
		for _, ref := range previous.SourceRefs {
			if err := appendRef(ref); err != nil {
				return nil, err
			}
		}
	}
	for _, ref := range folded {
		if err := appendRef(ref); err != nil {
			return nil, err
		}
	}
	return refs, nil
}

func mergedSourceRevisions(previous *viewrecent.SummaryNode, folded []sourcemessage.Message, refs []views.SourceRef) ([]views.SourceRevision, error) {
	var revisions []views.SourceRevision
	seen := map[string]bool{}
	appendRevision := func(rev views.SourceRevision) error {
		if rev.Kind != views.SourceMessage {
			return fmt.Errorf("summary buffer: source revision must reference message, got %q", rev.Kind)
		}
		if err := rev.Validate(); err != nil {
			return err
		}
		key := string(rev.Kind) + "\x00" + rev.SourceKey
		if seen[key] {
			return nil
		}
		seen[key] = true
		revisions = append(revisions, rev)
		return nil
	}
	if previous != nil {
		for _, rev := range previous.Signature.SourceRevisions {
			if err := appendRevision(rev); err != nil {
				return nil, err
			}
		}
	}
	for i, msg := range folded {
		key, err := refs[i].StableKeyE()
		if err != nil {
			return nil, err
		}
		if err := appendRevision(views.SourceRevision{
			Kind:        views.SourceMessage,
			SourceKey:   key,
			Revision:    strconv.FormatUint(msg.Seq, 10),
			ContentHash: MessageContentHash(msg),
			ObservedAt:  msg.CreatedAt,
		}); err != nil {
			return nil, err
		}
	}
	return revisions, nil
}

func buildSummaryText(previous *viewrecent.SummaryNode, folded []sourcemessage.Message) string {
	var b strings.Builder
	if previous != nil && strings.TrimSpace(previous.Summary) != "" {
		b.WriteString("Previous summary:\n")
		b.WriteString(previous.Summary)
		b.WriteString("\n\n")
	}
	b.WriteString("New conversation facts:")
	for _, msg := range folded {
		b.WriteString("\n- ")
		role := string(msg.Message.Role)
		if role == "" {
			role = "unknown"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(indentContinuation(renderMessage(msg.Message)))
	}
	return b.String()
}

func renderMessage(msg model.Message) string {
	parts := make([]string, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		switch part.Type {
		case model.PartText:
			parts = append(parts, part.Text)
		case model.PartToolCall:
			if part.ToolCall == nil {
				parts = append(parts, "[tool_call]")
				continue
			}
			parts = append(parts, fmt.Sprintf("[tool_call id=%s name=%s args=%s]", part.ToolCall.ID, part.ToolCall.Name, part.ToolCall.Arguments))
		case model.PartToolResult:
			if part.ToolResult == nil {
				parts = append(parts, "[tool_result]")
				continue
			}
			parts = append(parts, fmt.Sprintf("[tool_result tool_call_id=%s is_error=%t] %s", part.ToolResult.ToolCallID, part.ToolResult.IsError, part.ToolResult.Content))
		case model.PartImage:
			parts = append(parts, renderJSONPart("image", part.Image))
		case model.PartAudio:
			parts = append(parts, renderJSONPart("audio", part.Audio))
		case model.PartFile:
			parts = append(parts, renderJSONPart("file", part.File))
		case model.PartData:
			parts = append(parts, renderJSONPart("data", part.Data))
		default:
			parts = append(parts, fmt.Sprintf("[part type=%s]", part.Type))
		}
	}
	out := strings.Join(parts, "\n")
	if out == "" {
		return "(empty message)"
	}
	return out
}

func renderJSONPart(label string, value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "[" + label + "]"
	}
	return "[" + label + " " + string(data) + "]"
}

func indentContinuation(text string) string {
	return strings.ReplaceAll(text, "\n", "\n  ")
}

func truncateBytes(text string, max int) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	if max <= 0 {
		return ""
	}
	marker := "\n\n... [truncated] ...\n\n"
	if max <= len(marker) {
		return text[:max]
	}
	available := max - len(marker)
	head := available / 2
	tail := available - head
	return text[:head] + marker + text[len(text)-tail:]
}

func stableNodeID(scope views.Scope, previousID string, folded []views.SourceRef, transform string) viewrecent.NodeID {
	h := sha256.New()
	writeHashPart(h, scope.RuntimeID)
	writeHashPart(h, scope.UserID)
	writeHashPart(h, scope.AgentID)
	writeHashPart(h, scope.ConversationID)
	writeHashPart(h, scope.DatasetID)
	writeHashPart(h, previousID)
	writeHashPart(h, transform)
	for _, ref := range folded {
		key, err := ref.StableKeyE()
		if err != nil {
			key = ""
		}
		writeHashPart(h, key)
	}
	sum := h.Sum(nil)
	return viewrecent.NodeID("summary-buffer-" + hex.EncodeToString(sum[:16]))
}

func writeHashPart(h interface{ Write([]byte) (int, error) }, value string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(value))
	_, _ = h.Write([]byte{0})
}

func summaryTime(previous *viewrecent.SummaryNode, folded []sourcemessage.Message) time.Time {
	var latest time.Time
	for _, msg := range folded {
		if msg.CreatedAt.After(latest) {
			latest = msg.CreatedAt
		}
	}
	if latest.IsZero() && previous != nil {
		return previous.UpdatedAt
	}
	return latest
}

// MessageContentHash returns the stable message hash used in summary source
// revisions.
func MessageContentHash(msg sourcemessage.Message) string {
	data, err := json.Marshal(msg.Message)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneSourceRef(ref views.SourceRef) views.SourceRef {
	if ref.Message != nil {
		msg := *ref.Message
		if msg.Span != nil {
			span := *msg.Span
			msg.Span = &span
		}
		ref.Message = &msg
	}
	if ref.Document != nil {
		doc := *ref.Document
		if doc.Span != nil {
			span := *doc.Span
			doc.Span = &span
		}
		ref.Document = &doc
	}
	return ref
}
