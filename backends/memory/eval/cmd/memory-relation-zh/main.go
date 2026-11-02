// Command memory-relation-zh measures what the shipped supersede rules do with
// Chinese: the maintenance vocabulary (a phrase table keyed on the extractor's
// predicate) and the token-similarity fallback.
//
// It is the static half of the relation reading. The corpus is two hand-written
// halves -- a predicate inventory and a set of two-turn conversations whose
// later turn changes the value -- and both are labelled by construction, so the
// command needs no model, no workspace and no network: it asks the shipped
// vocabulary and the shipped similarity measure what they make of text a
// Chinese speaker wrote.
//
// What it cannot say is how often a model writes what the inventory lists. That
// is the live half, and it is a different command's job: this one measures the
// rules, not the extractor.
//
// The corpus lives in the package because the reading is a comparison: the same
// labelled pairs are what a candidate rule (a Chinese phrase table, a decision
// model, an embedding classifier) has to be scored against, and a corpus that
// moves between runs stops being a comparison.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/GizClaw/flowcraft/backends/memory/internal/textsim"
	"github.com/GizClaw/flowcraft/backends/memory/maintain"
)

// reading is the whole measurement, printable as text or JSON.
type reading struct {
	// VocabularyDigest names the vocabulary measured, so two readings can say
	// whether they compared the same rules.
	VocabularyDigest string `json:"vocabulary_digest"`
	// Threshold is the similarity the pass ships.
	Threshold float64 `json:"threshold"`
	// Phrases is the predicate inventory's side.
	Phrases phraseReading `json:"phrases"`
	// Pairs is the conversation corpus's side.
	Pairs pairReading `json:"pairs"`
}

// phraseReading scores the vocabulary against the inventory: a phrase the
// inventory attributes to a class must be classified into that class, and a
// phrase the inventory calls repeatable must not be classified at all.
type phraseReading struct {
	Positives int `json:"positives"`
	Classed   int `json:"classed"`
	// Negative is the count of phrases that must stay unclassified, and
	// WronglyClassed is how many of them the vocabulary classified anyway --
	// the failure that replaces one of several current values.
	Negative       int `json:"negative"`
	WronglyClassed int `json:"wrongly_classed"`
	// ControlPositives and ControlClassed are the English entries, counted
	// apart: they are the proof the vocabulary works on the language it was
	// written in, not part of what it measures.
	ControlPositives int `json:"control_positives"`
	ControlClassed   int `json:"control_classed"`
	// Misses are the positive phrases the vocabulary left unclassified.
	Misses []string `json:"misses,omitempty"`
	// Classes is the per-class split of Positives and Classed.
	Classes []classReading `json:"classes"`
}

type classReading struct {
	Class   string `json:"class"`
	Total   int    `json:"total"`
	Classed int    `json:"classed"`
}

// pairReading scores the similarity rule against the conversation corpus: a
// conversation whose later turn changes a value must be seen as one, and one
// whose turns are both current must not be.
type pairReading struct {
	Updates   int `json:"updates"`
	Detected  int `json:"detected"`
	Negatives int `json:"negatives"`
	// FalsePositives is how many repeatable conversations the rule called an
	// update -- the failure the class rule exists to avoid, and the one a
	// threshold cannot talk its way out of.
	FalsePositives int         `json:"false_positives"`
	Highest        float64     `json:"highest_update_similarity"`
	Lower          []pairScore `json:"pairs"`
}

type pairScore struct {
	ID         string  `json:"id"`
	Update     bool    `json:"update"`
	Similarity float64 `json:"similarity"`
	Detected   bool    `json:"detected"`
}

func main() {
	asJSON := flag.Bool("json", false, "print the reading as JSON instead of text")
	flag.Parse()

	measured := measure()
	if *asJSON {
		encoded, err := json.MarshalIndent(measured, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "memory-relation-zh: encode: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(encoded))
		return
	}
	print(measured)
}

// measure asks both rules what they make of the corpus.
func measure() reading {
	vocabulary := maintain.NewVocabulary(nil)
	measured := reading{
		VocabularyDigest: vocabulary.Digest(),
		Threshold:        maintain.DefaultSimilarityThreshold,
	}

	classed := make(map[string]int, len(zhRelationInventory))
	totals := make(map[string]int, len(zhRelationInventory))
	order := make([]string, 0, len(zhRelationInventory))
	for _, entry := range zhRelationInventory {
		class, ok := vocabulary.ClassOf(entry.Predicate)
		if entry.Control {
			if entry.Relation == "" {
				continue
			}
			measured.Phrases.ControlPositives++
			if ok && class == entry.Relation {
				measured.Phrases.ControlClassed++
			}
			continue
		}
		if entry.Relation == "" {
			measured.Phrases.Negative++
			if ok {
				measured.Phrases.WronglyClassed++
			}
			continue
		}
		measured.Phrases.Positives++
		if _, seen := totals[entry.Relation]; !seen {
			order = append(order, entry.Relation)
		}
		totals[entry.Relation]++
		if ok && class == entry.Relation {
			measured.Phrases.Classed++
			classed[entry.Relation]++
			continue
		}
		measured.Phrases.Misses = append(measured.Phrases.Misses,
			fmt.Sprintf("%s (%s)", entry.Predicate, entry.Relation))
	}
	sort.Strings(order)
	for _, class := range order {
		measured.Phrases.Classes = append(measured.Phrases.Classes, classReading{
			Class: class, Total: totals[class], Classed: classed[class],
		})
	}

	for _, scenario := range zhScenarios {
		older, newer, ok := firstTwoTurns(scenario)
		if !ok {
			continue
		}
		similarity := textsim.Jaccard(older.Says, newer.Says)
		update := older.Want != ""
		detected := update && similarity >= measured.Threshold
		switch {
		case update:
			measured.Pairs.Updates++
			if detected {
				measured.Pairs.Detected++
			}
			if similarity > measured.Pairs.Highest {
				measured.Pairs.Highest = similarity
			}
		default:
			measured.Pairs.Negatives++
			if detected {
				measured.Pairs.FalsePositives++
			}
		}
		measured.Pairs.Lower = append(measured.Pairs.Lower, pairScore{
			ID: scenario.ID, Update: update, Similarity: similarity, Detected: detected,
		})
	}
	return measured
}

// firstTwoTurns returns the pair a two-turn scenario states, if it has two.
func firstTwoTurns(scenario zhScenario) (zhTurn, zhTurn, bool) {
	if len(scenario.Turns) < 2 {
		return zhTurn{}, zhTurn{}, false
	}
	return scenario.Turns[0], scenario.Turns[1], true
}

func print(measured reading) {
	fmt.Printf("vocabulary: maintain.DefaultRelations (digest %s)\n", measured.VocabularyDigest)
	fmt.Printf("similarity: maintain default threshold %.2f\n\n", measured.Threshold)

	fmt.Printf("predicate inventory: %d/%d Chinese phrases classified\n",
		measured.Phrases.Classed, measured.Phrases.Positives)
	for _, class := range measured.Phrases.Classes {
		fmt.Printf("  %-15s %2d/%2d\n", class.Class, class.Classed, class.Total)
	}
	fmt.Printf("  negatives      %2d classified as a relation of %d\n\n",
		measured.Phrases.WronglyClassed, measured.Phrases.Negative)
	fmt.Printf("english control: %d/%d phrases classified\n\n",
		measured.Phrases.ControlClassed, measured.Phrases.ControlPositives)
	if len(measured.Phrases.Misses) > 0 {
		fmt.Println("  missed:")
		for _, miss := range measured.Phrases.Misses {
			fmt.Printf("    %s\n", miss)
		}
		fmt.Println()
	}

	fmt.Printf("conversations: %d/%d update pairs above the threshold (highest %.3f)\n",
		measured.Pairs.Detected, measured.Pairs.Updates, measured.Pairs.Highest)
	fmt.Printf("  repeatable pairs called an update: %d/%d\n\n",
		measured.Pairs.FalsePositives, measured.Pairs.Negatives)
	for _, pair := range measured.Pairs.Lower {
		label := "repeatable"
		if pair.Update {
			label = "update"
		}
		mark := " "
		if pair.Detected {
			mark = "x"
		}
		fmt.Printf("  [%s] %-34s %s similarity=%.3f\n", mark, pair.ID, label, pair.Similarity)
	}

	fmt.Printf("\n=> reachable by neither rule: %d/%d update pairs\n",
		measured.Pairs.Updates-measured.Pairs.Detected, measured.Pairs.Updates)
	if measured.Phrases.Classed == 0 {
		fmt.Println("   the vocabulary is keyed on English predicates")
	}
	if measured.Pairs.Detected == 0 {
		fmt.Println("   the similarity rule's default threshold is above every pair measured")
	}
}
