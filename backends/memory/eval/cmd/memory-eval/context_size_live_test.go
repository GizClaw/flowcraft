package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// renderLimitRunes mirrors eval/answer's renderContext cap: the answerer renders
// at most this many runes of recalled context into one prompt.
const renderLimitRunes = 24_000

// TestAnswerContextSize reports how big the prompt context actually is against
// the real deployment and an already-derived workspace: the configured budget
// (max items / max tokens) is a ceiling, but what reaches the model is whatever
// the packer selected. It retrieves only -- no answer or judge calls -- so it is
// cheap enough to run whenever the question comes up.
func TestAnswerContextSize(t *testing.T) {
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
		t.Skip("set MEMORY_EVAL_LIVE=1 to measure the live answer context")
	}
	if _, err := os.Stat(filepath.Join(workdir, "..", "..", "workspace", "storage")); err != nil {
		t.Skip("no derived workspace next to deploy.yaml; run an eval first")
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
	deployPath := os.Getenv("MEMORY_EVAL_DEPLOY")
	if deployPath == "" {
		deployPath = filepath.Join(workdir, "..", "..", "deploy.yaml")
	}
	built := buildAssembly(deployPath, 20)
	defer built.Close()

	var (
		items     []int
		tokens    []int
		rendered  []int
		latencies []time.Duration
		capped    int
	)
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	workers := 1
	if raw := os.Getenv("MEMORY_EVAL_LATENCY_WORKERS"); raw != "" {
		if value, convErr := strconv.Atoi(raw); convErr == nil && value > 0 {
			workers = value
		}
	}
	sampled := 0
	type probeTarget struct {
		scenario eval.Scenario
		question eval.Question
	}
	var targets []probeTarget
	for _, scenario := range scenarios[:2] {
		for index, question := range scenario.Questions {
			if index%7 != 0 { // a spread across the conversation, kept cheap
				continue
			}
			targets = append(targets, probeTarget{scenario: scenario, question: question})
		}
	}
	firstIDs := make([][]string, len(targets))
	var mu sync.Mutex
	var firstErr error
	var nextIndex atomic.Int64
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				targetIndex := int(nextIndex.Add(1)) - 1
				if targetIndex >= len(targets) {
					return
				}
				target := targets[targetIndex]
				scenario, question := target.scenario, target.question
				{
					started := time.Now()
					result, err := built.Memory.Context(ctx, corememory.ContextRequest{
						Scope: scope, ConversationID: scenario.ConversationID,
						Query: question.Query, Budget: scenario.Budget,
					})
					if err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
						return
					}
					mu.Lock()
					latencies = append(latencies, time.Since(started))
					mu.Unlock()
					ids := make([]string, 0, len(result.Items))
					for _, item := range result.Items {
						ids = append(ids, item.ID)
					}
					if targetIndex < len(firstIDs) {
						firstIDs[targetIndex] = ids
					}
					used := 0
					count := 0
					for _, item := range result.Items {
						text := strings.TrimSpace(item.Content.Text())
						if text == "" {
							continue
						}
						runes := len([]rune(text))
						if used+runes > renderLimitRunes {
							runes = renderLimitRunes - used
							capped++
						}
						used += runes
						count++
						tokens = append(tokens, item.TokenCount)
					}
					mu.Lock()
					items = append(items, count)
					rendered = append(rendered, used)
					sampled++
					mu.Unlock()
				}
			}
		}()
	}
	group.Wait()
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	if sampled == 0 {
		t.Fatal("no questions sampled")
	}
	sort.Ints(items)
	sort.Ints(rendered)
	t.Logf("sampled %d questions against the live workspace", sampled)
	t.Logf("items per prompt:      min=%d median=%d max=%d", items[0], items[len(items)/2], items[len(items)-1])
	t.Logf("packer token count:    total=%d median_per_question=%d", sumInts(tokens), medianPerQuestion(tokens, sampled))
	t.Logf("rendered context:      min=%d median=%d max=%d runes (cap %d, cap hit %d times)",
		rendered[0], rendered[len(rendered)/2], rendered[len(rendered)-1], renderLimitRunes, capped)
	// Cold/warm split: the first questions pay the message point-reads the
	// source-quote cache later absorbs.
	if len(latencies) > 8 {
		var cold, warm time.Duration
		const coldCount = 5
		for i, value := range latencies {
			if i < coldCount {
				cold += value
				continue
			}
			warm += value
		}
		t.Logf("cold first %d: mean=%s; warm rest: mean=%s",
			coldCount, (cold / coldCount).Round(time.Millisecond),
			(warm / time.Duration(len(latencies)-coldCount)).Round(time.Millisecond))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		var total time.Duration
		for _, value := range latencies {
			total += value
		}
		t.Logf("retrieval latency: mean=%s median=%s max=%s",
			(total / time.Duration(len(latencies))).Round(time.Millisecond),
			latencies[len(latencies)/2].Round(time.Millisecond),
			latencies[len(latencies)-1].Round(time.Millisecond))
	}
	if repeat := os.Getenv("MEMORY_EVAL_REPEAT"); repeat != "" {
		if rounds, convErr := strconv.Atoi(repeat); convErr == nil && rounds >= 2 {
			differing := 0
			var diffDepths []int
			for targetIndex, target := range targets {
				result, callErr := built.Memory.Context(ctx, corememory.ContextRequest{
					Scope: scope, ConversationID: target.scenario.ConversationID,
					Query: target.question.Query, Budget: target.scenario.Budget,
				})
				if callErr != nil {
					t.Fatal(callErr)
				}
				second := make([]string, 0, len(result.Items))
				for _, item := range result.Items {
					second = append(second, item.ID)
				}
				if !reflect.DeepEqual(firstIDs[targetIndex], second) {
					differing++
					first := firstIDs[targetIndex]
					if differing <= 5 {
						at := -1
						for index := 0; index < len(first) && index < len(second); index++ {
							if first[index] != second[index] {
								at = index
								break
							}
						}
						t.Logf("  differs: %q len=%d/%d first_diff_at=%d", target.question.Query, len(first), len(second), at)
					}
					diffDepths = append(diffDepths, firstDiffIndex(first, second))
				}
			}
			sort.Ints(diffDepths)
			if len(diffDepths) > 0 {
				t.Logf("first difference index: min=%d median=%d max=%d (list length ~%d)",
					diffDepths[0], diffDepths[len(diffDepths)/2], diffDepths[len(diffDepths)-1], len(firstIDs[0]))
			}
			t.Logf("repeat check: %d/%d questions returned a different item list across two passes",
				differing, len(targets))
		}
	}
	if totals := built.Memory.ContextStageTotals(); len(totals) > 0 {
		t.Logf("stage totals (mean per request):")
		stages := []string{"search", "hydrate", "quotes", "pack"}
		for name := range totals {
			if strings.HasPrefix(name, "lane:") {
				stages = append(stages, name)
			}
		}
		sort.Strings(stages)
		for _, stage := range stages {
			stat, ok := totals[stage]
			if !ok || stat.Count == 0 {
				continue
			}
			t.Logf("  %-7s n=%3d mean=%s total=%s", stage, stat.Count,
				(stat.Total / time.Duration(stat.Count)).Round(time.Millisecond), stat.Total.Round(time.Millisecond))
		}
	}
	t.Logf("approx prompt tokens:  median_context ~%d (runes/4) + system ~120 + question",
		rendered[len(rendered)/2]/4)
}

func sumInts(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

func medianPerQuestion(values []int, questions int) int {
	if questions == 0 {
		return 0
	}
	return sumInts(values) / questions
}

// firstDiffIndex reports where two item lists start to differ, or the shared
// length when one is a prefix of the other.
func firstDiffIndex(left, right []string) int {
	for index := 0; index < len(left) && index < len(right); index++ {
		if left[index] != right[index] {
			return index
		}
	}
	return min(len(left), len(right))
}
