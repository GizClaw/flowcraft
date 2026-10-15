// Command memory-fact-diff compares the fact sets two derivations of one corpus
// wrote, batch by batch, and names what only one of them holds.
//
// It answers the question the extraction budget raises once its cap is switched
// on: the cap keeps a batch's most important facts instead of its first ones, so
// the reading has to say which claims went away and what stands where they stood.
// Two derivations never agree word for word -- the extractor samples -- so facts
// are paired by the shared token measure, and a fact whose best pairing stays
// under the threshold counts as a difference.
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
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/internal/textsim"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

func main() {
	baselineRoot := flag.String("baseline", "", "workspace root the first derivation was stored in (required)")
	candidateRoot := flag.String("candidate", "", "workspace root the second derivation was stored in (required)")
	runtimeID := flag.String("runtime", "memories", "scope runtime_id")
	userID := flag.String("user", "", "scope user_id")
	agentID := flag.String("agent", "", "scope agent_id")
	match := flag.Float64("match", 0.7, "token similarity at or above which two facts are the same claim")
	snippet := flag.Int("snippet", 160, "runes of each printed fact")
	cap := flag.Int("cap", 0, "batch size above which a batch is marked capped (0 marks none)")
	limit := flag.Int("limit", 0, "batches to print, largest first (0 prints every batch that differs)")
	asJSON := flag.Bool("json", false, "print the reading as JSON instead of text")
	flag.Parse()
	if strings.TrimSpace(*baselineRoot) == "" || strings.TrimSpace(*candidateRoot) == "" {
		fmt.Fprintln(os.Stderr, "memory-fact-diff: -baseline and -candidate are both required")
		os.Exit(2)
	}

	reading, err := compare(context.Background(), *baselineRoot, *candidateRoot, corememory.Scope{
		RuntimeID: *runtimeID, UserID: *userID, AgentID: *agentID,
	}, *match)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory-fact-diff: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		encoded, err := json.MarshalIndent(reading, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "memory-fact-diff: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(encoded))
		return
	}
	reading.print(*snippet, *cap, *limit)
}

// side measures one derivation: how many facts it wrote, how they spread over
// the batches the extractor ran, and how big those batches got.
type side struct {
	Facts   int            `json:"facts"`
	Batches int            `json:"batches"`
	Rank    map[string]int `json:"facts_per_batch_rank"`

	sizes []int
}

// difference is one fact only one side holds, with the closest fact the other
// side wrote and that pairing's similarity -- the number that decides whether
// the fact really went away or only changed wording.
type difference struct {
	Text    string  `json:"text"`
	Best    float64 `json:"best_similarity"`
	Closest string  `json:"closest,omitempty"`
}

// batch is one source commit's comparison: how many facts each side wrote for
// it, which of them paired up, and what is left over on either side.
type batch struct {
	Key       string       `json:"key"`
	Baseline  int          `json:"baseline"`
	Candidate int          `json:"candidate"`
	Matched   int          `json:"matched"`
	Removed   []difference `json:"removed,omitempty"`
	Added     []difference `json:"added,omitempty"`
}

// reading is the whole comparison.
type reading struct {
	Scope      corememory.Scope `json:"scope"`
	Threshold  float64          `json:"threshold"`
	Baseline   side             `json:"baseline"`
	Candidate  side             `json:"candidate"`
	Matched    int              `json:"matched"`
	Removed    int              `json:"removed"`
	Added      int              `json:"added"`
	OnlyFirst  int              `json:"batches_only_in_baseline"`
	OnlySecond int              `json:"batches_only_in_candidate"`
	Batches    []batch          `json:"batches"`
}

func compare(ctx context.Context, baselineRoot, candidateRoot string, scope corememory.Scope, threshold float64) (*reading, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if threshold <= 0 || threshold > 1 {
		return nil, fmt.Errorf("match threshold %v is outside (0, 1]", threshold)
	}
	baseline, err := readFacts(ctx, baselineRoot, scope)
	if err != nil {
		return nil, fmt.Errorf("baseline: %w", err)
	}
	candidate, err := readFacts(ctx, candidateRoot, scope)
	if err != nil {
		return nil, fmt.Errorf("candidate: %w", err)
	}
	result := &reading{Scope: scope, Threshold: threshold}
	first := groupByCommit(baseline)
	second := groupByCommit(candidate)
	result.Baseline = describe(first)
	result.Candidate = describe(second)

	secondByKey := make(map[string]commit, len(second))
	for _, group := range second {
		secondByKey[group.key] = group
	}
	seen := make(map[string]struct{}, len(first))
	for _, group := range first {
		seen[group.key] = struct{}{}
		counterpart, ok := secondByKey[group.key]
		if !ok {
			result.OnlyFirst++
			result.Batches = append(result.Batches, gone(group))
			continue
		}
		measured := compareBatch(group, counterpart, threshold)
		result.Matched += measured.Matched
		result.Removed += len(measured.Removed)
		result.Added += len(measured.Added)
		if len(measured.Removed) > 0 || len(measured.Added) > 0 {
			result.Batches = append(result.Batches, measured)
		}
	}
	for _, group := range second {
		if _, ok := seen[group.key]; !ok {
			result.OnlySecond++
			result.Batches = append(result.Batches, appeared(group))
		}
	}
	// The batches the budget had room in come first: those are the ones worth
	// reading, and a batch that merely lost facts to sampling noise sorts last.
	sort.SliceStable(result.Batches, func(left, right int) bool {
		if result.Batches[left].Baseline != result.Batches[right].Baseline {
			return result.Batches[left].Baseline > result.Batches[right].Baseline
		}
		return result.Batches[left].Key < result.Batches[right].Key
	})
	return result, nil
}

// describe measures one side: its size, how many batches that size spread over,
// and the batch sizes' percentiles.
func describe(groups []commit) side {
	sizes := make([]int, 0, len(groups))
	facts := 0
	for _, group := range groups {
		sizes = append(sizes, len(group.facts))
		facts += len(group.facts)
	}
	return side{
		Facts:   facts,
		Batches: len(groups),
		sizes:   sizes,
		Rank: map[string]int{
			"p50": nearestRank(sizes, 0.5),
			"p90": nearestRank(sizes, 0.9),
			"p99": nearestRank(sizes, 0.99),
			"max": nearestRank(sizes, 1),
		},
	}
}

// capped measures a cap on batch size as it would fall on this side's batches:
// how many batches are over it, and how many facts that leaves out. It is the
// size of the lever -- what the cap could select away -- not a prediction, since
// the extractor writes a different batch once the prompt names the cap.
func (measured side) capped(cap int) (batches, facts int) {
	if cap <= 0 {
		return 0, 0
	}
	for _, size := range measured.sizes {
		if size > cap {
			batches++
			facts += size - cap
		}
	}
	return batches, facts
}

// gone is a batch the candidate does not have at all: everything the baseline
// wrote for that commit is a difference.
func gone(group commit) batch {
	result := batch{Key: group.Key() + "\t(absent)", Baseline: len(group.facts)}
	for _, fact := range group.facts {
		result.Removed = append(result.Removed, difference{Text: fact.Text})
	}
	return result
}

// appeared is a batch only the candidate has.
func appeared(group commit) batch {
	result := batch{Key: group.key + "\t(added)", Candidate: len(group.facts)}
	for _, fact := range group.facts {
		result.Added = append(result.Added, difference{Text: fact.Text})
	}
	return result
}

// compareBatch pairs the facts of one source commit across the two derivations
// and reports what each side holds alone. Pairing is one-to-one and greedy over
// the most similar pairs first: a batch's facts are distinct claims, so the
// reading must not let one fact stand in for two.
func compareBatch(baseline, candidate commit, threshold float64) batch {
	result := batch{Key: baseline.Key(), Baseline: len(baseline.facts), Candidate: len(candidate.facts)}
	leftSets := setsOf(baseline.facts)
	rightSets := setsOf(candidate.facts)
	type pairing struct {
		similarity float64
		left       int
		right      int
	}
	pairings := make([]pairing, 0, len(baseline.facts)*len(candidate.facts))
	for left := range baseline.facts {
		for right := range candidate.facts {
			pairings = append(pairings, pairing{
				similarity: similarityOf(baseline.facts[left], candidate.facts[right], leftSets[left], rightSets[right]),
				left:       left,
				right:      right,
			})
		}
	}
	sort.SliceStable(pairings, func(first, second int) bool {
		if pairings[first].similarity != pairings[second].similarity {
			return pairings[first].similarity > pairings[second].similarity
		}
		if pairings[first].left != pairings[second].left {
			return pairings[first].left < pairings[second].left
		}
		return pairings[first].right < pairings[second].right
	})
	usedLeft := make([]bool, len(baseline.facts))
	usedRight := make([]bool, len(candidate.facts))
	for _, measured := range pairings {
		if measured.similarity < threshold {
			break
		}
		if usedLeft[measured.left] || usedRight[measured.right] {
			continue
		}
		usedLeft[measured.left] = true
		usedRight[measured.right] = true
		result.Matched++
	}
	for index, fact := range baseline.facts {
		if usedLeft[index] {
			continue
		}
		best, counterpart := closest(fact, leftSets[index], candidate.facts, rightSets)
		result.Removed = append(result.Removed, difference{Text: fact.Text, Best: best, Closest: counterpart})
	}
	for index, fact := range candidate.facts {
		if usedRight[index] {
			continue
		}
		best, counterpart := closest(fact, rightSets[index], baseline.facts, leftSets)
		result.Added = append(result.Added, difference{Text: fact.Text, Best: best, Closest: counterpart})
	}
	return result
}

// closest finds the fact of the other side nearest to this one and how near it
// is, so a difference can be printed beside the claim that stands where it did.
func closest(fact factview.Fact, set map[string]struct{}, others []factview.Fact, otherSets []map[string]struct{}) (float64, string) {
	best := 0.0
	counterpart := ""
	for index, other := range others {
		similarity := similarityOf(fact, other, set, otherSets[index])
		if similarity > best {
			best = similarity
			counterpart = other.Text
		}
	}
	return best, counterpart
}

func setsOf(facts []factview.Fact) []map[string]struct{} {
	sets := make([]map[string]struct{}, len(facts))
	for index, fact := range facts {
		sets[index] = textsim.Set(fact.Text)
	}
	return sets
}

// similarityOf is the shared measure taken the generous way: two texts are the
// same claim when they are alike (Jaccard) or when one of them already says what
// the other says (containment). A restatement that adds a clause is one claim
// spending two slots in the batch, and the reading must treat it as one.
func similarityOf(left, right factview.Fact, leftSet, rightSet map[string]struct{}) float64 {
	if left.CanonicalHash != "" && left.CanonicalHash == right.CanonicalHash {
		return 1
	}
	jaccard := textsim.Similarity(leftSet, rightSet)
	containment := textsim.ContainmentOf(leftSet, rightSet)
	if containment > jaccard {
		return containment
	}
	return jaccard
}

// commit is one extractor batch: the facts that share one source commit. The key
// names the source messages, so the two derivations' batches join on it.
type commit struct {
	key   string
	facts []factview.Fact
}

// Key names the batch for a human: the messages it was derived from, shortened.
func (group commit) Key() string { return shorten(group.key, 96) }

// groupByCommit collects facts into the batches the extractor derived them in.
// Facts of one commit carry the same provenance, so the sorted reference list is
// the batch key; a fact without provenance stands alone.
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

func readFacts(ctx context.Context, root string, scope corememory.Scope) ([]factview.Fact, error) {
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
	return store.ListScope(ctx, scope)
}

// nearestRank is the percentile the density reading reports with, so the two
// readings describe a corpus the same way.
func nearestRank(values []int, percentile float64) int {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	rank := int(percentile * float64(len(sorted)))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func shorten(text string, runes int) string {
	trimmed := strings.Join(strings.Fields(text), " ")
	if runes <= 0 {
		return trimmed
	}
	measured := []rune(trimmed)
	if len(measured) <= runes {
		return trimmed
	}
	return string(measured[:runes]) + "..."
}
