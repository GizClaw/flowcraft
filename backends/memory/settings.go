package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	summaryderive "github.com/GizClaw/flowcraft/backends/memory/derive/summary"
	"github.com/GizClaw/flowcraft/backends/memory/lines/chat"
	"github.com/GizClaw/flowcraft/backends/memory/lines/knowledge"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// Storage driver names.
const (
	DriverWorkspace = "workspace"
	DriverSQLite    = "sqlite"
	DriverPostgres  = "postgres"
)

const (
	defaultRecentMaxItems  = 20
	defaultRecentMaxTokens = 2048
	defaultProjection      = "facts"
	defaultInterval        = "1m"

	// maxSummaryChunkSize and maxSummaryGroupSize bound the summary
	// compactor's fan-in so one derivation cannot build an unbounded
	// hierarchy in a single step.
	maxSummaryChunkSize = 10_000
	maxSummaryGroupSize = 1_000
)

// Settings is the strict memory.yaml document owned by this module. It has
// no version field; unknown fields are rejected by resource decoding.
type Settings struct {
	Storage    StorageSettings   `json:"storage"`
	Generate   ModelSettings     `json:"generate,omitempty"`
	Embed      ModelSettings     `json:"embed,omitempty"`
	Scopes     []ScopeSettings   `json:"scopes,omitempty"`
	Recent     RecentSettings    `json:"recent,omitempty"`
	Fact       FactSettings      `json:"fact,omitempty"`
	BM25       BM25Settings      `json:"bm25,omitempty"`
	Summary    SummarySettings   `json:"summary,omitempty"`
	Chunk      ChunkSettings     `json:"chunk,omitempty"`
	Lanes      LanesSettings     `json:"lanes,omitempty"`
	Retrieval  RetrievalSettings `json:"retrieval,omitempty"`
	Projection string            `json:"projection,omitempty"`
	// Derive tunes the derivation scan itself. Concurrency only changes the
	// schedule (how many conversations of a scope are derived at once), never
	// what is derived, so it stays out of the policy digest.
	Derive DeriveSettings `json:"derive,omitempty"`
	// Interval is the derivation scan period; "0" disables the background
	// runner while keeping RunOnce available.
	Interval string `json:"interval,omitempty"`
}

// StorageSettings selects the canonical Log and KV drivers.
type StorageSettings struct {
	Log DriverSettings `json:"log"`
	KV  DriverSettings `json:"kv"`
}

// DeriveSettings tunes the derivation scan.
type DeriveSettings struct {
	// Concurrency bounds how many conversations of one scope are derived in
	// parallel. Zero or one derives them one after another. Each conversation
	// keeps its own watermark and commit order, so the derived content is the
	// same either way.
	Concurrency int `json:"concurrency,omitempty"`
}

// DriverSettings names one storage driver plus its strict settings subtree.
type DriverSettings struct {
	Driver   string          `json:"driver"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// ModelSettings is the credential-free model reference used for generate and
// embed calls. Credentials stay in the inference document.
type ModelSettings struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Profile  string `json:"profile,omitempty"`
}

// ScopeSettings seeds a memory partition registered during Wire.
type ScopeSettings struct {
	RuntimeID string `json:"runtime_id"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

// RecentSettings bounds the recent-message lane when a request does not
// supply its own limits. Request-level limits may override these values but
// are themselves clamped to retrieval.MaxRecentItems/MaxRecentTokens.
type RecentSettings struct {
	MaxItems  int `json:"max_items,omitempty"`
	MaxTokens int `json:"max_tokens,omitempty"`
}

// FactSettings bounds chat fact extraction.
type FactSettings struct {
	Strategy               string `json:"strategy,omitempty"`
	TailMaxChars           int    `json:"tail_max_chars,omitempty"`
	MaxFacts               int    `json:"max_facts,omitempty"`
	MaxFactChars           int    `json:"max_fact_chars,omitempty"`
	MaxQueryChars          int    `json:"max_query_chars,omitempty"`
	MaxEmbeddingInputChars int    `json:"max_embedding_input_chars,omitempty"`
}

// BM25Settings tunes the lexical lane.
type BM25Settings struct {
	K1 float64 `json:"k1,omitempty"`
	B  float64 `json:"b,omitempty"`
}

// SummarySettings configures the L0-L3 summary branch. Disabled keeps
// summaries out of the pipeline entirely.
type SummarySettings struct {
	Disabled          bool `json:"disabled,omitempty"`
	ChunkSize         int  `json:"chunk_size,omitempty"`
	CondenseThreshold int  `json:"condense_threshold,omitempty"`
	GroupSize         int  `json:"group_size,omitempty"`
	MaxDepth          int  `json:"max_depth,omitempty"`
}

// ChunkSettings configures the deterministic knowledge-line chunker.
type ChunkSettings struct {
	MaxRunes     int                  `json:"max_runes,omitempty"`
	OverlapRunes int                  `json:"overlap_runes,omitempty"`
	Summary      ChunkSummarySettings `json:"summary,omitempty"`
}

// ChunkSummarySettings enables deterministic extractive hierarchy summaries.
type ChunkSummarySettings struct {
	Document bool `json:"document,omitempty"`
	Sections bool `json:"sections,omitempty"`
	MaxRunes int  `json:"max_runes,omitempty"`
}

// LaneSettings configures one retrieval lane.
type LaneSettings struct {
	Weight float64 `json:"weight,omitempty"`
}

// RetrievalSettings configures query-time retrieval behavior.
type RetrievalSettings struct {
	// SourceQuotes is how many source messages a derived fact may pull into
	// the context (0 disables it). Facts paraphrase, so surface details such
	// as a title or a quoted number only exist in the source turn.
	SourceQuotes int `json:"source_quotes,omitempty"`
	// Rerank reorders hydrated context items with the generate model before
	// packing (one extra generate call per context request).
	Rerank bool `json:"rerank,omitempty"`
}

// LanesSettings configures the built-in retrieval lanes.
type LanesSettings struct {
	Vector  LaneSettings `json:"vector,omitempty"`
	BM25    LaneSettings `json:"bm25,omitempty"`
	Entity  LaneSettings `json:"entity,omitempty"`
	Summary LaneSettings `json:"summary,omitempty"`
	// Fusion selects the lane fusion algorithm: "rrf" (default) or
	// "weighted". RRF is robust to score-scale differences between lanes.
	Fusion string `json:"fusion,omitempty"`
}

// applyDefaults fills zero values with the documented defaults in place.
func (settings *Settings) applyDefaults() {
	if strings.TrimSpace(settings.Storage.Log.Driver) == "" {
		settings.Storage.Log.Driver = DriverWorkspace
	}
	if strings.TrimSpace(settings.Storage.KV.Driver) == "" {
		settings.Storage.KV.Driver = DriverWorkspace
	}
	if settings.Recent.MaxItems == 0 {
		settings.Recent.MaxItems = defaultRecentMaxItems
	}
	if settings.Recent.MaxTokens == 0 {
		settings.Recent.MaxTokens = defaultRecentMaxTokens
	}
	if strings.TrimSpace(settings.Projection) == "" {
		settings.Projection = defaultProjection
	}
	if strings.TrimSpace(settings.Interval) == "" {
		settings.Interval = defaultInterval
	}
	defaults := chat.DefaultConfig()
	fact := &settings.Fact
	if fact.Strategy == "" {
		fact.Strategy = string(defaults.Strategy)
	}
	if fact.TailMaxChars == 0 {
		fact.TailMaxChars = defaults.TailMaxChars
	}
	if fact.MaxFacts == 0 {
		fact.MaxFacts = defaults.MaxFacts
	}
	if fact.MaxFactChars == 0 {
		fact.MaxFactChars = defaults.MaxFactChars
	}
	if fact.MaxQueryChars == 0 {
		fact.MaxQueryChars = defaults.MaxQueryChars
	}
	if fact.MaxEmbeddingInputChars == 0 {
		fact.MaxEmbeddingInputChars = defaults.MaxEmbeddingInputChars
	}
	if !settings.Summary.Disabled {
		summaryDefaults := summaryderive.DefaultConfig()
		if settings.Summary.ChunkSize == 0 {
			settings.Summary.ChunkSize = summaryDefaults.ChunkSize
		}
		if settings.Summary.CondenseThreshold == 0 {
			settings.Summary.CondenseThreshold = summaryDefaults.CondenseThreshold
		}
		if settings.Summary.GroupSize == 0 {
			settings.Summary.GroupSize = summaryDefaults.GroupSize
		}
		if settings.Summary.MaxDepth == 0 {
			settings.Summary.MaxDepth = summaryDefaults.MaxDepth
		}
	}
	if settings.Chunk.MaxRunes == 0 {
		settings.Chunk.MaxRunes = 1600
		if settings.Chunk.OverlapRunes == 0 {
			settings.Chunk.OverlapRunes = 160
		}
	}
	if settings.Lanes.Vector.Weight == 0 {
		settings.Lanes.Vector.Weight = 1
	}
	if settings.Lanes.BM25.Weight == 0 {
		settings.Lanes.BM25.Weight = 0.6
	}
	if settings.Lanes.Entity.Weight == 0 {
		settings.Lanes.Entity.Weight = 0.4
	}
	if settings.Lanes.Summary.Weight == 0 {
		settings.Lanes.Summary.Weight = 0.5
	}
	if strings.TrimSpace(settings.Lanes.Fusion) == "" {
		settings.Lanes.Fusion = "rrf"
	}
}

// Validate rejects invalid settings after defaults are applied.
func (settings Settings) Validate() error {
	if err := settings.Storage.validate(); err != nil {
		return err
	}
	if err := settings.Generate.validate("generate"); err != nil {
		return err
	}
	if err := settings.Embed.validate("embed"); err != nil {
		return err
	}
	for index, seed := range settings.Scopes {
		if err := seed.scope().Validate(); err != nil {
			return fmt.Errorf("scopes[%d]: %w", index, err)
		}
	}
	if settings.Recent.MaxItems < 0 {
		return errors.New("recent.max_items must not be negative")
	}
	if settings.Recent.MaxTokens < 0 {
		return errors.New("recent.max_tokens must not be negative")
	}
	if settings.Recent.MaxItems > retrieval.MaxRecentItems {
		return fmt.Errorf("recent.max_items must not exceed %d", retrieval.MaxRecentItems)
	}
	if settings.Retrieval.SourceQuotes < 0 || settings.Retrieval.SourceQuotes > 4 {
		return errors.New("retrieval.source_quotes must be between 0 and 4")
	}
	if settings.Derive.Concurrency < 0 {
		return errors.New("derive.concurrency must not be negative")
	}
	if settings.Recent.MaxTokens > retrieval.MaxRecentTokens {
		return fmt.Errorf("recent.max_tokens must not exceed %d", retrieval.MaxRecentTokens)
	}
	if _, err := settings.interval(); err != nil {
		return err
	}
	switch chat.FactStrategy(settings.Fact.Strategy) {
	case chat.StrategyNone, chat.StrategySimple, chat.StrategyRich:
	default:
		return fmt.Errorf("fact.strategy %q is not supported", settings.Fact.Strategy)
	}
	for name, value := range map[string]int{
		"fact.tail_max_chars":            settings.Fact.TailMaxChars,
		"fact.max_facts":                 settings.Fact.MaxFacts,
		"fact.max_fact_chars":            settings.Fact.MaxFactChars,
		"fact.max_query_chars":           settings.Fact.MaxQueryChars,
		"fact.max_embedding_input_chars": settings.Fact.MaxEmbeddingInputChars,
	} {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	for name, bound := range map[string]struct{ value, max int }{
		"fact.tail_max_chars":            {settings.Fact.TailMaxChars, chat.MaxTailChars},
		"fact.max_facts":                 {settings.Fact.MaxFacts, chat.MaxFacts},
		"fact.max_fact_chars":            {settings.Fact.MaxFactChars, chat.MaxFactChars},
		"fact.max_query_chars":           {settings.Fact.MaxQueryChars, chat.MaxQueryChars},
		"fact.max_embedding_input_chars": {settings.Fact.MaxEmbeddingInputChars, chat.MaxEmbeddingInputChars},
	} {
		if bound.value > bound.max {
			return fmt.Errorf("%s must not exceed %d", name, bound.max)
		}
	}
	for name, weight := range map[string]float64{
		"lanes.vector.weight":  settings.Lanes.Vector.Weight,
		"lanes.bm25.weight":    settings.Lanes.BM25.Weight,
		"lanes.entity.weight":  settings.Lanes.Entity.Weight,
		"lanes.summary.weight": settings.Lanes.Summary.Weight,
	} {
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight <= 0 {
			return fmt.Errorf("%s must be finite and positive", name)
		}
	}
	if math.IsNaN(settings.BM25.K1) || math.IsInf(settings.BM25.K1, 0) || settings.BM25.K1 < 0 {
		return errors.New("bm25.k1 must be finite and non-negative")
	}
	switch settings.Lanes.Fusion {
	case "rrf", "weighted":
	default:
		return fmt.Errorf("lanes.fusion %q is not supported", settings.Lanes.Fusion)
	}
	if math.IsNaN(settings.BM25.B) || math.IsInf(settings.BM25.B, 0) || settings.BM25.B < 0 || settings.BM25.B > 1 {
		return errors.New("bm25.b must be within [0,1]")
	}
	if !settings.Summary.Disabled {
		for name, value := range map[string]int{
			"summary.chunk_size":         settings.Summary.ChunkSize,
			"summary.condense_threshold": settings.Summary.CondenseThreshold,
			"summary.group_size":         settings.Summary.GroupSize,
			"summary.max_depth":          settings.Summary.MaxDepth,
		} {
			if value <= 0 {
				return fmt.Errorf("%s must be positive", name)
			}
		}
		if settings.Summary.ChunkSize > maxSummaryChunkSize {
			return fmt.Errorf("summary.chunk_size must not exceed %d", maxSummaryChunkSize)
		}
		if settings.Summary.GroupSize > maxSummaryGroupSize {
			return fmt.Errorf("summary.group_size must not exceed %d", maxSummaryGroupSize)
		}
	}
	if settings.Chunk.MaxRunes <= 0 || settings.Chunk.OverlapRunes < 0 || settings.Chunk.OverlapRunes >= settings.Chunk.MaxRunes {
		return errors.New("chunk.max_runes must be positive and overlap_runes must be non-negative and smaller")
	}
	if settings.Chunk.MaxRunes > knowledge.MaxChunkRunes {
		return fmt.Errorf("chunk.max_runes must not exceed %d", knowledge.MaxChunkRunes)
	}
	if (settings.Chunk.Summary.Document || settings.Chunk.Summary.Sections) && settings.Chunk.Summary.MaxRunes <= 0 {
		return errors.New("chunk.summary.max_runes must be positive when summaries are enabled")
	}
	if settings.Chunk.Summary.MaxRunes > knowledge.MaxSummaryRunes {
		return fmt.Errorf("chunk.summary.max_runes must not exceed %d", knowledge.MaxSummaryRunes)
	}
	return nil
}

func (settings StorageSettings) validate() error {
	if err := settings.Log.validate("storage.log"); err != nil {
		return err
	}
	return settings.KV.validate("storage.kv")
}

func (settings DriverSettings) validate(name string) error {
	switch settings.Driver {
	case DriverWorkspace, DriverSQLite, DriverPostgres:
		return nil
	default:
		return fmt.Errorf("%s.driver %q is not supported", name, settings.Driver)
	}
}

func (settings ModelSettings) isZero() bool {
	return settings.Provider == "" && settings.Name == "" && settings.Profile == ""
}

func (settings ModelSettings) validate(name string) error {
	if settings.isZero() {
		return nil
	}
	if strings.TrimSpace(settings.Provider) == "" {
		return fmt.Errorf("%s.provider is required", name)
	}
	if strings.TrimSpace(settings.Name) == "" {
		return fmt.Errorf("%s.name is required", name)
	}
	return nil
}

func (settings ModelSettings) ref() model.ModelRef {
	return model.ModelRef{
		ID:      model.ModelID{Provider: settings.Provider, Name: settings.Name},
		Profile: settings.Profile,
	}
}

func (settings FactSettings) strategy() chat.FactStrategy {
	return chat.FactStrategy(settings.Strategy)
}

func (settings ScopeSettings) scope() corememory.Scope {
	return corememory.Scope{RuntimeID: settings.RuntimeID, UserID: settings.UserID, AgentID: settings.AgentID}
}

func (settings Settings) interval() (time.Duration, error) {
	if settings.Interval == "0" {
		return 0, nil
	}
	value, err := time.ParseDuration(settings.Interval)
	if err != nil {
		return 0, fmt.Errorf("interval %q: %w", settings.Interval, err)
	}
	if value < 0 {
		return 0, errors.New("interval must not be negative")
	}
	return value, nil
}
