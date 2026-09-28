// Command memory-eval-diag prints per-question retrieval diagnostics: which
// source classes and item kinds the pack was built from, which of the dataset's
// own evidence turns it carried, and where the rest ended up.
//
// It answers what a report cannot. A report says only that a question's
// evidence did not reach the prompt; this says where it stopped -- cut by the
// pack, or never retrieved at all -- which are two different bugs with two
// different fixes. Everything here is retrieval-only: no answer or judge calls.
//
// Diagnosis reads the library the workspace already holds unless a derive pass
// is asked for, so the useful invocation names the derivation the run under
// investigation read:
//
//	go run ./cmd/memory-eval-diag -deploy ./deploy.yaml -env-file .env \
//	  -skip-derive -categories 1 -probe-items 300
//
// The pool probe is the part that needs explaining: the pack budget and the
// candidate depth retrieval reads are coupled (the provider reads three
// candidates per packed item), so "did retrieval ever have this turn" can only
// be asked by asking for a much larger pack. The probe therefore re-reads every
// question with -probe-items, and the rank it reports places the turn as if the
// pack were that large. The top of the ranking is stable; near-ties below it can
// still move, so a pool rank is a bound rather than a measurement.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	flowmemory "github.com/GizClaw/flowcraft/backends/memory"
	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/backends/memory/eval/internal/host"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func main() {
	dataset := flag.String("dataset", "locomo10.json", "LoCoMo JSON path")
	deployPath := flag.String("deploy", "", "build the assembly from a deploy.yaml instead of ad-hoc settings")
	envFile := flag.String("env-file", "", "load KEY=VALUE lines before building the deployment")
	samples := flag.Int("samples", 1, "number of conversations to diagnose")
	questions := flag.Int("questions", 0, "max questions per conversation (0 = all)")
	categoryFilter := flag.String("categories", "", "only diagnose these question categories (comma-separated, empty = all)")
	imageMode := flag.String("images", "native",
		"how turns with an image are loaded: annotation | native | both. It has to match the mode the "+
			"library was ingested with, because that decides the text an evidence turn is matched by")
	maxItems := flag.Int("max-items", 20, "context budget items")
	maxTokens := flag.Int("max-tokens", 4096, "context budget tokens")
	recentItems := flag.Int("recent-items", 20, "assembly recent.max_items (a deploy document overrides it)")
	top := flag.Int("top", 5, "top long-term hits printed per question")
	snippetRunes := flag.Int("snippet", 96, "runes of each printed snippet")
	probeItems := flag.Int("probe-items", 0,
		"re-read every question with this item budget, to tell evidence the pack cut from evidence retrieval never had (0 = off)")
	skipDerive := flag.Bool("skip-derive", false, "diagnose the derivation the workspace already holds: no ingest, no derive pass")
	flag.Parse()

	if *envFile != "" {
		must(host.LoadEnvFile(*envFile))
	}
	probe := *probeItems > *maxItems
	if *probeItems > 0 && !probe {
		must(fmt.Errorf("memory eval: -probe-items %d must exceed -max-items %d to say anything",
			*probeItems, *maxItems))
	}

	raw, err := os.ReadFile(*dataset)
	must(err)
	evalScope := eval.Scope{RuntimeID: "memories"}
	scenarios, stats, err := eval.LoadLoCoMo(raw, eval.LoaderOptions{
		Scope:   evalScope,
		Budget:  corememory.Budget{MaxItems: *maxItems, MaxTokens: *maxTokens},
		Images:  *imageMode,
		Samples: *samples,
	})
	must(err)
	if stats.SkippedAdversarial > 0 {
		fmt.Printf("dataset: %d adversarial (category 5) questions excluded by design\n",
			stats.SkippedAdversarial)
	}
	if *samples > 0 && len(scenarios) > *samples {
		scenarios = scenarios[:*samples]
	}
	if *questions > 0 {
		for index := range scenarios {
			if len(scenarios[index].Questions) > *questions {
				scenarios[index].Questions = scenarios[index].Questions[:*questions]
			}
		}
	}
	if strings.TrimSpace(*categoryFilter) != "" {
		allowed := map[int]struct{}{}
		for _, value := range strings.Split(*categoryFilter, ",") {
			number, convErr := strconv.Atoi(strings.TrimSpace(value))
			must(convErr)
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

	deployment := buildDeployment(*deployPath, *recentItems)
	defer deployment.Close()
	ctx := context.Background()
	if *skipDerive {
		fmt.Println("mode: reading the derivation the workspace already holds")
	} else {
		fmt.Printf("mode: ingesting %d scenario(s) and running one derivation pass\n", len(scenarios))
		for _, scenario := range scenarios {
			must(eval.Ingest(ctx, deployment.Memory, scenario))
		}
		deriveWithRetry(ctx, deployment.Memory)
	}
	fmt.Printf("deploy: %s\n", describeDeploy(*deployPath))
	fmt.Printf("recent: max_items=%d max_tokens=%d (effective, deploy document included)\n",
		deployment.Recent.MaxItems, deployment.Recent.MaxTokens)
	fmt.Printf("pack: max_items=%d max_tokens=%d\n", *maxItems, *maxTokens)
	if probe {
		fmt.Printf("probe: max_items=%d max_tokens=%d (pool ranks place a turn as if the pack were this large)\n",
			*probeItems, probeTokens(*maxTokens, *maxItems, *probeItems))
	} else {
		fmt.Println("probe: off, so a turn that missed the pack cannot be told apart from one retrieval never had")
	}
	// The derivation the answers come from is the store's, which is not the same
	// question as the policy this binary would derive with: a diagnostic over a
	// workspace left behind by an older policy describes that generation.
	library := host.LibraryState(ctx, deployment.Memory)
	fmt.Printf("library: %s\n", library)
	if *skipDerive && library.Underived > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d of %d conversations carry no derivation under policy digest %s:\n"+
				"  this run diagnoses the facts another policy generation wrote, so it measures that generation\n",
			library.Underived, library.Conversations, library.PolicyDigest)
	}
	fmt.Println()

	resolver := host.NewMessageProvenance(deployment.Memory.MessageStore(), corememory.Scope(evalScope))
	if len(scenarios) > 0 && !resolver.CarriesTurnIDs(ctx, scenarios[0].ConversationID, 32) {
		fmt.Println("identity: the store carries no dataset turn ids, so a turn is matched by committed text only --")
		fmt.Println("          a caption the stored turns do not carry (-images) hides every image turn from this diagnosis")
	}
	diagnoser := diagnoser{
		memory: deployment.Memory,
		scope:  corememory.Scope(evalScope),
		// One matcher for the whole run: the same fact is packed for many
		// questions, and resolving it is a store read per source.
		matcher: eval.NewMatcher(resolver),
		options: diagnosisOptions{
			maxItems: *maxItems, maxTokens: *maxTokens,
			probeItems: *probeItems, probeTokens: probeTokens(*maxTokens, *maxItems, *probeItems),
			top: *top, snippet: *snippetRunes,
		},
	}
	aggregate := newSummary()
	total := 0
	for _, scenario := range scenarios {
		total += len(scenario.Questions)
	}
	number := 0
	for _, scenario := range scenarios {
		index := newScenarioIndex(scenario)
		for _, question := range scenario.Questions {
			number++
			report := diagnoser.question(ctx, scenario, question, index)
			report.print(number, total)
			aggregate.add(report)
		}
	}
	aggregate.print(probe)
}

// diagnosisOptions carries the run's budget and print shape into one diagnosis.
type diagnosisOptions struct {
	maxItems    int
	maxTokens   int
	probeItems  int
	probeTokens int
	top         int
	snippet     int
}

// diagnoser holds what every question in a run shares: the assembly to read, the
// scope, the provenance resolver, its cache, and the budgets.
type diagnoser struct {
	memory  *flowmemory.Assembly
	scope   corememory.Scope
	matcher *eval.Matcher
	options diagnosisOptions
}

// placement says how far one dataset turn that carries a question's answer got.
type placement string

const (
	// packed: the configured budget carried it, which is what a report counts
	// as the evidence being recalled.
	packed placement = "packed"
	// poolOnly: the pack cut it, but the widened probe found it, so retrieval
	// had the turn and the budget lost it.
	poolOnly placement = "pool-only"
	// absent: the widened probe did not find it either, at any depth probed.
	absent placement = "absent"
	// missing: it did not reach the pack and no probe ran, so how far it got is
	// unknown.
	missing placement = "missing"
)

// evidencePlacement records where one evidence turn ended up, and what the item
// that carried it was.
type evidencePlacement struct {
	turnID    string
	placement placement
	// raw reports that the turn's own text reached the item's content. A false
	// value means the item resolved to the turn through its provenance only,
	// which for a fact is the whole commit it was extracted from: the model saw a
	// paraphrase of some turn in that commit, not the wording the dataset graded.
	raw   bool
	rank  int
	kind  corememory.ContextItemKind
	score float64
}

// topHit is one recalled long-term item as printed, with the dataset turns it
// stands for.
type topHit struct {
	rank    int
	class   corememory.ContextSourceClass
	kind    corememory.ContextItemKind
	score   float64
	turnIDs []string
	sources int
	text    string
}

// questionReport is everything one question's diagnosis found.
type questionReport struct {
	scenario  string
	category  int
	query     string
	items     int
	maxItems  int
	tokens    int
	truncated bool
	latency   time.Duration
	failed    bool
	sources   map[corememory.ContextSourceClass]int
	kinds     map[corememory.ContextItemKind]int
	evidence  []evidencePlacement
	hits      []topHit
	// probeItems is the size of the widened pack when the probe ran, so a
	// summary can show the probe really did widen.
	probeItems int
}

func (report questionReport) print(number, total int) {
	fmt.Printf("[%3d/%d] %s  cat=%d  %q\n", number, total, report.scenario, report.category, report.query)
	pack := fmt.Sprintf("items=%d/%d  tokens=%d  truncated=%v  latency=%s",
		report.items, report.maxItems, report.tokens, report.truncated, report.latency.Round(time.Millisecond))
	if report.failed {
		pack = "retrieval failed"
	}
	fmt.Printf("    pack      %s\n", pack)
	fmt.Printf("    sources   %s\n", renderCounts(report.sources))
	fmt.Printf("    kinds     %s\n", renderCounts(report.kinds))
	if len(report.evidence) == 0 {
		fmt.Println("    evidence  none recorded by the dataset for this question")
	} else {
		counts := map[placement]int{}
		for _, evidence := range report.evidence {
			counts[evidence.placement]++
		}
		fmt.Printf("    evidence  turns=%d  packed=%d  pool-only=%d  absent=%d  missing=%d\n",
			len(report.evidence), counts[packed], counts[poolOnly], counts[absent], counts[missing])
		for _, evidence := range report.evidence {
			// A turn that reached no pack at all has no rank and no item to
			// describe, so the line stops at the turn id.
			line := fmt.Sprintf("%s  %s", evidence.placement, evidence.turnID)
			if evidence.rank > 0 {
				line += fmt.Sprintf("  rank=%d", evidence.rank)
				line += fmt.Sprintf("  %s  %.3f", evidence.kind, evidence.score)
			}
			if evidence.placement == packed {
				if evidence.raw {
					line += "  raw"
				} else {
					line += "  provenance"
				}
			}
			fmt.Printf("        %s\n", line)
		}
	}
	if len(report.hits) > 0 {
		fmt.Println("    top long-term items (pack rank)")
		for _, hit := range report.hits {
			turns := renderTurns(hit.turnIDs)
			if turns == "" {
				turns = "-"
			}
			fmt.Printf("        rank=%d  %s  %s  %.3f  srcs=%d  %s  %q\n",
				hit.rank, hit.class, hit.kind, hit.score, hit.sources, turns, hit.text)
		}
	}
}

// renderTurns names the dataset turns an item stands for, capped: a fact
// carries the whole commit it was extracted from as provenance, so an uncapped
// list would print a session's worth of turn ids on one line.
func renderTurns(turns []string) string {
	const limit = 3
	if len(turns) == 0 {
		return ""
	}
	if len(turns) <= limit {
		return strings.Join(turns, ",")
	}
	return fmt.Sprintf("%s+%d", strings.Join(turns[:limit], ","), len(turns)-limit)
}

// summary aggregates a diag run: what the packs were made of, and where the
// evidence the dataset expects actually went.
type summary struct {
	questions  int
	items      int
	capacity   int
	tokens     int
	truncated  int
	failed     int
	sources    map[corememory.ContextSourceClass]int
	kinds      map[corememory.ContextItemKind]int
	placed     map[placement]int
	raw        int
	provenance int
	poolRanks  []int
	probePacks []int
	full       int
	withProof  int
}

func newSummary() summary {
	return summary{
		sources: map[corememory.ContextSourceClass]int{},
		kinds:   map[corememory.ContextItemKind]int{},
		placed:  map[placement]int{},
	}
}

func (totals *summary) add(report questionReport) {
	totals.questions++
	totals.items += report.items
	totals.capacity += report.maxItems
	totals.tokens += report.tokens
	if report.truncated {
		totals.truncated++
	}
	if report.failed {
		totals.failed++
	}
	if report.probeItems > 0 {
		totals.probePacks = append(totals.probePacks, report.probeItems)
	}
	for class, count := range report.sources {
		totals.sources[class] += count
	}
	for kind, count := range report.kinds {
		totals.kinds[kind] += count
	}
	if len(report.evidence) > 0 {
		totals.withProof++
	}
	complete := true
	for _, evidence := range report.evidence {
		totals.placed[evidence.placement]++
		switch evidence.placement {
		case packed:
			if evidence.raw {
				totals.raw++
			} else {
				totals.provenance++
			}
		case poolOnly:
			totals.poolRanks = append(totals.poolRanks, evidence.rank)
		}
		if evidence.placement != packed {
			complete = false
		}
	}
	if len(report.evidence) > 0 && complete {
		totals.full++
	}
}

func (totals summary) print(probed bool) {
	fmt.Println()
	fmt.Printf("summary: questions=%d  packed_items=%d/%d  truncated=%d  retrieval_failures=%d\n",
		totals.questions, totals.items, totals.capacity, totals.truncated, totals.failed)
	fmt.Printf("  sources   %s\n", renderCounts(totals.sources))
	fmt.Printf("  kinds     %s\n", renderCounts(totals.kinds))
	if totals.withProof == 0 {
		fmt.Println("  evidence  no question in this sample carries dataset evidence")
		return
	}
	fmt.Printf("  evidence  turns=%d  packed=%d  pool-only=%d  absent=%d  missing=%d\n",
		totals.placed[packed]+totals.placed[poolOnly]+totals.placed[absent]+totals.placed[missing],
		totals.placed[packed], totals.placed[poolOnly], totals.placed[absent], totals.placed[missing])
	fmt.Printf("            packed: raw=%d provenance=%d -- a provenance hit is a derived item whose sources cover\n",
		totals.raw, totals.provenance)
	fmt.Println("            the evidence turn's whole commit, so the model saw a paraphrase, not the graded wording")
	if len(totals.poolRanks) > 0 {
		sorted := append([]int(nil), totals.poolRanks...)
		sort.Ints(sorted)
		fmt.Printf("            pool-only rank: p50=%d p90=%d max=%d (the pack would have had to reach these ranks)\n",
			nearestRank(sorted, 0.5), nearestRank(sorted, 0.9), sorted[len(sorted)-1])
	}
	if len(totals.probePacks) > 0 {
		sorted := append([]int(nil), totals.probePacks...)
		sort.Ints(sorted)
		fmt.Printf("            probe packs: p50=%d max=%d items (what the widened read actually returned)\n",
			nearestRank(sorted, 0.5), sorted[len(sorted)-1])
	}
	fmt.Printf("  questions with every evidence turn packed: %d/%d\n", totals.full, totals.withProof)
	if !probed {
		fmt.Println("  note: run again with -probe-items to split 'missing' into 'cut by the pack' and 'never retrieved'")
	}
}

// question retrieves one question and places every evidence turn the dataset
// records for it: in the configured pack, in a widened pool only, or nowhere.
func (diagnoser diagnoser) question(
	ctx context.Context,
	scenario eval.Scenario,
	question eval.Question,
	index scenarioIndex,
) questionReport {
	options := diagnoser.options
	report := questionReport{
		scenario: scenario.Name, category: question.Category, query: question.Query,
		maxItems: options.maxItems,
		sources:  map[corememory.ContextSourceClass]int{},
		kinds:    map[corememory.ContextItemKind]int{},
	}
	request := corememory.ContextRequest{
		Scope: diagnoser.scope, ConversationID: scenario.ConversationID,
		// Dataset evidence ids deliberately stay out of the request: they grade
		// the result, they must not steer retrieval.
		Query:  question.Query,
		Budget: scenario.Budget,
	}
	if request.Budget.MaxItems <= 0 && request.Budget.MaxTokens <= 0 {
		request.Budget = corememory.Budget{MaxItems: options.maxItems, MaxTokens: options.maxTokens}
	}
	started := time.Now()
	result, err := diagnoser.memory.Context(ctx, request)
	report.latency = time.Since(started)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s %q: retrieval failed: %v\n", scenario.Name, question.Query, err)
		report.failed = true
		report.evidence = unplacedEvidence(question, missing)
		return report
	}
	report.items = len(result.Items)
	report.tokens = result.TokenCount
	report.truncated = result.Truncated
	for _, item := range result.Items {
		report.sources[item.SourceClass]++
		report.kinds[item.Kind]++
	}
	report.hits = diagnoser.topHits(result.Items, index)

	// widened is read only when the pack missed a turn, so a run without the
	// probe pays nothing for it.
	var widened []corememory.ContextItem
	probed := false
	for _, turnID := range question.Evidence {
		turnText := index.textByID[turnID]
		if turnText == "" {
			continue
		}
		if coverage := diagnoser.matcher.Cover(ctx, result.Items, turnID, turnText); coverage.Covered() {
			report.evidence = append(report.evidence, evidencePlacement{
				turnID: turnID, placement: packed, raw: coverage.ViaRaw, rank: coverage.Rank + 1,
				kind: result.Items[coverage.Rank].Kind, score: result.Items[coverage.Rank].Score,
			})
			continue
		}
		if options.probeItems <= options.maxItems {
			report.evidence = append(report.evidence, evidencePlacement{turnID: turnID, placement: missing})
			continue
		}
		if !probed {
			widened = diagnoser.widenedPack(ctx, request)
			probed = true
			report.probeItems = len(widened)
		}
		if coverage := diagnoser.matcher.Cover(ctx, widened, turnID, turnText); coverage.Covered() {
			report.evidence = append(report.evidence, evidencePlacement{
				turnID: turnID, placement: poolOnly, raw: coverage.ViaRaw, rank: coverage.Rank + 1,
				kind: widened[coverage.Rank].Kind, score: widened[coverage.Rank].Score,
			})
			continue
		}
		report.evidence = append(report.evidence, evidencePlacement{turnID: turnID, placement: absent})
	}
	return report
}

// widenedPack re-reads one question with a much larger budget. A failure is
// reported as an empty pack rather than as an error: the configured pack already
// answered the question this run is about.
func (diagnoser diagnoser) widenedPack(ctx context.Context, request corememory.ContextRequest) []corememory.ContextItem {
	widened := request
	widened.Budget = corememory.Budget{
		MaxItems:  diagnoser.options.probeItems,
		MaxTokens: diagnoser.options.probeTokens,
	}
	result, err := diagnoser.memory.Context(ctx, widened)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: probe for %q failed: %v\n", request.Query, err)
		return []corememory.ContextItem{}
	}
	return result.Items
}

// probeTokens keeps tokens per item constant, so widening the pack does not turn
// the token budget into the constraint the item budget is being asked about.
func probeTokens(maxTokens, maxItems, probeItems int) int {
	return maxTokens * probeItems / max(1, maxItems)
}

// topHits collects the long-term items of the pack, in pack order. The recent
// lane is left out because it scores a flat 1.0 and would fill every line
// without saying anything: the question here is what long-term retrieval
// contributed.
func (diagnoser diagnoser) topHits(items []corememory.ContextItem, index scenarioIndex) []topHit {
	var hits []topHit
	for position, item := range items {
		if item.SourceClass == corememory.ContextSourceRecent {
			continue
		}
		hits = append(hits, topHit{
			rank: position + 1, class: item.SourceClass, kind: item.Kind, score: item.Score,
			turnIDs: index.itemTurns(context.Background(), item, diagnoser.matcher),
			sources: len(item.Sources),
			text:    snippet(item.Content.Text(), diagnoser.options.snippet),
		})
		if len(hits) >= diagnoser.options.top {
			break
		}
	}
	return hits
}

// scenarioIndex maps the dataset's own turn ids to the text committed for them,
// so a recalled item can be attributed back to the evidence a report grades.
type scenarioIndex struct {
	textByID map[string]string
	idByText map[string]string
}

func newScenarioIndex(scenario eval.Scenario) scenarioIndex {
	index := scenarioIndex{
		textByID: make(map[string]string, len(scenario.Turns)),
		idByText: map[string]string{},
	}
	for _, turn := range scenario.Turns {
		for position, message := range turn.Messages {
			if position >= len(turn.DatasetIDs) {
				break
			}
			id := strings.TrimSpace(turn.DatasetIDs[position])
			text := strings.TrimSpace(message.Content.Text())
			if id == "" || text == "" {
				continue
			}
			if _, exists := index.textByID[id]; !exists {
				index.textByID[id] = text
			}
			if _, exists := index.idByText[text]; !exists {
				index.idByText[text] = id
			}
		}
	}
	return index
}

// itemTurns names the dataset turns an item stands for, by the ingest-side turn
// ids its canonical sources carry. It falls back to the loader's own conversion
// for a store that holds no ids: a raw message by its committed text, a derived
// item by the texts of the messages it resolves to.
func (index scenarioIndex) itemTurns(ctx context.Context, item corememory.ContextItem, matcher *eval.Matcher) []string {
	seen := map[string]struct{}{}
	var turns []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		turns = append(turns, id)
	}
	sources := matcher.Sources(ctx, item)
	tagged := false
	for _, source := range sources {
		if source.TurnID != "" {
			tagged = true
		}
		add(source.TurnID)
	}
	if tagged {
		return turns
	}
	if item.Kind == corememory.ContextRawMessage {
		add(index.idByText[strings.TrimSpace(item.Content.Text())])
		return turns
	}
	for _, source := range sources {
		add(index.idByText[strings.TrimSpace(source.Text)])
	}
	return turns
}

func unplacedEvidence(question eval.Question, state placement) []evidencePlacement {
	placements := make([]evidencePlacement, 0, len(question.Evidence))
	for _, turnID := range question.Evidence {
		placements = append(placements, evidencePlacement{turnID: turnID, placement: state})
	}
	return placements
}

func snippet(text string, limit int) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	runes := []rune(collapsed)
	if limit > 0 && len(runes) > limit {
		return string(runes[:limit]) + "..."
	}
	return collapsed
}

// deriveWithRetry runs the derivation pass the way the runner does: parallel
// derivation raises the transient provider error rate, and a pass is resumable,
// so a retry picks up where it stopped instead of redoing the work.
func deriveWithRetry(ctx context.Context, memory *flowmemory.Assembly) {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = memory.RunOnce(ctx); err == nil {
			return
		}
		fmt.Fprintf(os.Stderr, "derive attempt %d/3 failed: %v\n", attempt, err)
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 15 * time.Second)
		}
	}
	must(err)
}

func describeDeploy(path string) string {
	if strings.TrimSpace(path) == "" {
		return "ad-hoc (no models, throwaway workspace, so only recent + bm25 + entity)"
	}
	return path
}

// buildDeployment wires the assembly the document describes, or an ad-hoc one
// with no models when no document is given.
func buildDeployment(deployPath string, recentItems int) host.Deployment {
	deployment, err := host.Build(deployPath, recentItems)
	must(err)
	return deployment
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// renderCounts prints a count map in a stable order, so two runs of the same
// question diff cleanly.
func renderCounts[K comparable](counts map[K]int) string {
	keys := make([]K, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		if counts[key] == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%v=%d", key, counts[key]))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// nearestRank returns the value at the given percentile of an ascending series,
// which is the convention the reference harness's scoring uses.
func nearestRank(sorted []int, fraction float64) int {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(fraction*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
