package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestPackingRedundancy measures what the 30 packed items actually are: how
// many of them trace back to the same source turn (slots spent on repeats of
// one event) and how many carry the question's evidence. Retrieval only.
//
// MEMORY_EVAL_PER_CATEGORY=25 MEMORY_EVAL_MAX_ITEMS=30
func TestPackingRedundancy(t *testing.T) {
	envPath := liveEnvFile(t)
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	loadEnvFile(envPath)
	if os.Getenv("DEEPSEEK_API_KEY") == "" || os.Getenv("ARK_API_KEY") == "" {
		t.Skip("DEEPSEEK_API_KEY and ARK_API_KEY are required")
	}
	if os.Getenv("MEMORY_EVAL_LIVE") != "1" {
		t.Skip("set MEMORY_EVAL_LIVE=1 to measure packing")
	}
	perCategory := 25
	maxItems := 30
	if raw := os.Getenv("MEMORY_EVAL_PER_CATEGORY"); raw != "" {
		value, convErr := strconv.Atoi(raw)
		if convErr != nil {
			t.Fatal(convErr)
		}
		perCategory = value
	}
	if raw := os.Getenv("MEMORY_EVAL_MAX_ITEMS"); raw != "" {
		value, convErr := strconv.Atoi(raw)
		if convErr != nil {
			t.Fatal(convErr)
		}
		maxItems = value
	}
	raw, err := os.ReadFile(filepath.Join(workdir, "..", "..", "locomo10.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenarios, _, err := eval.LoadLoCoMo(raw, eval.LoaderOptions{Scope: eval.Scope{RuntimeID: "memories"}})
	if err != nil {
		t.Fatal(err)
	}
	built := buildAssembly(filepath.Join(workdir, "..", "..", "deploy.yaml"), 8)
	defer built.Close()
	scope := corememory.Scope{RuntimeID: "memories"}
	resolver := messageProvenance{store: built.Memory.MessageStore(), scope: scope}

	type sample struct {
		scenario  eval.Scenario
		question  eval.Question
		diaByText map[string]string
		evidence  map[string]struct{}
	}
	byCategory := map[int][]sample{}
	for _, scenario := range scenarios {
		diaByText := map[string]string{}
		for _, turn := range scenario.Turns {
			for position, message := range turn.Messages {
				if position >= len(turn.DatasetIDs) {
					break
				}
				id := strings.TrimSpace(turn.DatasetIDs[position])
				text := strings.TrimSpace(message.Content.Text())
				if id != "" && text != "" {
					if _, exists := diaByText[text]; !exists {
						diaByText[text] = id
					}
				}
			}
		}
		for _, question := range scenario.Questions {
			if len(question.Evidence) == 0 || len(byCategory[question.Category]) >= perCategory {
				continue
			}
			evidence := map[string]struct{}{}
			for _, id := range question.Evidence {
				evidence[id] = struct{}{}
			}
			byCategory[question.Category] = append(byCategory[question.Category],
				sample{scenario: scenario, question: question, diaByText: diaByText, evidence: evidence})
		}
	}
	var work []sample
	for _, category := range []int{1, 2, 3, 4} {
		work = append(work, byCategory[category]...)
	}

	var (
		mu                                             sync.Mutex
		items, uniqueTurns, evidenceItems, evidenceQ   int
		questions, coveredEvidenceTurns, totalEvidence int
	)
	var next atomic.Int64
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= len(work) {
					return
				}
				current := work[index]
				result, err := built.Memory.Context(context.Background(), corememory.ContextRequest{
					Scope: scope, ConversationID: current.scenario.ConversationID,
					Query:  current.question.Query,
					Budget: corememory.Budget{MaxItems: maxItems, MaxTokens: maxItems * 205},
				})
				if err != nil {
					continue
				}
				resolved := map[string][]string{}
				seenTurns := map[string]struct{}{}
				covered := map[string]struct{}{}
				localItems, localEvidenceItems := 0, 0
				for _, item := range result.Items {
					localItems++
					for _, text := range itemSourceTexts(item, resolver, resolved) {
						diaID, ok := current.diaByText[text]
						if !ok {
							continue
						}
						seenTurns[diaID] = struct{}{}
						if _, isEvidence := current.evidence[diaID]; isEvidence {
							localEvidenceItems++
							covered[diaID] = struct{}{}
							break
						}
					}
				}
				mu.Lock()
				items += localItems
				uniqueTurns += len(seenTurns)
				evidenceItems += localEvidenceItems
				questions++
				coveredEvidenceTurns += len(covered)
				totalEvidence += len(current.evidence)
				if len(covered) == len(current.evidence) {
					evidenceQ++
				}
				mu.Unlock()
			}
		}()
	}
	group.Wait()

	t.Logf("max_items=%d questions=%d", maxItems, questions)
	t.Logf("items packed=%d; distinct source turns=%d; duplicate share=%.3f",
		items, uniqueTurns, 1-float64(uniqueTurns)/float64(items))
	t.Logf("items carrying the question's evidence=%d (precision=%.3f)",
		evidenceItems, float64(evidenceItems)/float64(items))
	t.Logf("evidence turns covered=%d/%d (recall=%.3f); questions fully covered=%d (%.3f)",
		coveredEvidenceTurns, totalEvidence,
		float64(coveredEvidenceTurns)/float64(totalEvidence),
		evidenceQ, float64(evidenceQ)/float64(questions))
}

// itemSourceTexts returns the committed texts an item stands for: its own
// content for raw messages, and the resolved provenance for derived items.
func itemSourceTexts(item corememory.ContextItem, resolver messageProvenance, resolved map[string][]string) []string {
	if item.Kind == corememory.ContextRawMessage {
		return []string{strings.TrimSpace(item.Content.Text())}
	}
	texts, ok := resolved[item.ID]
	if !ok {
		texts = resolver.ResolveSourceTexts(context.Background(), item)
		resolved[item.ID] = texts
	}
	return texts
}
