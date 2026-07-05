package claw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ErrMemoryDisabled is returned when a caller tries to append memory through a
// Claw instance whose workspace has memory disabled.
var ErrMemoryDisabled = errors.New("claw: memory is disabled")

// MemoryFactKind is a public semantic memory fact kind accepted by
// AppendMemoryFact.
type MemoryFactKind string

const (
	MemoryFactEvent      MemoryFactKind = "event"
	MemoryFactState      MemoryFactKind = "state"
	MemoryFactPreference MemoryFactKind = "preference"
	MemoryFactProcedure  MemoryFactKind = "procedure"
	MemoryFactRelation   MemoryFactKind = "relation"
	MemoryFactPlan       MemoryFactKind = "plan"
	MemoryFactNote       MemoryFactKind = "note"
)

// MemoryFact is a caller-supplied structured memory fact for Claw workspace
// memory. Content is the primary recall surface; structured fields should only
// be set when the caller has stable domain semantics for them.
type MemoryFact struct {
	// Kind maps to Flowcraft's public semantic memory fact kinds. Empty
	// defaults to MemoryFactNote. The reserved internal episode kind is not
	// accepted through this API.
	Kind MemoryFactKind

	// Content is the recall-friendly natural-language fact text. This is the
	// primary text used by memory recall and prompt context, so callers should
	// make it understandable without requiring Metadata.
	Content string

	// Subject is the primary entity this fact is about, when the caller has a
	// stable entity identity. Examples: "pet:pet-123", "user:alice", or
	// "workspace:demo". Leave empty when the fact is general, when the entity is
	// unknown, or when Content alone is the intended recall surface.
	Subject string

	// Predicate is the stable relation, attribute, or domain action for the
	// fact. Examples: "pet_drive", "favorite_food", "current_mood", or
	// "played_game". For event facts, this is a good place for the domain event
	// type. Leave empty when the caller does not have a durable schema label and
	// only wants text recall.
	Predicate string

	// Object is the target or value of Subject+Predicate when it is naturally
	// representable as a short string. Examples: "wash", "sushi", "happy", or
	// "game:maze". Do not serialize complex payloads into Object; put complex
	// domain data in Metadata and keep Content human-readable.
	Object string

	// Entities are additional searchable entities mentioned by the fact. Use
	// stable ids or names that should help later recall, such as pet ids, item
	// ids, game ids, location names, or participant ids. Entities are not a
	// replacement for Subject; Subject is the main focus, while Entities are
	// extra recall anchors. Leave empty when there are no reliable anchors.
	Entities []string

	// Metadata preserves the original domain payload as JSON-compatible
	// structured data.
	Metadata map[string]any

	// ObservedAt is the observation time. Zero means time.Now().
	ObservedAt time.Time

	// Optional validity bounds for facts that should not be timeless.
	ValidFrom *time.Time
	ValidTo   *time.Time
}

// AppendMemoryFactResult reports the canonical memory facts written by
// AppendMemoryFact.
type AppendMemoryFactResult struct {
	// FactIDs are the canonical memory fact ids written by this call.
	FactIDs []string

	// AsyncRequestID and SemanticPending mirror recall async write semantics if
	// the underlying memory write mode needs them.
	AsyncRequestID  string
	SemanticPending bool
}

// AppendMemoryFact writes a caller-supplied structured fact into this Claw
// workspace's configured memory runtime without creating a chat turn.
func (c *Claw) AppendMemoryFact(ctx context.Context, fact MemoryFact) (AppendMemoryFactResult, error) {
	if c == nil {
		return AppendMemoryFactResult{}, ErrMemoryDisabled
	}
	c.memoryMu.RLock()
	defer c.memoryMu.RUnlock()
	if c.memory == nil || c.memory.mem == nil {
		return AppendMemoryFactResult{}, ErrMemoryDisabled
	}
	return c.memory.appendMemoryFact(ctx, fact)
}

func (m *memoryRuntime) appendMemoryFact(ctx context.Context, fact MemoryFact) (AppendMemoryFactResult, error) {
	if m == nil || m.mem == nil {
		return AppendMemoryFactResult{}, ErrMemoryDisabled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	tf, err := memoryFactToRecallFact(fact, now)
	if err != nil {
		return AppendMemoryFactResult{}, err
	}
	res, err := m.mem.Save(ctx, m.scope, recall.SaveRequest{
		Facts:      []recall.TemporalFact{tf},
		ObservedAt: tf.ObservedAt,
		Tier:       m.cfg.Write.Tier,
		Mode:       parseWriteMode(m.cfg.Write.Mode),
	})
	if err != nil {
		return AppendMemoryFactResult{}, err
	}
	if err := m.drainSideEffects(ctx); err != nil {
		return AppendMemoryFactResult{}, err
	}
	return AppendMemoryFactResult{
		FactIDs:         append([]string(nil), res.FactIDs...),
		AsyncRequestID:  res.AsyncRequestID,
		SemanticPending: res.SemanticPending,
	}, nil
}

func memoryFactToRecallFact(fact MemoryFact, now time.Time) (recall.TemporalFact, error) {
	kind, err := recallKindFromMemoryFactKind(fact.Kind)
	if err != nil {
		return recall.TemporalFact{}, err
	}
	content := strings.TrimSpace(fact.Content)
	if content == "" {
		return recall.TemporalFact{}, errdefs.Validationf("claw: memory fact content is required")
	}
	if len(fact.Metadata) > 0 {
		if _, err := json.Marshal(fact.Metadata); err != nil {
			return recall.TemporalFact{}, errdefs.Validationf("claw: memory fact metadata must be JSON-compatible: %v", err)
		}
	}
	observedAt := fact.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	return recall.TemporalFact{
		Kind:       kind,
		Content:    content,
		Subject:    strings.TrimSpace(fact.Subject),
		Predicate:  strings.TrimSpace(fact.Predicate),
		Object:     strings.TrimSpace(fact.Object),
		Entities:   nonEmptyStrings(fact.Entities),
		ObservedAt: observedAt,
		ValidFrom:  cloneTimePtr(fact.ValidFrom),
		ValidTo:    cloneTimePtr(fact.ValidTo),
		Polarity:   recall.PolarityAffirmed,
		Modality:   recall.ModalityActual,
		Certainty:  recall.CertaintyExplicit,
		Confidence: 0.9,
		Metadata:   cloneMemoryFactMetadata(fact.Metadata),
	}, nil
}

func recallKindFromMemoryFactKind(kind MemoryFactKind) (recall.FactKind, error) {
	switch strings.TrimSpace(string(kind)) {
	case "":
		return recall.FactNote, nil
	case string(MemoryFactEvent):
		return recall.FactEvent, nil
	case string(MemoryFactState):
		return recall.FactState, nil
	case string(MemoryFactPreference):
		return recall.FactPreference, nil
	case string(MemoryFactProcedure):
		return recall.FactProcedure, nil
	case string(MemoryFactRelation):
		return recall.FactRelation, nil
	case string(MemoryFactPlan):
		return recall.FactPlan, nil
	case string(MemoryFactNote):
		return recall.FactNote, nil
	case string(recall.FactEpisode):
		return "", errdefs.Validationf("claw: memory fact kind %q is reserved", recall.FactEpisode)
	default:
		return "", errdefs.Validationf("claw: unsupported memory fact kind %q", kind)
	}
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneMemoryFactMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
