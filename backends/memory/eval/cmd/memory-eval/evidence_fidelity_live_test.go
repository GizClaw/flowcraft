package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestEvidenceFidelity measures how much of our "full evidence" is really
// visible to the answer model. A hit counts as raw only when the evidence
// turn's own text reached the prompt; a provenance-only hit means the model saw
// a derived paraphrase (a fact) instead of the wording the dataset graded, and
// surface details like a book title can be missing there.
//
// Retrieval only: no answer or judge calls.
func TestEvidenceFidelity(t *testing.T) {
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
		t.Skip("set MEMORY_EVAL_LIVE=1 to measure live evidence fidelity")
	}
	raw, err := os.ReadFile(filepath.Join(workdir, "..", "..", "locomo10.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenarios, _, err := eval.LoadLoCoMo(raw, eval.LoaderOptions{
		Scope:  eval.Scope{RuntimeID: "memories"},
		Budget: corememory.Budget{MaxItems: 30, MaxTokens: 6144},
	})
	if err != nil {
		t.Fatal(err)
	}
	built := buildAssembly(filepath.Join(workdir, "..", "..", "deploy.yaml"), 20)
	defer built.Close()
	scope := corememory.Scope{RuntimeID: "memories"}
	resolver := messageProvenance{store: built.Memory.MessageStore(), scope: scope}

	type probe struct {
		scenario eval.Scenario
		question eval.Question
		turns    map[string]string
		raw      int
		only     int
		total    int
		kinds    map[corememory.ContextItemKind]int
	}
	var probes []probe
	const limit = 120
	for _, scenario := range scenarios[:3] {
		index := map[string]string{}
		for _, turn := range scenario.Turns {
			for position, message := range turn.Messages {
				if position >= len(turn.DatasetIDs) {
					break
				}
				id := strings.TrimSpace(turn.DatasetIDs[position])
				text := strings.TrimSpace(message.Content.Text())
				if id != "" && text != "" {
					if _, exists := index[id]; !exists {
						index[id] = text
					}
				}
			}
		}
		for _, question := range scenario.Questions {
			if question.Category != 1 && question.Category != 3 {
				continue
			}
			if len(question.Evidence) == 0 {
				continue
			}
			probes = append(probes, probe{
				scenario: scenario, question: question, turns: index,
				kinds: map[corememory.ContextItemKind]int{},
			})
			if len(probes) >= limit {
				break
			}
		}
		if len(probes) >= limit {
			break
		}
	}

	var next atomic.Int64
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= len(probes) {
					return
				}
				current := &probes[index]
				result, err := built.Memory.Context(context.Background(), corememory.ContextRequest{
					Scope: scope, ConversationID: current.scenario.ConversationID,
					Query: current.question.Query, Budget: current.scenario.Budget,
				})
				if err != nil {
					continue
				}
				resolved := map[string][]string{}
				for _, id := range current.question.Evidence {
					turnText := current.turns[id]
					if turnText == "" {
						continue
					}
					current.total++
					raw := false
					for _, item := range result.Items {
						if strings.Contains(item.Content.Text(), turnText) {
							raw = true
							break
						}
					}
					if raw {
						current.raw++
						continue
					}
					for _, item := range result.Items {
						texts, ok := resolved[item.ID]
						if !ok {
							texts = resolver.ResolveSourceTexts(context.Background(), item)
							resolved[item.ID] = texts
						}
						for _, text := range texts {
							if strings.Contains(text, turnText) {
								current.only++
								current.kinds[item.Kind]++
								raw = true
								break
							}
						}
						if raw {
							break
						}
					}
				}
			}
		}()
	}
	group.Wait()

	var total, rawHits, provenanceOnly, questionsWithProvenanceOnly int
	kinds := map[corememory.ContextItemKind]int{}
	for _, current := range probes {
		total += current.total
		rawHits += current.raw
		provenanceOnly += current.only
		for kind, count := range current.kinds {
			kinds[kind] += count
		}
		if current.only > 0 {
			questionsWithProvenanceOnly++
		}
	}
	t.Logf("sampled %d cat-1/cat-3 questions, %d evidence turns", len(probes), total)
	t.Logf("evidence turns: raw=%d (%.1f%%) provenance-only=%d (%.1f%%)",
		rawHits, percent(rawHits, total), provenanceOnly, percent(provenanceOnly, total))
	t.Logf("questions whose evidence arrived as a paraphrase: %d/%d (%.1f%%)",
		questionsWithProvenanceOnly, len(probes), percent(questionsWithProvenanceOnly, len(probes)))
	t.Logf("item kind behind provenance-only hits: %v", kinds)
}

func percent(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}
