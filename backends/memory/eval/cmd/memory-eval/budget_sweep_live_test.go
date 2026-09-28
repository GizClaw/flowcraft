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

// TestRetrievalBudgetSweep answers "can we shrink the context?" with retrieval
// only: the same questions are retrieved under several item budgets and the
// evidence coverage is compared. No answering, no judging.
//
// MEMORY_EVAL_BUDGETS=10,15,20,30 (default) MEMORY_EVAL_PER_CATEGORY=25
func TestRetrievalBudgetSweep(t *testing.T) {
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
		t.Skip("set MEMORY_EVAL_LIVE=1 to run the live budget sweep")
	}
	budgets := []int{10, 15, 20, 30}
	if raw := os.Getenv("MEMORY_EVAL_BUDGETS"); raw != "" {
		budgets = nil
		for _, value := range strings.Split(raw, ",") {
			parsed, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil {
				t.Fatalf("bad budget %q", value)
			}
			budgets = append(budgets, parsed)
		}
	}
	perCategory := 25
	if raw := os.Getenv("MEMORY_EVAL_PER_CATEGORY"); raw != "" {
		perCategory, err = strconv.Atoi(raw)
		if err != nil {
			t.Fatal(err)
		}
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
		scenario eval.Scenario
		question eval.Question
		turns    map[string]string
	}
	byCategory := map[int][]sample{}
	for _, scenario := range scenarios {
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
			if len(question.Evidence) == 0 || len(byCategory[question.Category]) >= perCategory {
				continue
			}
			byCategory[question.Category] = append(byCategory[question.Category],
				sample{scenario: scenario, question: question, turns: index})
		}
	}

	type result struct {
		turns, found, fullQuestions, questions int
	}
	for _, budget := range budgets {
		outcome := result{}
		var mu sync.Mutex
		var next atomic.Int64
		work := make([]sample, 0, perCategory*4)
		for _, category := range []int{1, 2, 3, 4} {
			work = append(work, byCategory[category]...)
		}
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
					request := corememory.ContextRequest{
						Scope: scope, ConversationID: current.scenario.ConversationID,
						Query:  current.question.Query,
						Budget: corememory.Budget{MaxItems: budget, MaxTokens: budget * 205},
					}
					recalled, err := built.Memory.Context(context.Background(), request)
					if err != nil {
						continue
					}
					resolved := map[string][]string{}
					turns, found := 0, 0
					for _, id := range current.question.Evidence {
						turnText := current.turns[id]
						if turnText == "" {
							continue
						}
						turns++
						if evidenceVisible(recalled.Items, turnText, resolver, resolved) {
							found++
						}
					}
					mu.Lock()
					outcome.turns += turns
					outcome.found += found
					outcome.questions++
					if turns > 0 && found == turns {
						outcome.fullQuestions++
					}
					mu.Unlock()
				}
			}()
		}
		group.Wait()
		t.Logf("max_items=%2d (max_tokens=%6d): questions=%3d turn_recall=%.3f full_evidence=%.3f",
			budget, budget*205,
			outcome.questions, ratio(outcome.found, outcome.turns), ratio(outcome.fullQuestions, outcome.questions))
	}
}

func evidenceVisible(items []corememory.ContextItem, turnText string, resolver messageProvenance, resolved map[string][]string) bool {
	for _, item := range items {
		if strings.Contains(item.Content.Text(), turnText) {
			return true
		}
		texts, ok := resolved[item.ID]
		if !ok {
			texts = resolver.ResolveSourceTexts(context.Background(), item)
			resolved[item.ID] = texts
		}
		for _, text := range texts {
			if strings.Contains(text, turnText) {
				return true
			}
		}
	}
	return false
}

func ratio(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}
