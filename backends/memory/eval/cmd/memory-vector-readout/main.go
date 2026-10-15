// Command memory-vector-readout reads what the vector lane costs one retrieval
// request on a store a derivation already produced: the phases inside the lane
// (serving or materializing a table, embedding the query, scanning, sorting),
// the share of the lane's time an exact scan takes, and the resident footprint
// of the materialized tables.
//
// It never ingests, never derives, and never answers: it reads the workspace
// the deploy document names, so the reading describes that derivation. The
// projection it prints scales the measured per-entry scan cost to table sizes
// the run did not have, which is what the exact-versus-approximate decision
// needs: the scan is the term that grows with the corpus.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	flowmemory "github.com/GizClaw/flowcraft/backends/memory"
	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/backends/memory/eval/internal/host"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

type options struct {
	deploy      string
	envFile     string
	dataset     string
	images      string
	categories  string
	out         string
	samples     int
	questions   int
	maxItems    int
	maxTokens   int
	recentItems int
	workers     int
	cold        bool
	hot         bool
}

// question is one retrieval request: the probe never answers, so the query and
// the scope are the whole of it.
type question struct {
	scope          corememory.Scope
	conversationID string
	query          string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func parseFlags() options {
	var options options
	flag.StringVar(&options.deploy, "deploy", "", "deployment document naming the workspace to read (required)")
	flag.StringVar(&options.envFile, "env-file", "", "load KEY=VALUE lines before building the deployment")
	flag.StringVar(&options.dataset, "dataset", "locomo10.json", "LoCoMo JSON path supplying the questions")
	flag.StringVar(&options.images, "images", "native", "loader image mode the store was ingested with: annotation | native | both")
	flag.StringVar(&options.categories, "categories", "", "question categories to read (comma-separated, empty = all)")
	flag.StringVar(&options.out, "out", "", "write the reading to this JSON path as well")
	flag.IntVar(&options.samples, "samples", 0, "conversations to read (0 = all)")
	flag.IntVar(&options.questions, "questions", 0, "questions per conversation (0 = all)")
	flag.IntVar(&options.maxItems, "max-items", 30, "context budget items")
	flag.IntVar(&options.maxTokens, "max-tokens", 6144, "context budget tokens")
	flag.IntVar(&options.recentItems, "recent-items", 8, "assembly recent.max_items (a deploy document overrides it)")
	flag.IntVar(&options.workers, "workers", 4, "concurrent retrieval workers for the hot pass")
	flag.BoolVar(&options.cold, "cold", true, "run the cold pass: one question per conversation, sequential")
	flag.BoolVar(&options.hot, "hot", true, "run the hot pass: every question, concurrent")
	flag.Parse()
	return options
}

func run() error {
	options := parseFlags()
	if strings.TrimSpace(options.deploy) == "" {
		return errors.New("-deploy is required: the reading describes the workspace that document names")
	}
	if options.envFile != "" {
		if err := host.LoadEnvFile(options.envFile); err != nil {
			return fmt.Errorf("env file: %w", err)
		}
	}
	scenarios, err := loadScenarios(options)
	if err != nil {
		return err
	}
	deployment, err := host.Build(options.deploy, options.recentItems)
	if err != nil {
		return err
	}
	defer deployment.Close()
	memory := deployment.Memory
	if _, ok := memory.VectorLaneStats(); !ok {
		return errors.New("the deployment builds no vector lane: the deploy document configures no embedding model")
	}
	ctx := context.Background()
	budget := corememory.Budget{MaxItems: options.maxItems, MaxTokens: options.maxTokens}
	revision, modified := buildIdentity()
	library := host.LibraryState(ctx, memory)
	if library.Underived > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d of %d conversations carry no derivation under policy digest %s:\n"+
				"  this run reads the facts another policy generation wrote, so it measures that generation\n",
			library.Underived, library.Conversations, library.PolicyDigest)
	}
	items := flatten(scenarios)

	reading := reading{
		Tool: "memory-vector-readout", StartedAt: time.Now().UTC(),
		Revision: revision, VCSModified: modified,
		Deploy: options.deploy, Library: library,
		PolicyDigest: memory.PolicyDigest(), RetrievalSummary: memory.RetrievalSummary(),
		RecentMaxItems: deployment.Recent.MaxItems, RecentMaxTokens: deployment.Recent.MaxTokens,
		MaxItems: options.maxItems, MaxTokens: options.maxTokens,
		Scopes: distinctScopes(scenarios), Workers: options.workers,
	}
	reading.HeapBeforeBytes, reading.RSSBeforeBytes = liveMemory()
	fmt.Println("mode: reading the derivation the workspace already holds; nothing is ingested or derived")
	fmt.Printf("deploy: %s\n", options.deploy)
	fmt.Printf("library: %s\n", library)
	fmt.Printf("retrieval: %s\n", reading.RetrievalSummary)
	fmt.Printf("budget: max_items=%d max_tokens=%d (recent max_items=%d max_tokens=%d)\n",
		options.maxItems, options.maxTokens, reading.RecentMaxItems, reading.RecentMaxTokens)
	fmt.Printf("scopes: %d  questions: %d  workers: %d\n\n", reading.Scopes, len(items), options.workers)

	if options.cold {
		cold, err := coldPass(ctx, memory, scenarios, budget)
		if err != nil {
			return err
		}
		reading.Cold = cold
		renderCold(os.Stdout, cold)
	}
	if options.hot {
		hot, err := hotPass(ctx, memory, items, options.workers, budget)
		if err != nil {
			return err
		}
		reading.Hot = hot
		renderHot(os.Stdout, hot)
	}
	final, _ := memory.VectorLaneStats()
	reading.Footprint = final.Footprint
	reading.HeapAfterBytes, reading.RSSAfterBytes = liveMemory()
	renderFootprint(os.Stdout, reading)

	if options.out != "" {
		encoded, err := json.MarshalIndent(reading, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(options.out, append(encoded, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Printf("\nreading: %s\n", options.out)
	}
	return nil
}

// loadScenarios reads the questions the run will ask. The loader keeps the
// store's shape and never downloads a referenced image: the probe reads text
// the store already holds.
func loadScenarios(options options) ([]eval.Scenario, error) {
	raw, err := os.ReadFile(options.dataset)
	if err != nil {
		return nil, err
	}
	scenarios, stats, err := eval.LoadLoCoMo(raw, eval.LoaderOptions{
		Scope:  eval.Scope{RuntimeID: "memories"},
		Budget: corememory.Budget{MaxItems: options.maxItems, MaxTokens: options.maxTokens},
		Images: options.images,
		// The probe never ingests, so a referenced image is a URL in the text
		// shape and nothing fetches it; the run stays a read.
		ReferenceImages: true,
		Samples:         options.samples,
	})
	if err != nil {
		return nil, err
	}
	if stats.SkippedAdversarial > 0 {
		fmt.Printf("dataset: %d adversarial (category 5) questions excluded by design\n", stats.SkippedAdversarial)
	}
	if options.samples > 0 && len(scenarios) > options.samples {
		scenarios = scenarios[:options.samples]
	}
	if options.questions > 0 {
		for index := range scenarios {
			if len(scenarios[index].Questions) > options.questions {
				scenarios[index].Questions = scenarios[index].Questions[:options.questions]
			}
		}
	}
	if strings.TrimSpace(options.categories) != "" {
		allowed := make(map[int]struct{})
		for _, value := range strings.Split(options.categories, ",") {
			number, convErr := strconv.Atoi(strings.TrimSpace(value))
			if convErr != nil {
				return nil, fmt.Errorf("categories: %w", convErr)
			}
			allowed[number] = struct{}{}
		}
		for index := range scenarios {
			filtered := scenarios[index].Questions[:0]
			for _, question := range scenarios[index].Questions {
				if _, ok := allowed[question.Category]; ok {
					filtered = append(filtered, question)
				}
			}
			scenarios[index].Questions = filtered
		}
	}
	return scenarios, nil
}

func flatten(scenarios []eval.Scenario) []question {
	items := make([]question, 0)
	for _, scenario := range scenarios {
		scope := corememory.Scope{
			RuntimeID: scenario.Scope.RuntimeID, UserID: scenario.Scope.UserID, AgentID: scenario.Scope.AgentID,
		}
		for _, entry := range scenario.Questions {
			items = append(items, question{scope: scope, conversationID: scenario.ConversationID, query: entry.Query})
		}
	}
	return items
}

// distinctScopes counts the tenant scopes the questions touch; one scope
// carries every conversation of its tenant.
func distinctScopes(scenarios []eval.Scenario) int {
	scopes := make(map[corememory.Scope]struct{})
	for _, scenario := range scenarios {
		scopes[corememory.Scope{
			RuntimeID: scenario.Scope.RuntimeID, UserID: scenario.Scope.UserID, AgentID: scenario.Scope.AgentID,
		}] = struct{}{}
	}
	return len(scopes)
}

// coldPass asks one question per scope, sequentially: the first query of a
// scope pays for materializing its table, and that is the cost a warm
// deployment avoids and a fresh process pays. A scope holds every conversation
// of its tenant, so probing each conversation would report warm queries after
// the first as if they were cold.
func coldPass(
	ctx context.Context,
	memory *flowmemory.Assembly,
	scenarios []eval.Scenario,
	budget corememory.Budget,
) ([]coldReading, error) {
	readings := make([]coldReading, 0, len(scenarios))
	probed := make(map[corememory.Scope]struct{}, len(scenarios))
	for _, scenario := range scenarios {
		if len(scenario.Questions) == 0 {
			continue
		}
		scope := corememory.Scope{
			RuntimeID: scenario.Scope.RuntimeID, UserID: scenario.Scope.UserID, AgentID: scenario.Scope.AgentID,
		}
		if _, seen := probed[scope]; seen {
			continue
		}
		probed[scope] = struct{}{}
		// A collection before the query separates this scope's resident cost
		// from earlier garbage; it runs outside the measured latency.
		heapBefore, _ := liveMemory()
		before, _ := memory.VectorLaneStats()
		started := time.Now()
		if _, err := memory.Context(ctx, corememory.ContextRequest{
			Scope: scope, ConversationID: scenario.ConversationID,
			Query: scenario.Questions[0].Query, Budget: budget,
		}); err != nil {
			return nil, fmt.Errorf("cold pass %s: %w", scenario.ConversationID, err)
		}
		latency := time.Since(started)
		after, _ := memory.VectorLaneStats()
		heapAfter, _ := liveMemory()
		readings = append(readings, coldReading{
			ConversationID:   scenario.ConversationID,
			Latency:          latency,
			SnapshotLoad:     after.SnapshotLoad - before.SnapshotLoad,
			SnapshotMisses:   after.SnapshotMisses - before.SnapshotMisses,
			EntriesAdded:     after.Footprint.Entries - before.Footprint.Entries,
			VectorBytesAdded: after.Footprint.VectorBytes - before.Footprint.VectorBytes,
			HeapDeltaBytes:   deltaBytes(heapAfter, heapBefore),
		})
	}
	return readings, nil
}

// hotPass asks every question the run selected, with options.Workers workers,
// and attributes the pass by subtracting two readings taken around it.
func hotPass(
	ctx context.Context,
	memory *flowmemory.Assembly,
	items []question,
	workers int,
	budget corememory.Budget,
) (*hotReading, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if workers < 1 {
		workers = 1
	}
	stagesBefore := memory.ContextStageTotals()
	statsBefore, _ := memory.VectorLaneStats()
	latencies := make([]time.Duration, len(items))
	errs := make([]error, len(items))
	var counter atomic.Int64
	var group sync.WaitGroup
	started := time.Now()
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(counter.Add(1)) - 1
				if index >= len(items) {
					return
				}
				item := items[index]
				queryStarted := time.Now()
				_, err := memory.Context(ctx, corememory.ContextRequest{
					Scope: item.scope, ConversationID: item.conversationID,
					Query: item.query, Budget: budget,
				})
				latencies[index] = time.Since(queryStarted)
				errs[index] = err
			}
		}()
	}
	group.Wait()
	wall := time.Since(started)
	for index, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("hot pass: question %d of %d: %w", index+1, len(items), err)
		}
	}
	statsAfter, _ := memory.VectorLaneStats()
	phase := phaseDelta(statsBefore, statsAfter)
	stages := stageDeltas(stagesBefore, memory.ContextStageTotals())
	var contextSum time.Duration
	for _, latency := range latencies {
		contextSum += latency
	}
	entriesPerScope := 0
	if statsAfter.Footprint.Scopes > 0 {
		entriesPerScope = statsAfter.Footprint.Entries / statsAfter.Footprint.Scopes
	}
	reading := &hotReading{
		Questions:  len(items),
		Workers:    workers,
		Wall:       wall,
		Latency:    distribution(latencies),
		Stages:     stages,
		Vector:     phase,
		Shares:     computeShares(phase, stages, contextSum),
		Projection: projections(phase, entriesPerScope, statsAfter.Footprint.Dimensions),
	}
	if phase.Embeds > 0 {
		reading.MeanEmbed = phase.EmbedTotal / time.Duration(phase.Embeds)
	}
	if phase.EntriesScanned > 0 {
		reading.PerEntryScan = time.Duration(int64(phase.ScanTotal) / phase.EntriesScanned)
	}
	if phase.Queries > 0 {
		reading.ScannedPerQuery = float64(phase.EntriesScanned) / float64(phase.Queries)
		reading.ScoredPerQuery = float64(phase.EntriesScored) / float64(phase.Queries)
	}
	return reading, nil
}

// liveMemory reports the process's live heap after a collection and its
// resident set size; the RSS is zero where the kernel does not report one.
func liveMemory() (uint64, uint64) {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc, rssBytes()
}

func rssBytes() uint64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return value * 1024
	}
	return 0
}

// buildIdentity names the revision this binary was built from, so a reading
// can be compared only with readings of the same code.
func buildIdentity() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(no build info)", false
	}
	revision, modified := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		revision = "(no vcs stamp)"
	}
	return revision, modified
}

// sortedStages returns the stage names in a stable order, so two runs print
// their attribution the same way.
func sortedStages(stages map[string]stageReading) []string {
	names := make([]string, 0, len(stages))
	for name := range stages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
