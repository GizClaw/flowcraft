// Command memory-fact-density reads a stored workspace and reports how densely
// its derived facts sit: how many facts one source commit produced, how similar
// the facts of one commit are to each other, and how many of them survive the
// extraction budget's floor and merge.
//
// It is the reading the extraction budget asks for before anyone pays for a
// derivation. The budget's merge is a token-similarity rule whose threshold is a
// property of a corpus rather than of the module: too low and two claims about
// one subject fold into one, too high and a restatement keeps its double slot.
// This command measures both sides on facts that are already stored, for the
// symmetric measure (Jaccard) and for the one that asks whether the shorter fact
// is already inside the longer (containment), so the choice is made on data and
// the predicted facts/commit is known before the model is called.
//
// It reads only: no model call, no derivation, no write.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/internal/textsim"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

func main() {
	root := flag.String("workspace", "", "workspace root to read (required)")
	runtimeID := flag.String("runtime", "memories", "scope runtime_id")
	userID := flag.String("user", "", "scope user_id")
	agentID := flag.String("agent", "", "scope agent_id")
	minTokens := flag.Int("min-tokens", 4, "floor the projection applies (0 disables it)")
	jaccardCutoffs := flag.String("jaccard", "0.4,0.5,0.6,0.7,0.8,0.9", "Jaccard cutoffs to project")
	containmentCutoffs := flag.String("containment", "0.8,0.85,0.9,0.95,1", "containment cutoffs to project")
	topPairs := flag.Int("top-pairs", 12, "most similar same-commit pairs to print (0 disables)")
	snippet := flag.Int("snippet", 110, "runes of each printed fact")
	asJSON := flag.Bool("json", false, "print the reading as JSON instead of text")
	flag.Parse()
	if strings.TrimSpace(*root) == "" {
		fmt.Fprintln(os.Stderr, "memory-fact-density: -workspace is required")
		os.Exit(2)
	}

	reading, err := read(context.Background(), *root, corememory.Scope{
		RuntimeID: *runtimeID, UserID: *userID, AgentID: *agentID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory-fact-density: %v\n", err)
		os.Exit(1)
	}
	reading.Project(*minTokens, parseCutoffs(*jaccardCutoffs), parseCutoffs(*containmentCutoffs))
	if *asJSON {
		encoded, err := json.MarshalIndent(reading, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "memory-fact-density: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(encoded))
		return
	}
	reading.print(*topPairs, *snippet)
}

func parseCutoffs(value string) []float64 {
	cutoffs := make([]float64, 0, 8)
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parsed, err := strconv.ParseFloat(part, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memory-fact-density: cutoff %q: %v\n", part, err)
			os.Exit(2)
		}
		cutoffs = append(cutoffs, parsed)
	}
	sort.Float64s(cutoffs)
	return cutoffs
}

// commit is one extractor batch: the facts that share a source commit, which is
// what the budget selects over. A commit's key is its provenance, the source
// references the deriver stamped on every fact it read from that commit.
type commit struct {
	key   string
	facts []factview.Fact
}

// pair is one same-commit pair of facts with the two measures the budget could
// merge on.
type pair struct {
	jaccard     float64
	containment float64
	left        string
	right       string
}

// reading is the measurement: the stored facts grouped by the commit that
// produced them, every same-commit pair, and the projections the budget predicts.
type reading struct {
	Scope        corememory.Scope `json:"scope"`
	Facts        int              `json:"facts"`
	Commits      int              `json:"commits"`
	FactsPerTurn float64          `json:"facts_per_commit"`
	SizeRank     map[string]int   `json:"facts_per_commit_rank"`
	Pairs        int              `json:"pairs"`
	Histograms   map[string][]int `json:"histograms"`
	Projections  []projection     `json:"projections,omitempty"`
	Top          []pair           `json:"top_pairs,omitempty"`

	commits []commit
	pairs   []pair
}

// projection is what the budget would leave behind: the floor drops facts too
// thin to retrieve, the merge folds restatements of one claim.
type projection struct {
	Measure     string  `json:"measure"`
	Cutoff      float64 `json:"cutoff"`
	MinTokens   int     `json:"min_tokens"`
	Survivors   int     `json:"survivors"`
	KeptShare   float64 `json:"kept_share"`
	FactsPerTun float64 `json:"facts_per_commit"`
}

func read(ctx context.Context, root string, scope corememory.Scope) (*reading, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	ws, err := workspace.NewLocalWorkspace(root)
	if err != nil {
		return nil, err
	}
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		return nil, err
	}
	log, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		return nil, err
	}
	store, err := factview.NewFactStore(log, kv)
	if err != nil {
		return nil, err
	}
	facts, err := store.ListScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	result := &reading{Scope: scope, Facts: len(facts)}
	result.commits = groupByCommit(facts)
	result.Commits = len(result.commits)
	if result.Commits > 0 {
		result.FactsPerTurn = float64(result.Facts) / float64(result.Commits)
	}
	sizes := make([]int, 0, len(result.commits))
	for _, group := range result.commits {
		sizes = append(sizes, len(group.facts))
	}
	result.SizeRank = map[string]int{
		"p50": nearestRank(sizes, 0.5),
		"p90": nearestRank(sizes, 0.9),
		"p99": nearestRank(sizes, 0.99),
		"max": nearestRank(sizes, 1),
	}
	result.Histograms = map[string][]int{"jaccard": make([]int, 10), "containment": make([]int, 10)}
	for _, group := range result.commits {
		result.pairs = append(result.pairs, pairsOf(group.facts)...)
	}
	result.Pairs = len(result.pairs)
	for _, measured := range result.pairs {
		result.Histograms["jaccard"][bucket(measured.jaccard)]++
		result.Histograms["containment"][bucket(measured.containment)]++
	}
	return result, nil
}

// groupByCommit collects the facts of one scope into the batches the extractor
// derived them in. Facts of the same commit carry the same provenance, so the
// sorted reference list is the batch key. A fact without provenance stands alone
// instead of joining every other provenance-less fact.
func groupByCommit(facts []factview.Fact) []commit {
	index := make(map[string]int)
	groups := make([]commit, 0, len(facts))
	for _, fact := range facts {
		key := provenanceKey(fact)
		position, ok := index[key]
		if !ok {
			position = len(groups)
			index[key] = position
			groups = append(groups, commit{key: key})
		}
		groups[position].facts = append(groups[position].facts, fact)
	}
	// The largest batches first: they are where the budget has room to work.
	sort.SliceStable(groups, func(left, right int) bool {
		if len(groups[left].facts) != len(groups[right].facts) {
			return len(groups[left].facts) > len(groups[right].facts)
		}
		return groups[left].key < groups[right].key
	})
	return groups
}

func provenanceKey(fact factview.Fact) string {
	if len(fact.Provenance) == 0 {
		return "fact:" + fact.ID
	}
	refs := make([]string, 0, len(fact.Provenance))
	for _, ref := range fact.Provenance {
		refs = append(refs, string(ref.Kind)+"\x00"+ref.ID+"\x00"+ref.Revision)
	}
	sort.Strings(refs)
	return strings.Join(refs, "\x01")
}

func pairsOf(facts []factview.Fact) []pair {
	pairs := make([]pair, 0, len(facts)*(len(facts)-1)/2)
	for left := 0; left < len(facts); left++ {
		for right := left + 1; right < len(facts); right++ {
			if facts[left].CanonicalHash == facts[right].CanonicalHash {
				// The canonical identity already folds these: the budget never
				// sees them as two facts.
				continue
			}
			leftSet := textsim.Set(facts[left].Text)
			rightSet := textsim.Set(facts[right].Text)
			pairs = append(pairs, pair{
				jaccard:     textsim.Similarity(leftSet, rightSet),
				containment: textsim.ContainmentOf(leftSet, rightSet),
				left:        facts[left].Text,
				right:       facts[right].Text,
			})
		}
	}
	return pairs
}

// Project applies the budget's deterministic scan to every cutoff: the floor
// drops a fact whose token count is below minTokens, and the merge folds a fact
// into the first kept fact the measure puts at or above the cutoff (the longer
// text survives, as in the extractor).
func (reading *reading) Project(minTokens int, jaccardCutoffs, containmentCutoffs []float64) {
	reading.Projections = make([]projection, 0, len(jaccardCutoffs)+len(containmentCutoffs))
	for _, cutoff := range jaccardCutoffs {
		reading.Projections = append(reading.Projections,
			reading.project("jaccard", cutoff, minTokens, func(measured pair) float64 { return measured.jaccard }))
	}
	for _, cutoff := range containmentCutoffs {
		reading.Projections = append(reading.Projections,
			reading.project("containment", cutoff, minTokens, func(measured pair) float64 { return measured.containment }))
	}
}

func (reading *reading) project(measure string, cutoff float64, minTokens int, of func(pair) float64) projection {
	survivors := 0
	for _, group := range reading.commits {
		survivors += len(merge(group.facts, cutoff, minTokens, of))
	}
	result := projection{Measure: measure, Cutoff: cutoff, MinTokens: minTokens, Survivors: survivors}
	if reading.Facts > 0 {
		result.KeptShare = float64(survivors) / float64(reading.Facts)
	}
	if reading.Commits > 0 {
		result.FactsPerTun = float64(survivors) / float64(reading.Commits)
	}
	return result
}

// merge is the extractor's scan in miniature: the floor first, then each fact is
// compared against the facts already kept of its own commit.
func merge(facts []factview.Fact, cutoff float64, minTokens int, of func(pair) float64) []string {
	kept := make([]string, 0, len(facts))
	sets := make([]map[string]struct{}, 0, len(facts))
	for _, fact := range facts {
		current := textsim.Set(fact.Text)
		if minTokens > 0 && len(current) < minTokens {
			continue
		}
		merged := false
		for index := range kept {
			measured := of(pair{jaccard: textsim.Similarity(sets[index], current), containment: textsim.ContainmentOf(sets[index], current)})
			if measured < cutoff {
				continue
			}
			if len(fact.Text) > len(kept[index]) {
				kept[index] = fact.Text
				sets[index] = current
			}
			merged = true
			break
		}
		if merged {
			continue
		}
		kept = append(kept, fact.Text)
		sets = append(sets, current)
	}
	return kept
}

func (reading *reading) print(topPairs, snippet int) {
	fmt.Printf("workspace scope runtime=%q user=%q agent=%q\n",
		reading.Scope.RuntimeID, reading.Scope.UserID, reading.Scope.AgentID)
	fmt.Printf("stored facts %d over %d commits (%.2f facts/commit)\n",
		reading.Facts, reading.Commits, reading.FactsPerTurn)
	fmt.Printf("facts per commit: p50=%d p90=%d p99=%d max=%d\n",
		reading.SizeRank["p50"], reading.SizeRank["p90"], reading.SizeRank["p99"], reading.SizeRank["max"])
	fmt.Printf("same-commit pairs %d (canonical duplicates excluded)\n", reading.Pairs)
	for _, measure := range []string{"jaccard", "containment"} {
		fmt.Printf("%s histogram (0.0-0.1 ... 0.9-1.0): %v\n", measure, reading.Histograms[measure])
	}
	fmt.Println("projection (floor + merge, greedy, longer text survives):")
	for _, projected := range reading.Projections {
		fmt.Printf("  %-11s cutoff %.2f  survivors %5d (kept %.1f%%)  %.2f facts/commit\n",
			projected.Measure, projected.Cutoff, projected.Survivors, projected.KeptShare*100, projected.FactsPerTun)
	}
	if topPairs <= 0 {
		return
	}
	sorted := append([]pair(nil), reading.pairs...)
	sort.SliceStable(sorted, func(left, right int) bool {
		if sorted[left].jaccard != sorted[right].jaccard {
			return sorted[left].jaccard > sorted[right].jaccard
		}
		return sorted[left].containment > sorted[right].containment
	})
	if len(sorted) > topPairs {
		sorted = sorted[:topPairs]
	}
	fmt.Printf("most similar same-commit pairs (top %d):\n", len(sorted))
	for _, measured := range sorted {
		fmt.Printf("  j=%.2f c=%.2f\n    A %s\n    B %s\n",
			measured.jaccard, measured.containment, snippetOf(measured.left, snippet), snippetOf(measured.right, snippet))
	}
}

func snippetOf(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func bucket(value float64) int {
	index := int(value * 10)
	if index < 0 {
		return 0
	}
	if index > 9 {
		return 9
	}
	return index
}

// nearestRank returns the value at the given fraction of the sorted sample.
func nearestRank(values []int, fraction float64) int {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	index := int(fraction*float64(len(sorted))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
