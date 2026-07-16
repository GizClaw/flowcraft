package claw

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	recallops "github.com/GizClaw/flowcraft/memory/recall/ops"
	recallworkspace "github.com/GizClaw/flowcraft/memory/recall/store/workspace"
	"github.com/GizClaw/flowcraft/memory/retrieval/bbh"
	"github.com/GizClaw/flowcraft/sdk/model"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// MemoryConfig controls Claw's recall integration.
type MemoryConfig struct {
	Enabled   bool                  `json:"enabled,omitempty"`
	Scope     MemoryScopeConfig     `json:"scope,omitempty"`
	Write     MemoryWriteConfig     `json:"write,omitempty"`
	Extract   MemoryExtractConfig   `json:"extract,omitempty"`
	Layout    MemoryLayoutConfig    `json:"layout,omitempty"`
	Recall    MemoryRecallConfig    `json:"recall,omitempty"`
	Retrieval MemoryRetrievalConfig `json:"retrieval,omitempty"`
	Embedding MemoryEmbeddingConfig `json:"embedding,omitempty"`

	// Deprecated flat fields kept for old local configs.
	Backend          string     `json:"backend,omitempty"`
	RuntimeID        string     `json:"runtime_id,omitempty"`
	UserID           string     `json:"user_id,omitempty"`
	AgentID          string     `json:"agent_id,omitempty"`
	TopK             int        `json:"top_k,omitempty"`
	Graph            bool       `json:"graph,omitempty"`
	SaveConversation bool       `json:"save_conversation,omitempty"`
	BBH              bbh.Config `json:"bbh,omitempty"`
}

type MemoryScopeConfig struct {
	RuntimeID string `json:"runtime_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

type MemoryWriteConfig struct {
	SaveConversation bool                         `json:"save_conversation,omitempty"`
	Mode             string                       `json:"mode,omitempty"`
	Tier             string                       `json:"tier,omitempty"`
	BoardFacts       []MemoryWriteBoardFactConfig `json:"board_facts,omitempty"`
}

type MemoryWriteBoardFactConfig struct {
	BoardVar       string   `json:"board_var,omitempty"`
	Kind           string   `json:"kind,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	Predicate      string   `json:"predicate,omitempty"`
	Object         string   `json:"object,omitempty"`
	Entities       []string `json:"entities,omitempty"`
	RequiredPrefix string   `json:"required_prefix,omitempty"`
}

type MemoryExtractConfig struct {
	Enabled      bool                     `json:"enabled,omitempty"`
	Model        string                   `json:"model,omitempty"`
	Mode         recall.LLMExtractionMode `json:"mode,omitempty"`
	SystemPrompt string                   `json:"system_prompt,omitempty"`
	Temperature  *float64                 `json:"temperature,omitempty"`
	SchemaName   string                   `json:"schema_name,omitempty"`
	StageTimeout string                   `json:"stage_timeout,omitempty"`
}

// MemoryLayoutConfig describes semantic lanes the extractor should maintain
// inside the same recall storage scope.
type MemoryLayoutConfig struct {
	Lanes []MemoryLaneConfig `json:"lanes,omitempty"`
}

type MemoryLaneConfig struct {
	Name        string `json:"name,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Description string `json:"description,omitempty"`
	Extract     string `json:"extract,omitempty"`
	Recall      string `json:"recall,omitempty"`
}

type MemoryRecallConfig struct {
	Enabled        *bool                                `json:"enabled,omitempty"`
	TopK           int                                  `json:"top_k,omitempty"`
	GraphEnabled   bool                                 `json:"graph_enabled,omitempty"`
	IncludeRetired bool                                 `json:"include_retired,omitempty"`
	Query          MemoryRecallQueryConfig              `json:"query,omitempty"`
	Render         MemoryRecallRenderConfig             `json:"render,omitempty"`
	Inject         string                               `json:"inject,omitempty"`
	BoardVar       string                               `json:"board_var,omitempty"`
	Profiles       map[string]MemoryRecallProfileConfig `json:"profiles,omitempty"`
}

type MemoryRecallProfileConfig struct {
	Enabled        *bool                    `json:"enabled,omitempty"`
	TopK           int                      `json:"top_k,omitempty"`
	IncludeRetired bool                     `json:"include_retired,omitempty"`
	Query          MemoryRecallQueryConfig  `json:"query,omitempty"`
	Render         MemoryRecallRenderConfig `json:"render,omitempty"`
	Output         string                   `json:"output,omitempty"`
	BoardVar       string                   `json:"board_var,omitempty"`
}

type MemoryRecallQueryConfig struct {
	Text      string   `json:"text,omitempty"`
	Entities  []string `json:"entities,omitempty"`
	Subject   string   `json:"subject,omitempty"`
	Predicate string   `json:"predicate,omitempty"`
	Object    string   `json:"object,omitempty"`
	Kinds     []string `json:"kinds,omitempty"`
	GraphHops int      `json:"graph_hops,omitempty"`
	Lanes     []string `json:"lanes,omitempty"`
}

type MemoryRecallRenderConfig struct {
	Header     string `json:"header,omitempty"`
	ItemPrefix string `json:"item_prefix,omitempty"`
	MaxItems   int    `json:"max_items,omitempty"`
}

type MemoryRetrievalConfig struct {
	Backend string     `json:"backend,omitempty"`
	BBH     bbh.Config `json:"bbh,omitempty"`
}

type MemoryEmbeddingConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	Model   string `json:"model,omitempty"`
}

type memoryRuntime struct {
	mem       recall.Memory
	side      recall.SideEffectProcessor
	async     recall.AsyncSemanticProcessor
	opsCancel context.CancelFunc
	ops       *recallops.Supervisor
	backend   *recallworkspace.Backend
	scope     recall.Scope
	cfg       MemoryConfig
}

func (c *Claw) buildMemory(ctx context.Context) (*memoryRuntime, error) {
	if !c.cfg.Memory.Enabled {
		return nil, nil
	}

	memCfg := c.cfg.Memory.normalized(c.cfg.Agent.ID)
	opts := []recall.Option{recall.WithGraphEnabled(memCfg.Recall.GraphEnabled)}
	memoryWS := sdkworkspace.Sub(c.ws, c.cfg.Workspace.MemoryRoot)
	metadataWS := sdkworkspace.Sub(memoryWS, "metadata")
	backend, err := recallworkspace.New(metadataWS)
	if err != nil {
		return nil, err
	}
	scope := recall.Scope{
		RuntimeID: memCfg.Scope.RuntimeID,
		UserID:    memCfg.Scope.UserID,
		AgentID:   memCfg.Scope.AgentID,
	}
	opts = append(opts,
		recall.WithTemporalStore(backend.TemporalStore()),
		recall.WithEvidenceStore(backend.EvidenceStore()),
		recall.WithSideEffectOutbox(backend.SideEffectOutbox()),
		recall.WithAsyncSemanticQueue(backend.AsyncSemanticQueue()),
	)

	var bbhIndex *bbh.Index
	switch strings.TrimSpace(memCfg.Retrieval.Backend) {
	case "", "memory":
	case "bbh":
		localMemoryWS, ok := memoryWS.(*sdkworkspace.LocalWorkspace)
		if !ok {
			return nil, fmt.Errorf("claw: retrieval backend bbh requires a local workspace")
		}
		retrievalWS, err := localMemoryWS.Sub("retrieval")
		if err != nil {
			return nil, fmt.Errorf("claw: create bbh retrieval workspace: %w", err)
		}
		index, err := bbh.New(retrievalWS, bbh.WithConfig(memCfg.Retrieval.BBH))
		if err != nil {
			return nil, err
		}
		bbhIndex = index
		opts = append(opts, recall.WithRetrievalIndex(index))
	default:
		return nil, fmt.Errorf("claw: unsupported memory backend %q", memCfg.Retrieval.Backend)
	}

	if memCfg.Extract.Enabled && memCfg.Extract.Model != "" {
		extractor, err := c.model(ctx, memCfg.Extract.Model)
		if err != nil {
			return nil, err
		}
		extractOpts := []recall.LLMExtractorOption{}
		if memCfg.Extract.Mode != "" {
			extractOpts = append(extractOpts, recall.WithLLMExtractionMode(memCfg.Extract.Mode))
		}
		if prompt := memCfg.extractSystemPrompt(); prompt != "" {
			extractOpts = append(extractOpts, recall.WithLLMExtractorSystemPrompt(prompt))
		}
		if memCfg.Extract.Temperature != nil {
			extractOpts = append(extractOpts, recall.WithLLMExtractorTemperature(*memCfg.Extract.Temperature))
		}
		if memCfg.Extract.SchemaName != "" {
			extractOpts = append(extractOpts, recall.WithLLMExtractorSchemaName(memCfg.Extract.SchemaName))
		}
		stageTimeout, err := memCfg.Extract.stageTimeoutDuration()
		if err != nil {
			return nil, err
		}
		if stageTimeout > 0 {
			extractOpts = append(extractOpts, recall.WithLLMExtractorStageTimeout(stageTimeout))
		}
		opts = append(opts, recall.WithLLMExtractor(extractor, extractOpts...))
	}
	if memCfg.Embedding.Enabled && memCfg.Embedding.Model != "" {
		emb, err := c.embedderByName(ctx, memCfg.Embedding.Model)
		if err != nil {
			return nil, err
		}
		opts = append(opts, recall.WithEmbedder(newCachedEmbedder(emb)))
	}

	mem, err := recall.New(opts...)
	if err != nil {
		return nil, err
	}
	side, _ := recall.NewSideEffectProcessor(mem)
	async, _ := recall.NewAsyncSemanticProcessor(mem)
	if parseWriteMode(memCfg.Write.Mode) == recall.WriteModeAsyncSemantic && async == nil {
		return nil, fmt.Errorf("claw: async semantic memory processor is not available")
	}
	if bbhIndex != nil {
		if err := bbhIndex.WarmNamespace(ctx, recall.NamespaceFor(scope)); err != nil {
			_ = bbhIndex.Close()
			return nil, fmt.Errorf("claw: warm bbh retrieval namespace: %w", err)
		}
	}
	runtime := &memoryRuntime{
		mem:     mem,
		side:    side,
		async:   async,
		backend: backend,
		scope:   scope,
		cfg:     memCfg,
	}
	if async != nil {
		if err := runtime.startOps(ctx); err != nil {
			_ = runtime.close(ctx)
			return nil, err
		}
	}
	return runtime, nil
}

func (m *memoryRuntime) recallBoardVars(ctx context.Context, query string) (map[string]string, error) {
	if m == nil || m.mem == nil {
		return nil, nil
	}
	if !m.cfg.Recall.enabled() {
		return nil, nil
	}
	profiles := m.recallProfiles()
	if len(profiles) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(profiles))
	type recallJob struct {
		profile namedRecallProfile
		output  string
	}
	jobs := make([]recallJob, 0, len(profiles))
	for _, profile := range profiles {
		if !profile.enabled() {
			continue
		}
		output := profile.outputVar()
		if output == "" {
			continue
		}
		out[output] = ""
		jobs = append(jobs, recallJob{profile: profile, output: output})
	}
	if len(jobs) == 0 {
		return out, nil
	}
	type recallResult struct {
		output string
		value  string
		err    error
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan recallResult, len(jobs))
	for _, job := range jobs {
		job := job
		go func() {
			value, err := m.recallProfile(ctx, query, job.profile)
			results <- recallResult{output: job.output, value: value, err: err}
		}()
	}
	for range jobs {
		result := <-results
		if result.err != nil {
			cancel()
			return nil, result.err
		}
		if strings.TrimSpace(result.value) != "" {
			out[result.output] = result.value
		}
	}
	return out, nil
}

func (m *memoryRuntime) recallProfile(ctx context.Context, input string, profile namedRecallProfile) (string, error) {
	recallQuery := m.recallQuery(input, profile.config)
	if recallQueryEmpty(recallQuery) {
		return "", nil
	}
	hits, err := m.mem.Recall(ctx, m.scope, recallQuery)
	if err != nil {
		return "", err
	}
	hits = filterRecallHits(hits, profile.config.Query.Lanes)
	if len(hits) == 0 {
		return "", nil
	}
	return renderRecallHits(hits, profile.config.Render, profile.limit()), nil
}

func recallQueryEmpty(query recall.Query) bool {
	return strings.TrimSpace(query.Text) == "" &&
		len(query.Entities) == 0 &&
		strings.TrimSpace(query.Subject) == "" &&
		strings.TrimSpace(query.Predicate) == "" &&
		strings.TrimSpace(query.Object) == "" &&
		query.TimeRange.IsZero()
}

type namedRecallProfile struct {
	name   string
	config MemoryRecallProfileConfig
}

func (p namedRecallProfile) enabled() bool {
	return p.config.Enabled == nil || *p.config.Enabled
}

func (p namedRecallProfile) limit() int {
	if p.config.TopK > 0 {
		return p.config.TopK
	}
	return 5
}

func (p namedRecallProfile) outputVar() string {
	output := strings.TrimSpace(p.config.Output)
	if output == "" {
		output = strings.TrimSpace(p.config.BoardVar)
	}
	if output == "" {
		output = strings.TrimSpace(p.name)
	}
	if output == "default" {
		output = "memory_context"
	}
	return output
}

func (m *memoryRuntime) recallProfiles() []namedRecallProfile {
	if len(m.cfg.Recall.Profiles) == 0 {
		inject := strings.ToLower(strings.TrimSpace(m.cfg.Recall.Inject))
		if inject == "none" {
			return nil
		}
		profile := MemoryRecallProfileConfig{
			TopK:           m.cfg.Recall.TopK,
			IncludeRetired: m.cfg.Recall.IncludeRetired,
			Query:          m.cfg.Recall.Query,
			Render:         m.cfg.Recall.Render,
			Output:         m.cfg.Recall.BoardVar,
		}
		if strings.TrimSpace(profile.Output) == "" {
			profile.Output = "memory_context"
		}
		return []namedRecallProfile{{name: "default", config: profile}}
	}
	names := make([]string, 0, len(m.cfg.Recall.Profiles))
	for name := range m.cfg.Recall.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]namedRecallProfile, 0, len(names))
	for _, name := range names {
		profile := m.cfg.Recall.Profiles[name]
		if profile.TopK <= 0 {
			profile.TopK = m.cfg.Recall.TopK
		}
		if profile.TopK <= 0 {
			profile.TopK = 5
		}
		if !profile.IncludeRetired {
			profile.IncludeRetired = m.cfg.Recall.IncludeRetired
		}
		out = append(out, namedRecallProfile{name: name, config: profile})
	}
	return out
}

func (m *memoryRuntime) recallQuery(input string, profile MemoryRecallProfileConfig) recall.Query {
	cfg := profile.Query
	text := strings.TrimSpace(cfg.Text)
	switch text {
	case "", "input":
		text = input
	case "none":
		text = ""
	default:
		text = strings.ReplaceAll(text, "${input}", input)
	}
	limit := profile.TopK
	if limit <= 0 {
		limit = 5
	}
	return recall.Query{
		Text:           text,
		Entities:       append([]string(nil), cfg.Entities...),
		Limit:          limit,
		Subject:        cfg.Subject,
		Predicate:      cfg.Predicate,
		Object:         cfg.Object,
		Kinds:          recallKinds(cfg.Kinds),
		GraphHops:      cfg.GraphHops,
		IncludeRetired: profile.IncludeRetired,
	}
}

func recallKinds(kinds []string) []recall.FactKind {
	if len(kinds) == 0 {
		return nil
	}
	out := make([]recall.FactKind, 0, len(kinds))
	for _, kind := range kinds {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			continue
		}
		out = append(out, recall.FactKind(kind))
	}
	return out
}

func filterRecallHits(hits []recall.Hit, recallLanes []string) []recall.Hit {
	lanes := normalizedLaneSet(recallLanes)
	if len(lanes) == 0 {
		return hits
	}
	out := make([]recall.Hit, 0, len(hits))
	for _, hit := range hits {
		if laneMatches(hit.Fact.Content, lanes) {
			out = append(out, hit)
		}
	}
	return out
}

func normalizedLaneSet(lanes []string) map[string]struct{} {
	if len(lanes) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(lanes))
	for _, lane := range lanes {
		lane = strings.ToLower(strings.TrimSpace(lane))
		if lane == "" {
			continue
		}
		out[lane] = struct{}{}
	}
	return out
}

func laneMatches(content string, lanes map[string]struct{}) bool {
	prefix, _, ok := strings.Cut(strings.TrimSpace(content), ":")
	if !ok {
		return false
	}
	_, ok = lanes[strings.ToLower(strings.TrimSpace(prefix))]
	return ok
}

func renderRecallHits(hits []recall.Hit, render MemoryRecallRenderConfig, defaultLimit int) string {
	header := render.Header
	if strings.TrimSpace(header) == "" {
		header = "Relevant memory:"
	}
	prefix := render.ItemPrefix
	if prefix == "" {
		prefix = "- "
	}
	maxItems := render.MaxItems
	if maxItems <= 0 {
		maxItems = defaultLimit
	}
	var b strings.Builder
	if strings.TrimSpace(header) != "" {
		b.WriteString(header)
		b.WriteByte('\n')
	}
	for i, hit := range hits {
		if i >= maxItems {
			break
		}
		content := strings.TrimSpace(hit.Fact.Content)
		if content == "" {
			continue
		}
		fmt.Fprintf(&b, "%s%s\n", prefix, content)
	}
	return b.String()
}

func (m *memoryRuntime) saveTurn(ctx context.Context, contextID, userText string, assistant model.Message, boardVars map[string]any) error {
	if m == nil || m.mem == nil {
		return nil
	}
	now := time.Now()
	assistantText := assistant.Content()
	var turns []recall.TurnContext
	if m.cfg.Write.SaveConversation {
		turns = []recall.TurnContext{
			{
				ID:        contextID + ":user:" + now.Format("20060102150405.000000000"),
				SessionID: contextID,
				Role:      "user",
				Speaker:   "user",
				Time:      now,
				Text:      userText,
			},
			{
				ID:        contextID + ":assistant:" + now.Format("20060102150405.000000000"),
				SessionID: contextID,
				Role:      "assistant",
				Speaker:   "assistant",
				Time:      now,
				Text:      assistantText,
			},
		}
	}
	facts := m.boardFacts(boardVars, now)
	if len(turns) == 0 && len(facts) == 0 {
		return nil
	}
	_, err := m.mem.Save(ctx, m.scope, recall.SaveRequest{
		Facts:      facts,
		Turns:      turns,
		ObservedAt: now,
		Tier:       m.cfg.Write.Tier,
		Mode:       parseWriteMode(m.cfg.Write.Mode),
	})
	if err != nil {
		return err
	}
	return m.drainSideEffects(ctx)
}

func (m *memoryRuntime) boardFacts(vars map[string]any, observedAt time.Time) []recall.TemporalFact {
	if m == nil || len(m.cfg.Write.BoardFacts) == 0 || len(vars) == 0 {
		return nil
	}
	out := make([]recall.TemporalFact, 0, len(m.cfg.Write.BoardFacts))
	for _, cfg := range m.cfg.Write.BoardFacts {
		name := strings.TrimSpace(cfg.BoardVar)
		if name == "" {
			continue
		}
		content := boardVarString(vars[name])
		if content == "" {
			continue
		}
		if prefix := strings.TrimSpace(cfg.RequiredPrefix); prefix != "" {
			idx := strings.Index(content, prefix)
			if idx < 0 {
				continue
			}
			content = strings.TrimSpace(content[idx:])
		}
		kind := recall.FactKind(strings.TrimSpace(cfg.Kind))
		if !kind.IsValid() {
			kind = recall.FactNote
		}
		fact := recall.TemporalFact{
			Kind:       kind,
			Content:    content,
			Subject:    strings.TrimSpace(cfg.Subject),
			Predicate:  strings.TrimSpace(cfg.Predicate),
			Object:     strings.TrimSpace(cfg.Object),
			Entities:   nonEmptyStrings(cfg.Entities),
			ObservedAt: observedAt,
			ValidFrom:  &observedAt,
			Polarity:   recall.PolarityAffirmed,
			Modality:   recall.ModalityActual,
			Certainty:  recall.CertaintyExplicit,
			Confidence: 0.9,
		}
		out = append(out, fact)
	}
	return out
}

func boardVarString(v any) string {
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		return ""
	}
}

func nonEmptyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (m *memoryRuntime) drainSideEffects(ctx context.Context) error {
	if m == nil || m.side == nil {
		return nil
	}
	_, err := m.side.ProcessSideEffects(ctx, recall.SideEffectProcessOptions{
		WorkerID: "claw",
		Scope:    m.scope,
		Limit:    100,
	})
	return err
}

func (m *memoryRuntime) close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var errs []error
	if m.ops != nil {
		err := stopRecallOps(ctx, m.ops)
		if m.opsCancel != nil {
			m.opsCancel()
		}
		errs = append(errs, err)
		m.ops = nil
		m.opsCancel = nil
	}
	if m.mem != nil {
		err := m.drainPending(ctx)
		errs = append(errs, err)
	}
	if m.mem != nil {
		err := m.mem.Close()
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func stopRecallOps(ctx context.Context, ops *recallops.Supervisor) error {
	if ops == nil {
		return nil
	}
	return ops.GracefulStop(ctx)
}

func (m *memoryRuntime) startOps(parent context.Context) error {
	if m == nil || m.mem == nil || m.async == nil || m.scope.PartitionKey() == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	runner, err := recallops.NewRunner(
		m.mem,
		recallops.WithWorkerID("claw"),
		recallops.WithBatchSize(2),
		recallops.WithIntervals(time.Second, 5*time.Second),
	)
	if err != nil {
		cancel()
		return err
	}
	supervisor, err := recallops.Start(ctx, runner, recallops.Target{
		Scopes: []recall.Scope{m.scope},
	})
	if err != nil {
		cancel()
		return err
	}
	m.opsCancel = cancel
	m.ops = supervisor
	return nil
}

func (m *memoryRuntime) drainPending(ctx context.Context) error {
	if m == nil || m.scope.PartitionKey() == "" {
		return nil
	}
	iter := 0
	for {
		iter++
		var claimed int
		if m.side != nil {
			side, err := m.side.ProcessSideEffects(ctx, recall.SideEffectProcessOptions{
				WorkerID: "claw",
				Scope:    m.scope,
				Limit:    100,
			})
			if err != nil {
				return err
			}
			claimed += side.Claimed
		}
		if m.async != nil {
			async, err := m.async.ProcessAsyncSemantic(ctx, recall.AsyncSemanticProcessOptions{
				WorkerID: "claw",
				Scope:    m.scope,
				Limit:    100,
			})
			if err != nil {
				return err
			}
			claimed += async.Claimed
		}
		if claimed == 0 {
			break
		}
	}
	if obs, ok := m.mem.(recall.AsyncSemanticQueueObserver); ok && m.async != nil {
		stats, err := obs.AsyncSemanticQueueStats(ctx, m.scope)
		if err != nil {
			return err
		}
		if stats.Pending > 0 || stats.Leased > 0 || stats.ExpiredLeases > 0 {
			return fmt.Errorf("claw: async semantic queue not drained: pending=%d leased=%d expired_leases=%d failed=%d", stats.Pending, stats.Leased, stats.ExpiredLeases, stats.Failed)
		}
	}
	return nil
}

func (m MemoryConfig) normalized(agentID string) MemoryConfig {
	out := m
	if out.Scope.RuntimeID == "" {
		out.Scope.RuntimeID = firstNonEmpty(out.RuntimeID, "claw")
	}
	if out.Scope.UserID == "" {
		out.Scope.UserID = firstNonEmpty(out.UserID, "local")
	}
	if out.Scope.AgentID == "" {
		out.Scope.AgentID = firstNonEmpty(out.AgentID, agentID)
	}
	if out.Retrieval.Backend == "" {
		out.Retrieval.Backend = firstNonEmpty(out.Backend, "bbh")
	}
	if out.Retrieval.BBH == (bbh.Config{}) {
		out.Retrieval.BBH = out.BBH
	}
	if out.Recall.TopK <= 0 {
		if out.TopK > 0 {
			out.Recall.TopK = out.TopK
		} else {
			out.Recall.TopK = 5
		}
	}
	if out.Graph {
		out.Recall.GraphEnabled = true
	}
	if out.Recall.Enabled == nil {
		out.Recall.Enabled = boolPtr(true)
	}
	if out.SaveConversation {
		out.Write.SaveConversation = true
	}
	if strings.TrimSpace(out.Extract.StageTimeout) == "" {
		out.Extract.StageTimeout = "15s"
	}
	return out
}

func (c MemoryRecallConfig) enabled() bool {
	return c.Enabled != nil && *c.Enabled
}

func boolPtr(v bool) *bool {
	return &v
}

func (c MemoryExtractConfig) stageTimeoutDuration() (time.Duration, error) {
	raw := strings.TrimSpace(c.StageTimeout)
	if raw == "" {
		raw = "15s"
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("claw: parse memory.extract.stage_timeout %q: %w", c.StageTimeout, err)
	}
	return d, nil
}

func (m MemoryConfig) extractSystemPrompt() string {
	prompt := strings.TrimSpace(m.Extract.SystemPrompt)
	layout := m.Layout.extractPrompt()
	switch {
	case prompt == "":
		return layout
	case layout == "":
		return prompt
	default:
		return prompt + "\n\n" + layout
	}
}

func (l MemoryLayoutConfig) extractPrompt() string {
	if len(l.Lanes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Memory layout:\n")
	b.WriteString("Extract durable facts into these semantic lanes. Include the lane name at the beginning of each extracted fact content in the form \"<lane>: ...\" so recall can distinguish progress, preferences, questions, and other memory types.\n")
	for _, lane := range l.Lanes {
		name := strings.TrimSpace(lane.Name)
		if name == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(name)
		if kind := strings.TrimSpace(lane.Kind); kind != "" {
			b.WriteString(" (kind: ")
			b.WriteString(kind)
			b.WriteString(")")
		}
		if desc := strings.TrimSpace(lane.Description); desc != "" {
			b.WriteString(": ")
			b.WriteString(desc)
		}
		if extract := strings.TrimSpace(lane.Extract); extract != "" {
			b.WriteString(" Extract: ")
			b.WriteString(extract)
		}
		if recall := strings.TrimSpace(lane.Recall); recall != "" {
			b.WriteString(" Recall use: ")
			b.WriteString(recall)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func parseWriteMode(mode string) recall.WriteMode {
	switch strings.TrimSpace(mode) {
	case "async_semantic":
		return recall.WriteModeAsyncSemantic
	default:
		return recall.WriteModeSync
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
