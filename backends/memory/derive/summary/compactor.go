// Package summary deterministically compacts flat conversation records.
package summary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// AlgorithmVersion participates in the derivation policy identity. Bump it
// whenever the summarizer implementation changes (for example the switch from
// the extractive fallback to the model-backed summarizer), so existing
// workspaces rebuild their summary records.
const AlgorithmVersion = "summary-compact-v2"

type Config struct {
	ChunkSize         int
	CondenseThreshold int
	GroupSize         int
	MaxDepth          int
}

func DefaultConfig() Config {
	return Config{ChunkSize: 10, CondenseThreshold: 6, GroupSize: 3, MaxDepth: 4}
}

func (config Config) withDefaults() Config {
	defaults := DefaultConfig()
	if config.ChunkSize == 0 {
		config.ChunkSize = defaults.ChunkSize
	}
	if config.CondenseThreshold == 0 {
		config.CondenseThreshold = defaults.CondenseThreshold
	}
	if config.GroupSize == 0 {
		config.GroupSize = defaults.GroupSize
	}
	if config.MaxDepth == 0 {
		config.MaxDepth = defaults.MaxDepth
	}
	return config
}

func (config Config) validate() error {
	if config.ChunkSize <= 0 || config.CondenseThreshold <= 0 || config.GroupSize <= 0 ||
		config.MaxDepth <= 0 || config.MaxDepth > 4 {
		return errors.New("summary compactor: chunk_size, condense_threshold, group_size, and max_depth must be in range")
	}
	return nil
}

type Input struct {
	ID            string
	Text          string
	Topics        []string
	SourceRefs    []corememory.SourceRef
	CoverageRange summaryview.CoverageRange
}

// InputFromFact builds the canonical compaction input for one published fact.
// The derivation worker (per commit) is the production caller and must use
// this helper: the L0 source digest covers the coverage range, so
// constructing it differently elsewhere yields different records for the
// same fact.
func InputFromFact(fact factview.Fact) Input {
	startSeq, endSeq := provenanceSequenceRange(fact.Provenance)
	eventTime := fact.EventTime
	if eventTime.IsZero() {
		eventTime = fact.CreatedAt
	}
	return Input{
		ID: fact.ID, Text: fact.Text, Topics: append([]string(nil), fact.Entities...),
		SourceRefs: append([]corememory.SourceRef(nil), fact.Provenance...),
		CoverageRange: summaryview.CoverageRange{
			StartSeq: startSeq, EndSeq: endSeq, StartTime: eventTime, EndTime: eventTime,
		},
	}
}

// provenanceSequenceRange derives the covered message sequence interval from
// the fact provenance. Non-message sources and unparsable revisions are
// ignored so the range stays a pure function of the provenance.
func provenanceSequenceRange(sources []corememory.SourceRef) (uint64, uint64) {
	var start, end uint64
	for _, source := range sources {
		if source.Kind != corememory.SourceMessage {
			continue
		}
		value, err := strconv.ParseUint(source.Revision, 10, 64)
		if err != nil {
			continue
		}
		if start == 0 || value < start {
			start = value
		}
		if value > end {
			end = value
		}
	}
	return start, end
}

// ComputeL0SourceDigest recomputes the immutable authority digest for one
// canonical compaction input.
func ComputeL0SourceDigest(input Input) string {
	return digestValue("l0", input)
}

// ComputeRollupSourceDigest recomputes a summary digest from its exact child
// records without trusting the stored parent digest.
func ComputeRollupSourceDigest(inputs []summaryview.Record) string {
	_, _, sources, _, coverage := aggregate(append([]summaryview.Record(nil), inputs...))
	authority := make([]struct {
		ID           string `json:"id"`
		Text         string `json:"text"`
		SourceDigest string `json:"source_digest"`
	}, len(inputs))
	for index, input := range inputs {
		authority[index].ID = input.ID
		authority[index].Text = input.Text
		authority[index].SourceDigest = input.SourceDigest
	}
	return digestValue("source", authority, sources, coverage)
}

type CompactRequest struct {
	Scope           corememory.Scope
	ConversationID  string
	GenerationID    string
	PolicySignature string
	Inputs          []Input
}

type SummarizeRequest struct {
	Level      summaryview.Level
	Texts      []string
	Topics     []string
	SourceRefs []corememory.SourceRef
}

type Summarizer interface {
	Summarize(context.Context, SummarizeRequest) (string, error)
}

// Store is the narrow consumer-side view of the summary view required by the
// compactor; *summaryview.SummaryStore satisfies it structurally.
type Store interface {
	Add(context.Context, summaryview.AddRequest) (summaryview.Record, error)
	Get(context.Context, corememory.Scope, string, string) (summaryview.Record, bool, error)
	LoadActive(context.Context, corememory.Scope, string) (summaryview.Manifest, bool, error)
	PublishActive(context.Context, summaryview.Manifest) error
	ListActive(context.Context, corememory.Scope, string, summaryview.ListOptions) ([]summaryview.Record, error)
}

type Compactor struct {
	config     Config
	store      Store
	summarizer Summarizer
}

func New(config Config, store Store, summarizer Summarizer) (*Compactor, error) {
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("summary compactor: store is required")
	}
	if summarizer == nil {
		summarizer = ExtractiveSummarizer{}
	}
	return &Compactor{config: config, store: store, summarizer: summarizer}, nil
}

// Compact derives the conversation's summary records for the complete input
// window and publishes them as the new active manifest. The request inputs
// must be the full window: the manifest replaces the previous one, so a
// partial window would drop the summary records of the omitted inputs.
// Callers build their inputs with InputFromFact.
func (compactor *Compactor) Compact(ctx context.Context, request CompactRequest) ([]summaryview.Record, error) {
	if compactor == nil || compactor.store == nil || compactor.summarizer == nil {
		return nil, errors.New("summary compactor: compactor is incomplete")
	}
	if ctx == nil {
		return nil, errors.New("summary compactor: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.ConversationID) == "" || strings.TrimSpace(request.GenerationID) == "" {
		return nil, errors.New("summary compactor: conversation_id and generation_id are required")
	}
	inputs := append([]Input(nil), request.Inputs...)
	if len(inputs) == 0 {
		// An empty window would replace the active catalog with nothing.
		// Compaction is incremental, not a reset: callers must pass the
		// complete input window, and the active records stay untouched.
		return compactor.store.ListActive(ctx, request.Scope, request.ConversationID, summaryview.ListOptions{})
	}
	sort.Slice(inputs, func(i, j int) bool {
		left, right := inputs[i].CoverageRange, inputs[j].CoverageRange
		if left.StartSeq != right.StartSeq {
			return left.StartSeq < right.StartSeq
		}
		return inputs[i].ID < inputs[j].ID
	})
	active := make(map[string]summaryview.Record)
	if _, found, err := compactor.store.LoadActive(ctx, request.Scope, request.ConversationID); err != nil {
		return nil, err
	} else if found {
		records, listErr := compactor.store.ListActive(ctx, request.Scope, request.ConversationID, summaryview.ListOptions{})
		if listErr != nil {
			return nil, listErr
		}
		for _, record := range records {
			active[record.ID] = record
		}
	}
	current := make([]summaryview.Record, 0, len(inputs))
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sourceDigest := digestValue("l0", input)
		transform := compactor.transformSignature(request, summaryview.L0)
		inputIDs := []string{input.ID}
		id := summaryview.StableID(request.Scope, request.ConversationID, summaryview.L0,
			inputIDs, sourceDigest, transform)
		record, err := compactor.ensureRecord(ctx, active, summaryview.AddRequest{
			ID:    id,
			Scope: request.Scope, ConversationID: request.ConversationID, Level: summaryview.L0,
			Text: strings.TrimSpace(input.Text), Content: textContent(strings.TrimSpace(input.Text)),
			Topics: input.Topics, InputIDs: inputIDs, SourceRefs: input.SourceRefs,
			CoverageRange: input.CoverageRange, SourceDigest: sourceDigest,
			TransformSignature: transform, GenerationID: request.GenerationID,
		})
		if err != nil {
			return nil, err
		}
		current = append(current, record)
	}
	all := append([]summaryview.Record(nil), current...)
	if len(current) > 0 && compactor.config.MaxDepth > 1 {
		next, err := compactor.compactGroups(ctx, request, active, summaryview.L1, current, compactor.config.ChunkSize)
		if err != nil {
			return nil, err
		}
		current = next
		all = append(all, current...)
	}
	for level := summaryview.L2; len(current) >= compactor.config.CondenseThreshold &&
		level <= summaryview.L3 && int(level) < compactor.config.MaxDepth; level++ {
		next, err := compactor.compactGroups(ctx, request, active, level, current, compactor.config.GroupSize)
		if err != nil {
			return nil, err
		}
		current = next
		all = append(all, current...)
	}
	ids := make([]string, len(all))
	for index, record := range all {
		ids[index] = record.ID
	}
	var coverage summaryview.CoverageRange
	if len(inputs) > 0 {
		_, _, _, _, coverage = aggregate(all[:len(inputs)])
	}
	if err := compactor.store.PublishActive(ctx, summaryview.Manifest{
		Scope: request.Scope, ConversationID: request.ConversationID, GenerationID: request.GenerationID,
		RecordIDs: ids, CoverageRange: coverage,
		FrontierDigest: digestValue("frontier", ids, coverage),
	}); err != nil {
		return nil, err
	}
	return compactor.store.ListActive(ctx, request.Scope, request.ConversationID, summaryview.ListOptions{})
}

func (compactor *Compactor) compactGroups(
	ctx context.Context,
	request CompactRequest,
	active map[string]summaryview.Record,
	level summaryview.Level,
	inputs []summaryview.Record,
	size int,
) ([]summaryview.Record, error) {
	var result []summaryview.Record
	for start := 0; start < len(inputs); start += size {
		end := min(start+size, len(inputs))
		records, err := compactor.summarize(ctx, request, active, level, inputs[start:end])
		if err != nil {
			return nil, err
		}
		result = append(result, records...)
	}
	return result, nil
}

func (compactor *Compactor) summarize(
	ctx context.Context,
	request CompactRequest,
	active map[string]summaryview.Record,
	level summaryview.Level,
	inputs []summaryview.Record,
) ([]summaryview.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	texts, topics, sources, ids, coverage := aggregate(inputs)
	sourceDigest := ComputeRollupSourceDigest(inputs)
	transform := compactor.transformSignature(request, level)
	id := summaryview.StableID(request.Scope, request.ConversationID, level, ids, sourceDigest, transform)
	if record, ok := active[id]; ok {
		return []summaryview.Record{record}, nil
	}
	if record, found, err := compactor.store.Get(ctx, request.Scope, request.ConversationID, id); err != nil {
		return nil, err
	} else if found {
		active[id] = record
		return []summaryview.Record{record}, nil
	}
	text, err := compactor.summarizer.Summarize(ctx, SummarizeRequest{
		Level: level, Texts: texts, Topics: topics, SourceRefs: sources,
	})
	if err == nil && strings.TrimSpace(text) == "" {
		err = errors.New("empty summary")
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if len(inputs) > 1 {
			middle := len(inputs) / 2
			left, leftErr := compactor.summarize(ctx, request, active, level, inputs[:middle])
			if leftErr != nil {
				return nil, leftErr
			}
			right, rightErr := compactor.summarize(ctx, request, active, level, inputs[middle:])
			return append(left, right...), rightErr
		}
		text = extractive(texts)
	}
	record, err := compactor.store.Add(ctx, summaryview.AddRequest{
		ID:    id,
		Scope: request.Scope, ConversationID: request.ConversationID, Level: level,
		Text: strings.TrimSpace(text), Content: textContent(strings.TrimSpace(text)),
		Topics: topics, InputIDs: ids, SourceRefs: sources, CoverageRange: coverage,
		SourceDigest: sourceDigest, TransformSignature: transform, GenerationID: request.GenerationID,
	})
	if err != nil {
		return nil, err
	}
	active[record.ID] = record
	return []summaryview.Record{record}, nil
}

func (compactor *Compactor) transformSignature(request CompactRequest, level summaryview.Level) string {
	policy := strings.TrimSpace(request.PolicySignature)
	if policy == "" {
		policy = digestValue("config", compactor.config)
	}
	return fmt.Sprintf("%s:%s:%s", AlgorithmVersion, level, policy)
}

func (compactor *Compactor) ensureRecord(
	ctx context.Context,
	active map[string]summaryview.Record,
	request summaryview.AddRequest,
) (summaryview.Record, error) {
	if record, ok := active[request.ID]; ok {
		return record, nil
	}
	record, found, err := compactor.store.Get(ctx, request.Scope, request.ConversationID, request.ID)
	if err != nil {
		return summaryview.Record{}, err
	}
	if found {
		active[record.ID] = record
		return record, nil
	}
	record, err = compactor.store.Add(ctx, request)
	if err == nil {
		active[record.ID] = record
	}
	return record, err
}

type ExtractiveSummarizer struct{}

func (ExtractiveSummarizer) Summarize(_ context.Context, request SummarizeRequest) (string, error) {
	return extractive(request.Texts), nil
}

func extractive(texts []string) string {
	const maxRunes = 1200
	text := strings.Join(texts, "\n")
	runes := []rune(text)
	if len(runes) > maxRunes {
		text = string(runes[:maxRunes])
	}
	return text
}

func aggregate(inputs []summaryview.Record) ([]string, []string, []corememory.SourceRef, []string, summaryview.CoverageRange) {
	var texts, topics, ids []string
	var sources []corememory.SourceRef
	var coverage summaryview.CoverageRange
	for index, input := range inputs {
		texts = append(texts, input.Text)
		topics = append(topics, input.Topics...)
		ids = append(ids, input.ID)
		sources = append(sources, input.SourceRefs...)
		if index == 0 || input.CoverageRange.StartSeq < coverage.StartSeq {
			coverage.StartSeq = input.CoverageRange.StartSeq
		}
		if input.CoverageRange.EndSeq > coverage.EndSeq {
			coverage.EndSeq = input.CoverageRange.EndSeq
		}
		if coverage.StartTime.IsZero() || (!input.CoverageRange.StartTime.IsZero() && input.CoverageRange.StartTime.Before(coverage.StartTime)) {
			coverage.StartTime = input.CoverageRange.StartTime
		}
		if input.CoverageRange.EndTime.After(coverage.EndTime) {
			coverage.EndTime = input.CoverageRange.EndTime
		}
	}
	return texts, uniqueStrings(topics), uniqueSources(sources), uniqueOrderedStrings(ids), coverage
}

func uniqueOrderedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func uniqueSources(values []corememory.SourceRef) []corememory.SourceRef {
	sort.Slice(values, func(i, j int) bool {
		left, right := values[i], values[j]
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
	})
	result := values[:0]
	for _, value := range values {
		if len(result) > 0 && result[len(result)-1] == value {
			continue
		}
		result = append(result, value)
	}
	return append([]corememory.SourceRef(nil), result...)
}

func digestValue(values ...any) string {
	data, _ := json.Marshal(values)
	sum := sha256.Sum256(append([]byte("flowcraft.memory.summary.source\x00v1\x00"), data...))
	return hex.EncodeToString(sum[:])
}

func textContent(text string) coremessage.Content {
	return coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text}}}
}
