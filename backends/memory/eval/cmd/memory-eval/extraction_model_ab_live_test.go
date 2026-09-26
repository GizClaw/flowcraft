package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/backends/memory/lines/chat"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// TestExtractionModelAB answers "is model X good enough to extract?" before
// anyone spends a full derivation pass on it. It runs the production
// extractor (same prompt, same response schema, same bounds) over the same
// turns with two generate models and reports four things: how often the call
// or its schema fails, how many facts come back, how much of the turn's
// surface detail (dates, numbers, titles, proper nouns) survives into the
// facts, and how many facts the turn does not support.
//
// Output: $MEMORY_EVAL_EXTRACTION_AB (default /tmp/extraction-model-ab.json).
func TestExtractionModelAB(t *testing.T) {
	envPath := liveEnvFile(t)
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	loadEnvFile(envPath)
	if os.Getenv("DEEPSEEK_API_KEY") == "" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("DEEPSEEK_API_KEY and OPENAI_API_KEY are required")
	}
	if os.Getenv("MEMORY_EVAL_LIVE") != "1" {
		t.Skip("set MEMORY_EVAL_LIVE=1 to compare extraction models")
	}
	raw, err := os.ReadFile(filepath.Join(workdir, "..", "..", "locomo10.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenarios, _, err := eval.LoadLoCoMo(raw, eval.LoaderOptions{
		Scope: eval.Scope{RuntimeID: "memories"},
		// Captions instead of images: the production extractor only ever sees
		// Content.Text(), so the picture never reaches it either way, and both
		// models must see the same input.
		Images: "annotation",
	})
	if err != nil {
		t.Fatal(err)
	}
	samples := sampleTurns(scenarios, 3)
	if len(samples) < 10 {
		t.Fatalf("only %d sampled turns, want at least 10", len(samples))
	}
	built := buildAssembly(filepath.Join(workdir, "..", "..", "deploy.yaml"), 20)
	defer built.Close()

	type modelUnderTest struct {
		label    string
		provider string
		name     string
	}
	models := []modelUnderTest{
		{label: "deepseek-flash", provider: "deepseek", name: "deepseek-flash"},
		{label: "gpt-4o-mini", provider: "openai", name: "gpt-4o-mini"},
	}

	type turnResult struct {
		Scenario  string   `json:"scenario"`
		Turn      int      `json:"turn"`
		Error     string   `json:"error,omitempty"`
		Facts     []string `json:"facts"`
		LatencyMS int64    `json:"latency_ms"`
		Surface   int      `json:"surface_details"`
		Covered   int      `json:"surface_covered"`
		Unsupport int      `json:"unsupported_facts"`
	}
	type modelReport struct {
		Model            string       `json:"model"`
		Calls            int          `json:"calls"`
		Failures         int          `json:"failures"`
		Facts            int          `json:"facts"`
		FactsPerTurn     float64      `json:"facts_per_turn"`
		Surface          int          `json:"surface_details"`
		Covered          int          `json:"surface_covered"`
		Coverage         float64      `json:"surface_coverage"`
		Unsupported      int          `json:"unsupported_facts"`
		UnsupportedShare float64      `json:"unsupported_share"`
		LatencyP50MS     int64        `json:"latency_p50_ms"`
		Turns            []turnResult `json:"turns"`
	}

	reports := make([]modelReport, 0, len(models))
	for _, current := range models {
		config := chat.DefaultConfig()
		config.GenerateModel = &model.ModelRef{ID: model.ModelID{Provider: current.provider, Name: current.name}}
		config.Runtime = built.Inference
		extractor, err := chat.NewFactExtractorWithConfig(config)
		if err != nil {
			t.Fatalf("%s: build extractor: %v", current.label, err)
		}
		report := modelReport{Model: current.label}
		var latencies []int64
		for index, sample := range samples {
			started := time.Now()
			artifacts, err := extractor.Derive(context.Background(), probeArtifact(sample.scenario, index, sample.text, sample.eventTime))
			latency := time.Since(started)
			latencies = append(latencies, latency.Milliseconds())
			result := turnResult{Scenario: sample.scenario, Turn: sample.turn, LatencyMS: latency.Milliseconds()}
			report.Calls++
			if err != nil {
				report.Failures++
				result.Error = err.Error()
				report.Turns = append(report.Turns, result)
				continue
			}
			var builder strings.Builder
			for _, artifact := range artifacts {
				text := strings.TrimSpace(artifact.Content.Text())
				if text == "" {
					continue
				}
				result.Facts = append(result.Facts, text)
				builder.WriteString(text)
				builder.WriteByte('\n')
			}
			facts := builder.String()
			surface := surfaceDetails(sample.text)
			result.Surface = len(surface)
			for _, detail := range surface {
				if strings.Contains(strings.ToLower(facts), strings.ToLower(detail)) {
					result.Covered++
				}
			}
			for _, fact := range result.Facts {
				if !supportedBy(fact, sample.text) {
					result.Unsupport++
				}
			}
			report.Facts += len(result.Facts)
			report.Surface += result.Surface
			report.Covered += result.Covered
			report.Unsupported += result.Unsupport
			report.Turns = append(report.Turns, result)
		}
		report.FactsPerTurn = float64(report.Facts) / float64(max(report.Calls-report.Failures, 1))
		if report.Surface > 0 {
			report.Coverage = float64(report.Covered) / float64(report.Surface)
		}
		if report.Facts > 0 {
			report.UnsupportedShare = float64(report.Unsupported) / float64(report.Facts)
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		if len(latencies) > 0 {
			report.LatencyP50MS = latencies[len(latencies)/2]
		}
		reports = append(reports, report)
	}

	outPath := os.Getenv("MEMORY_EVAL_EXTRACTION_AB")
	if outPath == "" {
		outPath = "/tmp/extraction-model-ab.json"
	}
	encoded, err := json.MarshalIndent(map[string]any{
		"turns": len(samples), "models": reports, "generated_at": time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Logf("extraction A/B over %d turns (caption mode: both models see the same text)", len(samples))
	t.Logf("%-16s %6s %8s %10s %12s %14s %10s", "model", "calls", "failures", "facts/turn", "surface cov", "unsupported", "p50 ms")
	for _, report := range reports {
		t.Logf("%-16s %6d %8d %10.2f %11.1f%% %13.1f%% %10d",
			report.Model, report.Calls, report.Failures, report.FactsPerTurn,
			report.Coverage*100, report.UnsupportedShare*100, report.LatencyP50MS)
	}
	for _, report := range reports {
		for _, turn := range report.Turns {
			if turn.Error != "" {
				t.Logf("  %s %s#%d failed: %s", report.Model, turn.Scenario, turn.Turn, firstLine(turn.Error))
			}
		}
	}
	t.Logf("wrote %s", outPath)
}

type turnSample struct {
	scenario  string
	turn      int
	text      string
	eventTime time.Time
}

// sampleTurns takes perConversation evenly spaced turns from every scenario.
func sampleTurns(scenarios []eval.Scenario, perConversation int) []turnSample {
	var samples []turnSample
	for _, scenario := range scenarios {
		turns := scenario.Turns
		if len(turns) == 0 {
			continue
		}
		step := max(len(turns)/perConversation, 1)
		for index := 0; index < len(turns) && len(samples) < perConversation*(len(scenarios)); index += step {
			turn := turns[index]
			var lines []string
			for _, message := range turn.Messages {
				text := strings.TrimSpace(message.Content.Text())
				if text == "" {
					continue
				}
				lines = append(lines, string(message.Role)+": "+text)
			}
			if len(lines) == 0 {
				continue
			}
			samples = append(samples, turnSample{
				scenario: scenario.Name,
				turn:     index,
				text:     strings.Join(lines, "\n"),
				// A fixed timestamp: event_time feeds linking and maintenance,
				// not the extraction prompt -- the turn's own date is already
				// in the text as the loader's "[...]" prefix.
				eventTime: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
			})
		}
	}
	return samples
}

// probeArtifact mirrors what the worker hands the extractor for one commit.
func probeArtifact(scenario string, index int, text string, eventTime time.Time) component.Artifact {
	return component.Artifact{
		Kind:    chat.KindRawMessage,
		ID:      fmt.Sprintf("probe-%s-%d", scenario, index),
		Content: coremessage.NewTextContent(text),
		Sources: []corememory.SourceRef{{
			Kind: corememory.SourceMessage,
			ID:   fmt.Sprintf("%s/msg-%020d", scenario, index+1),
		}},
		Metadata: corememory.Metadata{
			"runtime_id":      "memories",
			"conversation_id": scenario,
			"event_time":      eventTime.Format(time.RFC3339Nano),
		},
	}
}

var (
	surfaceDates   = regexp.MustCompile(`(?i)\b\d{1,2}\s+(january|february|march|april|may|june|july|august|september|october|november|december)\b|\b(january|february|march|april|may|june|july|august|september|october|november|december)\s+\d{1,2}\b`)
	surfaceNumbers = regexp.MustCompile(`\b\d[\d.,]*\b`)
	surfaceQuoted  = regexp.MustCompile(`"([^"]{3,60})"|“([^”]{3,60})”`)
	surfaceProper  = regexp.MustCompile(`\b([A-Z][a-z]{2,}(?:\s+[A-Z][a-z]{2,})+)\b`)
)

// surfaceDetails extracts the details a LoCoMo gold answer is made of: dates,
// numbers, quoted titles and multi-word proper nouns.
func surfaceDetails(text string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if len(value) < 2 {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	for _, match := range surfaceDates.FindAllString(text, -1) {
		add(match)
	}
	for _, match := range surfaceNumbers.FindAllString(text, -1) {
		add(match)
	}
	for _, groups := range surfaceQuoted.FindAllStringSubmatch(text, -1) {
		for _, group := range groups[1:] {
			add(group)
		}
	}
	for _, groups := range surfaceProper.FindAllStringSubmatch(text, -1) {
		add(groups[1])
	}
	return out
}

var stopWords = map[string]struct{}{
	"the": {}, "and": {}, "that": {}, "with": {}, "for": {}, "was": {}, "were": {}, "have": {},
	"has": {}, "had": {}, "she": {}, "her": {}, "his": {}, "him": {}, "they": {}, "them": {},
	"this": {}, "that's": {}, "from": {}, "about": {}, "into": {}, "over": {}, "when": {},
	"what": {}, "which": {}, "been": {}, "will": {}, "would": {}, "could": {}, "their": {},
}

func contentTokens(text string) map[string]struct{} {
	fields := regexp.MustCompile(`[^a-z0-9]+`).Split(strings.ToLower(text), -1)
	out := map[string]struct{}{}
	for _, field := range fields {
		if len(field) < 3 {
			continue
		}
		if _, skip := stopWords[field]; skip {
			continue
		}
		out[field] = struct{}{}
	}
	return out
}

// supportedBy is a coarse hallucination proxy: a fact whose content words
// barely appear in the turn it came from is probably not grounded in it.
func supportedBy(fact, turn string) bool {
	factTokens := contentTokens(fact)
	if len(factTokens) == 0 {
		return true
	}
	turnTokens := contentTokens(turn)
	overlap := 0
	for token := range factTokens {
		if _, ok := turnTokens[token]; ok {
			overlap++
		}
	}
	return float64(overlap)/float64(len(factTokens)) >= 0.3
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return value[:index]
	}
	return value
}
