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
	"github.com/GizClaw/flowcraft/backends/memory/profile"
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
	// defaultTriggerInterval is the period the derivation trigger polls the
	// work journal at. It is what makes a committed turn retrievable in
	// seconds instead of on the sweep's period, so it is much shorter than the
	// sweep's default; "0" turns the trigger off and leaves the sweep.
	defaultTriggerInterval = "1s"

	// maxSummaryChunkSize and maxSummaryGroupSize bound the summary
	// compactor's fan-in so one derivation cannot build an unbounded
	// hierarchy in a single step.
	maxSummaryChunkSize = 10_000
	maxSummaryGroupSize = 1_000
	// maxProfileTokens and maxProfileEntries bound what a deployment may make
	// its profile block. The block rides along with every request, so its
	// ceiling is a module bound and not a tuning: a block larger than this is
	// a second retrieval pool wearing the block's name.
	maxProfileTokens  = 2_048
	maxProfileEntries = 128
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
	Profile    ProfileSettings   `json:"profile,omitempty"`
	Chunk      ChunkSettings     `json:"chunk,omitempty"`
	Lanes      LanesSettings     `json:"lanes,omitempty"`
	Retrieval  RetrievalSettings `json:"retrieval,omitempty"`
	Projection string            `json:"projection,omitempty"`
	// ObservedAtAnchor renders each message's observed_at anchor as a text
	// prefix in the derivation source, so the extractor can resolve relative
	// time references against the time the message was actually observed.
	// Only a message that carries the anchor gets the prefix; one without it
	// keeps its untimestamped text rather than borrowing the commit time,
	// which over a bulk import would degrade to an "import day" label.
	//
	// It is on by default, and the pointer carries the tri-state a bool
	// cannot: an absent key derives with the anchor, an explicit
	// `observed_at_anchor: false` asks for the untimestamped reading a 1.4.0
	// workspace derived, and an explicit true is the default spelled out. It
	// changes what the extractor reads, so it is part of the derivation
	// policy digest.
	ObservedAtAnchor *bool `json:"observed_at_anchor,omitempty"`
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
	// Trigger tunes the fast path beside the sweep. It rides on the background
	// runner: interval "0" turns the whole runner off, trigger.interval "0"
	// turns off just the trigger.
	Trigger TriggerSettings `json:"trigger,omitempty"`
}

// TriggerSettings tunes the derivation trigger: the consumer that derives the
// conversations and scopes the work journal and the fact outbox recorded work
// for, instead of waiting for the next sweep.
type TriggerSettings struct {
	// Interval is the period the trigger polls at. Empty takes the default;
	// "0" turns the trigger off, which leaves the sweep as the only runner.
	Interval string `json:"interval,omitempty"`
	// LeaseTTL bounds one scope's lease, so a runner that dies mid-pass stops
	// holding the scope. Empty takes the worker default; runners sharing a
	// workspace have to agree on both this and their clocks for the lease to
	// mean anything across processes.
	LeaseTTL string `json:"lease_ttl,omitempty"`
	// Batch bounds how many records one poll reads from one source (the work
	// journal, the fact outbox). Empty takes the worker default.
	Batch int `json:"batch,omitempty"`
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
	// Temperature pins the sampling temperature of every generate call this
	// module makes: fact extraction, summaries, and reranking. Absent leaves
	// the provider's default, which is what a product host usually wants and
	// what every reading taken before 2026-10-20 was measured under.
	//
	// A measurement host pins it because an unpinned call is a sample. Samples
	// compound on the write side: two fresh derivations of one corpus under one
	// revision wrote 3424 and 3090 facts, and that difference alone moved
	// evidence recall by 1.4pp, which is larger than the effect a read-path arm
	// is asked to show. Pinning derivations makes the fact set a function of the
	// input instead of a draw.
	//
	// It changes what the extractor writes, so it is part of the derivation
	// policy digest: turning it on re-derives the scope. The embed block takes
	// no temperature, because an embedding is not sampled.
	Temperature *float64 `json:"temperature,omitempty"`
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
	// Quota names the batch cap (max_facts) in the extractor prompt and asks
	// every fact for its own priority, so a batch over the cap is truncated by
	// that priority instead of by position: it is how a host makes a dense turn
	// choose what to keep, instead of keeping whatever the model wrote first.
	//
	// Off by default. It changes what the extractor writes, so it is part of the
	// derivation policy digest, and switching it on re-derives the scope.
	// fact.max_facts is the lever that makes it bite: measured over this
	// project's own conversations, one commit of the benchmark corpus derives
	// 12.59 facts on average with a 99th percentile of 27, so the default cap of
	// 64 truncates nothing and the quota's only effect there is the prompt.
	Quota bool `json:"quota,omitempty"`
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
	// VectorCache bounds the vector lane's resident tables. Absent, the lane
	// keeps every scope's table it has read, which is what it always did; a
	// host that serves many scopes bounds it, because one scope's table is
	// its embedding model's width times four bytes per entry (measured: a
	// 9324-entry, 2048-dimension table holds 72.8MB of vector payload).
	//
	// It is part of the retrieval summary because it is what a footprint
	// reading has to be read against, and because two runs of the same
	// deployment that differ in it are not the same configuration.
	VectorCache *VectorCacheSettings `json:"vector_cache,omitempty"`
}

// ProfileSettings configures the resident profile block: the bounded lines
// about a conversation's participants that ride along with every context
// request instead of being retrieved by it.
//
// It is off by default, and that default is a statement rather than an
// omission: the block is served on every request whatever the query says, so
// turning it on changes what every answer reads. The vocabulary that fills the
// pass's half is the module's measured default (profile.DefaultClasses); a
// deployment that needs another one configures the block store in Go, and a
// deployment whose corpus has no facts in this shape leaves the block off.
type ProfileSettings struct {
	// Enabled serves the block and lets the maintenance pass fill its
	// distilled half. Off means the read path serves nothing from a block
	// whatever the store holds: a block nobody serves is cheap, a block
	// served by accident is not.
	Enabled bool `json:"enabled,omitempty"`
	// MaxTokens bounds one served block in the module's cost model
	// (pack.EstimatedTokens), default profile.DefaultMaxTokens.
	MaxTokens int `json:"max_tokens,omitempty"`
	// MaxEntries bounds how many lines one served block holds, default
	// profile.DefaultMaxEntries.
	MaxEntries int `json:"max_entries,omitempty"`
}

// VectorCacheSettings bounds what the vector lane keeps resident. Both fields
// are a count of what may be held: zero is no bound, and a bound never changes
// what a query sees, only what the lane keeps after reading it. One held table
// is one shard's, so the bounds count shards, not tenants.
type VectorCacheSettings struct {
	// MaxCachedShards is how many shard tables may be resident at once. The
	// lane drops the one it read least recently to make room.
	MaxCachedShards int `json:"max_cached_shards,omitempty"`
	// MaxEntriesPerShard is the largest table the lane keeps. A shard whose
	// table is larger is read and scanned per query instead of held, trading
	// the memory for the read.
	MaxEntriesPerShard int `json:"max_entries_per_shard,omitempty"`
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
	if strings.TrimSpace(settings.Derive.Trigger.Interval) == "" {
		settings.Derive.Trigger.Interval = defaultTriggerInterval
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
	if settings.Profile.MaxTokens == 0 {
		settings.Profile.MaxTokens = profile.DefaultMaxTokens
	}
	if settings.Profile.MaxEntries == 0 {
		settings.Profile.MaxEntries = profile.DefaultMaxEntries
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

// anchorObservedAt resolves the tri-state observed_at_anchor setting: only an
// explicit false opts out, so a document that does not name the key -- or names
// it true -- derives with the anchor. Every consumer reads the switch through
// this, so "absent means on" is stated once instead of at each call site.
func (settings Settings) anchorObservedAt() bool {
	return settings.ObservedAtAnchor == nil || *settings.ObservedAtAnchor
}

// retrievalSummary renders the retrieval-side configuration -- lane weights and
// fusion, the bm25 parameters, source quotes and rerank -- as one stable JSON
// object with defaults applied. The derivation policy digest covers what
// memorization writes; it does not cover what retrieval reads it with, so a
// host stamps a run with this value to keep two retrieval configurations from
// sharing a fingerprint.
//
// The object is marshalled from the settings types themselves, so a knob added
// to Lanes, BM25 or Retrieval appears here without a second edit. bm25.k1/b
// stay out of the object while they are zero: a zero means "the lane's
// built-in default" (k1 = 1.2, b = 0.75), not a value this configuration
// chose.
func (settings Settings) retrievalSummary() (string, error) {
	// The block is named only while it is served: a deployment that leaves it
	// off serves exactly what it served before the block existed, so its
	// footprint has to stay the one it had -- two runs may share a fingerprint
	// only while they share what a request reads.
	var served *ProfileSettings
	if settings.Profile.Enabled {
		served = &settings.Profile
	}
	encoded, err := json.Marshal(struct {
		Lanes     LanesSettings     `json:"lanes"`
		BM25      BM25Settings      `json:"bm25"`
		Retrieval RetrievalSettings `json:"retrieval"`
		Profile   *ProfileSettings  `json:"profile,omitempty"`
	}{Lanes: settings.Lanes, BM25: settings.BM25, Retrieval: settings.Retrieval, Profile: served})
	if err != nil {
		return "", fmt.Errorf("retrieval summary: %w", err)
	}
	return string(encoded), nil
}

// Validate rejects invalid settings after defaults are applied.
func (settings Settings) Validate() error {
	if err := settings.Storage.validate(); err != nil {
		return err
	}
	if err := settings.Generate.validate("generate"); err != nil {
		return err
	}
	if settings.Embed.Temperature != nil {
		return errors.New("embed.temperature is not supported: an embedding is not sampled")
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
	if cache := settings.Lanes.VectorCache; cache != nil {
		for name, value := range map[string]int{
			"max_cached_shards":     cache.MaxCachedShards,
			"max_entries_per_shard": cache.MaxEntriesPerShard,
		} {
			if value < 0 {
				return fmt.Errorf("lanes.vector_cache.%s must not be negative", name)
			}
		}
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
	if _, err := settings.triggerInterval(); err != nil {
		return err
	}
	if _, err := settings.triggerLeaseTTL(); err != nil {
		return err
	}
	if settings.Derive.Trigger.Batch < 0 {
		return errors.New("derive.trigger.batch must not be negative")
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
	// The block's bound holds whether or not it is served: a block store
	// written under one setting and read under another must not be the place
	// the ceiling moves, and a value no request could carry is a typo rather
	// than an intent.
	if settings.Profile.MaxTokens <= 0 || settings.Profile.MaxTokens > maxProfileTokens {
		return fmt.Errorf("profile.max_tokens must be positive and must not exceed %d", maxProfileTokens)
	}
	if settings.Profile.MaxEntries <= 0 || settings.Profile.MaxEntries > maxProfileEntries {
		return fmt.Errorf("profile.max_entries must be positive and must not exceed %d", maxProfileEntries)
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
	if settings.Temperature != nil {
		if value := *settings.Temperature; math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 2 {
			return fmt.Errorf("%s.temperature %v is outside [0,2]", name, value)
		}
		if settings.Provider == "" && settings.Name == "" {
			return fmt.Errorf("%s.temperature requires provider and name", name)
		}
	}
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
	return period("interval", settings.Interval)
}

// triggerInterval is the trigger's poll period. "0" turns the trigger off,
// which leaves the sweep as the only runner.
func (settings Settings) triggerInterval() (time.Duration, error) {
	return period("derive.trigger.interval", settings.Derive.Trigger.Interval)
}

// triggerLeaseTTL is how long one scope's claim stays one without a renewal. A
// ttl shorter than the work it bounds lets another runner take the scope while
// the holder is still deriving, which costs duplicate work rather than
// correctness, but a runner renews as it goes: the ttl has to outlast one
// conversation's pass, not a scope's whole drain.
func (settings Settings) triggerLeaseTTL() (time.Duration, error) {
	if strings.TrimSpace(settings.Derive.Trigger.LeaseTTL) == "" {
		return 0, nil
	}
	value, err := period("derive.trigger.lease_ttl", settings.Derive.Trigger.LeaseTTL)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, errors.New("derive.trigger.lease_ttl must be positive")
	}
	return value, nil
}

// period parses one configured duration. "0" is a period of zero, which every
// caller reads as "off"; anything else has to parse and must not be negative.
func period(name, value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "0" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", name, value, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return parsed, nil
}
