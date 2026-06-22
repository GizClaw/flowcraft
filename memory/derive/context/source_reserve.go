package context

import (
	stdctx "context"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
)

// ReserveDerivedHitKind identifies which derived hit families participate in a
// source-backed reserve packer's gate.
type ReserveDerivedHitKind string

const (
	ReserveDerivedSummary    ReserveDerivedHitKind = "summary"
	ReserveDerivedEntityFact ReserveDerivedHitKind = "entity_fact"
	ReserveDerivedDocument   ReserveDerivedHitKind = "document"
)

// SourceBackedReservePacker appends raw source-message evidence referenced by
// derived memory hits. It preserves the base packer's ordering and only adds
// reserve items to the tail when the configured gate is satisfied.
type SourceBackedReservePacker struct {
	Base derive.ContextPacker

	MaxReserveMessages   int
	MaxSourceRefsPerHit  int
	MinQueryTokens       int
	MinDerivedHits       int
	MinReserveCandidates int
	MinRelativeScore     float64
	MinEntityConfidence  float64

	UseSummaryRefs    bool
	UseEntityFactRefs bool

	GateOn []ReserveDerivedHitKind

	ReserveMetadata map[string]any
}

func (p SourceBackedReservePacker) PackContext(ctx stdctx.Context, input derive.ContextPackInput) (derive.ContextPackOutput, error) {
	base := p.Base
	if base == nil {
		base = RRFPacker{}
	}
	output, err := base.PackContext(ctx, input)
	if err != nil {
		return derive.ContextPackOutput{}, err
	}
	if p.MaxReserveMessages <= 0 {
		return output, nil
	}
	if input.SourceMessages == nil {
		return output, nil
	}
	candidates, err := p.reserveCandidates(ctx, input, output.Items)
	if err != nil {
		return derive.ContextPackOutput{}, err
	}
	if !p.shouldAppend(input, candidates) {
		return output, nil
	}
	out := append([]derive.ContextItem(nil), output.Items...)
	added := 0
	for _, candidate := range candidates {
		if added >= p.MaxReserveMessages {
			break
		}
		out = append(out, candidate.item)
		added++
	}
	return derive.ContextPackOutput{Items: out}, nil
}

type sourceReserveCandidate struct {
	item      derive.ContextItem
	key       string
	hitRank   int
	refRank   int
	score     float64
	firstSeen int
}

func (p SourceBackedReservePacker) reserveCandidates(ctx stdctx.Context, input derive.ContextPackInput, baseline []derive.ContextItem) ([]sourceReserveCandidate, error) {
	seen := contextItemKeySet(baseline)
	candidates := map[string]sourceReserveCandidate{}
	ordinal := 0
	add := func(ref views.SourceRef, hit retrieval.Hit, hitRank int, refRank int, score float64, source string) error {
		ordinal++
		if ref.Kind != views.SourceMessage || ref.Message == nil {
			return nil
		}
		msg, ok, err := input.SourceMessages.GetSourceMessage(ctx, ref.Message.ConversationID, ref.Message.MessageID)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		key := sourceMessageKey(msg)
		if key == "" || seen[key] {
			return nil
		}
		item := sourceReserveContextItem(msg, hit, source, p.ReserveMetadata)
		if strings.TrimSpace(item.Text) == "" {
			return nil
		}
		candidate := sourceReserveCandidate{item: item, key: key, hitRank: hitRank, refRank: refRank, score: score, firstSeen: ordinal}
		if existing, ok := candidates[key]; ok && compareSourceReserveCandidate(existing, candidate) <= 0 {
			return nil
		}
		candidates[key] = candidate
		return nil
	}

	if p.UseSummaryRefs {
		for hitRank, hit := range input.SummaryHits {
			baseScore := normalizedRetrievalScore(hit.Retrieval, hitRank)
			for refRank, ref := range hit.Node.SourceRefs {
				if p.MaxSourceRefsPerHit > 0 && refRank >= p.MaxSourceRefsPerHit {
					break
				}
				if err := add(ref, hit.Retrieval, hitRank, refRank, baseScore/float64(refRank+1), string(ReserveDerivedSummary)); err != nil {
					return nil, err
				}
			}
		}
	}
	if p.UseEntityFactRefs {
		for hitRank, hit := range input.EntityHits {
			confidence := hit.Fact.Confidence
			if confidence <= 0 {
				confidence = 0.8
			}
			if confidence > 1 {
				confidence = 1
			}
			if p.MinEntityConfidence > 0 && confidence < p.MinEntityConfidence {
				continue
			}
			baseScore := confidence / float64(hitRank+1)
			for refRank, ref := range hit.Fact.SourceRefs {
				if p.MaxSourceRefsPerHit > 0 && refRank >= p.MaxSourceRefsPerHit {
					break
				}
				if err := add(ref, hit.Retrieval, hitRank, refRank, baseScore/float64(refRank+1), string(ReserveDerivedEntityFact)); err != nil {
					return nil, err
				}
			}
		}
	}

	out := make([]sourceReserveCandidate, 0, len(candidates))
	maxScore := 0.0
	for _, candidate := range candidates {
		out = append(out, candidate)
		if candidate.score > maxScore {
			maxScore = candidate.score
		}
	}
	if p.MinRelativeScore > 0 && maxScore > 0 {
		minScore := maxScore * p.MinRelativeScore
		filtered := out[:0]
		for _, candidate := range out {
			if candidate.score >= minScore {
				filtered = append(filtered, candidate)
			}
		}
		out = filtered
	}
	sort.SliceStable(out, func(i, j int) bool {
		return compareSourceReserveCandidate(out[i], out[j]) < 0
	})
	return out, nil
}

func (p SourceBackedReservePacker) shouldAppend(input derive.ContextPackInput, candidates []sourceReserveCandidate) bool {
	if len(candidates) < p.MinReserveCandidates {
		return false
	}
	if p.MinQueryTokens > 0 && discriminativeQueryTokenCount(input.Query) < p.MinQueryTokens {
		return false
	}
	if p.MinDerivedHits > 0 && p.derivedHitCount(input) < p.MinDerivedHits {
		return false
	}
	return true
}

func (p SourceBackedReservePacker) derivedHitCount(input derive.ContextPackInput) int {
	kinds := p.GateOn
	if len(kinds) == 0 {
		if p.UseSummaryRefs {
			kinds = append(kinds, ReserveDerivedSummary)
		}
		if p.UseEntityFactRefs {
			kinds = append(kinds, ReserveDerivedEntityFact)
		}
	}
	total := 0
	for _, kind := range kinds {
		switch kind {
		case ReserveDerivedSummary:
			total += len(input.SummaryHits)
		case ReserveDerivedEntityFact:
			total += len(input.EntityHits)
		case ReserveDerivedDocument:
			total += len(input.DocumentHits)
		}
	}
	return total
}

func sourceReserveContextItem(msg sourcemessage.Message, hit retrieval.Hit, source string, metadata map[string]any) derive.ContextItem {
	msg.Metadata = maps.Clone(msg.Metadata)
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	msg.Metadata["source_backed_reserve"] = true
	msg.Metadata["source_backed_reserve_source"] = source
	for key, value := range metadata {
		msg.Metadata[key] = value
	}
	return derive.ContextItem{
		Kind:      derive.ContextItemRecentMessage,
		Text:      renderSourceMessageText(msg),
		Message:   &msg,
		Retrieval: &hit,
	}
}

func renderSourceMessageText(msg sourcemessage.Message) string {
	content := msg.Content()
	if strings.TrimSpace(content) == "" {
		return ""
	}
	return string(msg.Role) + ": " + content
}

func contextItemKeySet(items []derive.ContextItem) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		if item.Message != nil {
			if key := sourceMessageKey(*item.Message); key != "" {
				out[key] = true
			}
		}
	}
	return out
}

func sourceMessageKey(msg sourcemessage.Message) string {
	if msg.ConversationID != "" && msg.ID != "" {
		return "message:" + msg.ConversationID + ":" + msg.ID
	}
	if msg.ID != "" {
		return "message:" + msg.ID
	}
	return ""
}

func normalizedRetrievalScore(hit retrieval.Hit, hitRank int) float64 {
	if hit.Score > 0 {
		return hit.Score / float64(hitRank+1)
	}
	return 1 / float64(hitRank+1)
}

func compareSourceReserveCandidate(a, b sourceReserveCandidate) int {
	if a.score > b.score {
		return -1
	}
	if a.score < b.score {
		return 1
	}
	if a.hitRank < b.hitRank {
		return -1
	}
	if a.hitRank > b.hitRank {
		return 1
	}
	if a.refRank < b.refRank {
		return -1
	}
	if a.refRank > b.refRank {
		return 1
	}
	if a.firstSeen < b.firstSeen {
		return -1
	}
	if a.firstSeen > b.firstSeen {
		return 1
	}
	return strings.Compare(a.key, b.key)
}

func discriminativeQueryTokenCount(query string) int {
	seen := map[string]bool{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		token := strings.TrimSpace(raw)
		if len(token) < 3 {
			continue
		}
		seen[token] = true
	}
	return len(seen)
}

func (p SourceBackedReservePacker) String() string {
	return fmt.Sprintf("SourceBackedReservePacker(max=%d)", p.MaxReserveMessages)
}
