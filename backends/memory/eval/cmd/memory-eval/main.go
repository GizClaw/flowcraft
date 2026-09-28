package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	evalanswer "github.com/GizClaw/flowcraft/backends/memory/eval/answer"
	"github.com/GizClaw/flowcraft/backends/memory/eval/internal/host"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/rerank"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func main() {
	dataset := flag.String("dataset", "locomo10.json", "LoCoMo JSON path")
	samples := flag.Int("samples", 1, "number of conversations to run")
	questions := flag.Int("questions", 0, "max questions per conversation (0 = all)")
	categoryFilter := flag.String("categories", "", "only run these question categories (comma-separated, empty = all)")
	imageMode := flag.String("images", "native",
		"how turns with an image are ingested: annotation | native | both. "+
			"native attaches the image itself, but remote urls are rejected by the multimodal embedder, "+
			"so it needs images materialized inline first")
	maxItems := flag.Int("max-items", 20, "context budget items")
	maxTokens := flag.Int("max-tokens", 4096, "context budget tokens")
	recentItems := flag.Int("recent-items", 20, "assembly recent.max_items")
	deployPath := flag.String("deploy", "", "build the assembly from a deploy.yaml instead of ad-hoc settings")
	envFile := flag.String("env-file", "", "load KEY=VALUE lines before building the deployment")
	buildOnly := flag.Bool("build-only", false, "build and wire the deployment, then exit")
	answerStage := flag.Bool("answer", false, "generate an answer from the recalled context and grade it")
	resume := flag.Bool("resume", false, "skip scenarios already present in -out and retry failed ones")
	answerProvider := flag.String("answer-provider", "deepseek", "provider id of the answering model")
	answerModel := flag.String("answer-model", "deepseek-flash", "name of the answering model (this endpoint also serves deepseek-v4-pro)")
	answerContextRunes := flag.Int("answer-context-runes", 24_000, "runes of recalled context rendered into one answer prompt")
	judgeProvider := flag.String("judge-provider", "", "provider id of the judge model (default: answer provider)")
	judgeModel := flag.String("judge-model", "", "name of the judge model (default: answer model)")
	judgeStyle := flag.String("judge-style", "strict", "judge prompt style: strict | locomo | both")
	answerStyle := flag.String("answer-style", "long",
		"answering protocol: long (product, used for strict/lenient), short (official LoCoMo protocol, for token-F1), or evidence (short, but the model quotes its evidence first)")
	temporalHint := flag.Bool("temporal-hint", false,
		"append the reference harness's category-2 suffix (\"Use DATE of CONVERSATION to answer with an approximate date.\") "+
			"to temporal questions; off by default, because it routes on the dataset's category label")
	answerConcurrency := flag.Int("answer-concurrency", 4,
		"questions processed in parallel (1 = sequential); only the schedule changes, never the prompts or the report order")
	prepareAll := flag.Bool("prepare-all", false,
		"ingest every scenario, run one derivation pass, then answer: lets derive.concurrency fan out across all conversations")
	skipDerive := flag.Bool("skip-derive", false,
		"answer from the derivation the workspace already holds: no ingest, no derive pass")
	baselinePath := flag.String("baseline", "", "compare the run against this baseline JSON and fail on regression")
	baselineTolerance := flag.Float64("baseline-tolerance", 2.0, "allowed regression in percentage points")
	out := flag.String("out", "", "write full reports JSON here")
	resumeAnyway := flag.Bool("resume-anyway", false,
		"reuse scenarios from -out even when their fingerprint differs from this run")
	flag.Parse()
	if *envFile != "" {
		loadEnvFile(*envFile)
	}
	if *skipDerive && *prepareAll {
		must(fmt.Errorf("memory eval: -skip-derive answers from the stored derivation, so it cannot run -prepare-all's ingest and derive pass"))
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
	fmt.Printf("dataset: scenarios=%d turns=%d questions=%d skipped=%d skipped_adversarial=%d",
		stats.Conversations, stats.Turns, stats.Questions, stats.Skipped, stats.SkippedAdversarial)
	if *imageMode == "native" || *imageMode == "both" {
		fmt.Printf(" images_attached=%d images_failed=%d", stats.ImagesAttached, stats.ImagesFailed)
		// Why the images are missing decides whether the run is comparable: a
		// dead link is the dataset's, a timeout is the network's.
		if failures := stats.ImageFailures; failures != (eval.ImageFailureCounts{}) {
			fmt.Printf(" image_failures{permanent=%d busy=%d transient=%d oversized=%d}",
				failures.Permanent, failures.Busy, failures.Transient, failures.Oversized)
		}
		if stats.ImagesShrunk > 0 {
			fmt.Printf(" images_shrunk=%d", stats.ImagesShrunk)
		}
	}
	fmt.Println()
	if stats.SkippedAdversarial > 0 {
		// Say it out loud: every rate below is measured over the answerable
		// categories only, and the excluded slice is not scored at all.
		fmt.Printf("note: %d adversarial (category 5) questions excluded by design: "+
			"all rates below cover answerable questions only\n", stats.SkippedAdversarial)
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

	deployment := buildAssembly(*deployPath, *recentItems)
	defer deployment.Close()
	if *buildOnly {
		fmt.Println("deployment built and wired successfully")
		return
	}

	// Evidence recall follows fact provenance back to the canonical messages a
	// fact was derived from, so a fact hit counts as the source turn being
	// recalled -- by dataset turn id where ingest tagged the messages, and by
	// committed text where the store predates that tagging.
	resolver := host.NewMessageProvenance(deployment.Memory.MessageStore(), corememory.Scope(evalScope))
	evidenceRule, taggedConversations := evidenceMatching(context.Background(), resolver, scenarios)
	if taggedConversations < len(scenarios) {
		fmt.Fprintf(os.Stderr,
			"warning: %d of %d conversations hold no dataset turn ids (ingested before ingest tagged messages):\n"+
				"  their evidence recall is matched by committed text only, so a rendering difference between\n"+
				"  the loader and the store hides a turn. Re-ingest into a fresh workspace for comparable numbers.\n",
			len(scenarios)-taggedConversations, len(scenarios))
	}
	options := eval.Options{
		Provenance:  resolver,
		Concurrency: *answerConcurrency,
	}
	var usage []usageRole
	if *answerStage {
		if deployment.Inference == nil {
			must(fmt.Errorf("answering requires -deploy: an inference assembly must be wired"))
		}
		answerer, err := evalanswer.New(deployment.Inference, model.ModelRef{
			ID: model.ModelID{Provider: *answerProvider, Name: *answerModel},
		})
		must(err)
		answerer = answerer.WithMaxContextRunes(*answerContextRunes).
			WithAnswerStyle(evalanswer.AnswerStyle(*answerStyle)).
			WithTemporalHint(*temporalHint)
		options.Answerer = answerer
		usage = append(usage, usageRole{role: "answer", counts: answerer})
		if *judgeProvider == "" {
			*judgeProvider = *answerProvider
		}
		if *judgeModel == "" {
			*judgeModel = *answerModel
		}
		newJudge := func(style evalanswer.JudgeStyle) *evalanswer.Model {
			judge, err := evalanswer.New(deployment.Inference, model.ModelRef{
				ID: model.ModelID{Provider: *judgeProvider, Name: *judgeModel},
			})
			must(err)
			return judge.WithJudgeStyle(style)
		}
		switch *judgeStyle {
		case "none":
			// The official metric is computed offline from the stored answers,
			// so this run needs no judge at all.
		case "strict":
			options.Judge = newJudge(evalanswer.JudgeStrict)
		case "locomo":
			options.Judge = newJudge(evalanswer.JudgeLocoMo)
		case "both":
			strict := newJudge(evalanswer.JudgeStrict)
			lenient := newJudge(evalanswer.JudgeLocoMo)
			options.Judge = strict
			options.LenientJudge = lenient
			usage = append(usage,
				usageRole{role: "judge", counts: strict},
				usageRole{role: "lenient_judge", counts: lenient})
		default:
			must(fmt.Errorf("unknown -judge-style %q (want strict|locomo|both)", *judgeStyle))
		}
		if options.Judge != nil && len(usage) == 1 {
			usage = append(usage, usageRole{role: "judge", counts: options.Judge.(*evalanswer.Model)})
		}
		for index := range usage {
			usage[index].model = judgeFingerprint(*judgeProvider, *judgeModel, *answerProvider, *answerModel)
			if usage[index].role == "answer" {
				usage[index].model = *answerProvider + "/" + *answerModel
			}
		}
	}

	started := time.Now()
	// The store's derivation state and the model counters are read lazily: a run
	// that derives must report what it produced, not what it started from, and a
	// run's token counters only fill up as it answers.
	currentLibrary := func() *eval.LibraryState {
		state := host.LibraryState(context.Background(), deployment.Memory)
		if state.Empty() {
			return nil
		}
		return &state
	}
	currentUsage := func() []eval.ModelUsage {
		measured := make([]eval.ModelUsage, 0, len(usage))
		for _, entry := range usage {
			calls, input, output := entry.counts.Stats()
			measured = append(measured, eval.ModelUsage{
				Role: entry.role, Model: entry.model,
				Calls: calls, InputTokens: input, OutputTokens: output,
			})
		}
		return measured
	}
	fingerprint := eval.Fingerprint{
		Revision:     buildRevision(),
		Dataset:      eval.DatasetDigest(raw),
		Questions:    stats.Questions,
		PolicyDigest: deployment.Memory.PolicyDigest(),
		Values: map[string]string{
			"loader":               "locomo10",
			"images":               *imageMode,
			"skipped_adversarial":  strconv.Itoa(stats.SkippedAdversarial),
			"samples":              strconv.Itoa(*samples),
			"questions_per_sample": strconv.Itoa(*questions),
			"categories":           *categoryFilter,
			"max_items":            strconv.Itoa(*maxItems),
			"max_tokens":           strconv.Itoa(*maxTokens),
			// The recent window the assembly actually serves with, not the
			// -recent-items flag: a deploy document sets recent.max_items
			// itself, so recording the flag would name a value no run used
			// (and make two runs that differ only in that setting agree).
			"recent_items":         strconv.Itoa(deployment.Recent.MaxItems),
			"recent_tokens":        strconv.Itoa(deployment.Recent.MaxTokens),
			"judge_style":          *judgeStyle,
			"answer_model":         *answerProvider + "/" + *answerModel,
			"answer_prompt":        answerPromptVersion(*answerStyle),
			"answer_style":         *answerStyle,
			"temporal_hint":        strconv.FormatBool(*temporalHint),
			"answer_context_runes": strconv.Itoa(*answerContextRunes),
			"judge_model":          judgeFingerprint(*judgeProvider, *judgeModel, *answerProvider, *answerModel),
			// How evidence recall was matched, which the store decides: turn
			// identity where ingest tagged the messages, committed text where it
			// did not. The two do not score the same, so a report stored under
			// one is not comparable with a run under the other.
			"evidence_matching": evidenceRule,
			"rerank_policy":     rerank.AlgorithmVersion,
			// Whether rerank actually ran. rerank_policy names the policy code
			// that is linked in, not whether retrieval used it, and the setting
			// lives in the deploy document: without this, two runs that differ
			// only in that switch carry the same fingerprint.
			"rerank":       onOff(deployment.Rerank),
			"judge_prompt": evalanswer.JudgePromptVersion,
			"schedule":     scheduleName(*prepareAll),
			// A run that answers from the stored derivation measures a
			// different thing than one that derives first, so the fingerprint
			// keeps the two apart (and -resume refuses to mix them).
			"derive": deriveMode(*skipDerive),
			// The assembly settings the policy digest does not cover (lane
			// weights, source quotes, the recent window) live in the deploy
			// document, so its digest pins them all. Without this, editing
			// deploy.yaml would change results while the fingerprint stayed
			// identical.
			"deploy": deployDigest(*deployPath),
			// -answer-concurrency is deliberately absent: it changes only the
			// schedule, so it must not block resuming a run.
		},
	}
	fmt.Printf("fingerprint: %s\n", fingerprint)
	if state := currentLibrary(); state != nil {
		fmt.Printf("library: %s\n", state)
		// Answering from the stored derivation measures whatever the store holds.
		// When the watermarks do not carry this policy digest, the facts being
		// graded were produced by another generation of the derivation policy --
		// which is invisible in the fingerprint and changes the numbers.
		if *skipDerive && state.Underived > 0 {
			fmt.Fprintf(os.Stderr,
				"warning: %d of %d conversations carry no derivation under policy digest %s:\n"+
					"  -skip-derive answers from facts another policy generation wrote, so this run measures that generation\n",
				state.Underived, state.Conversations, state.PolicyDigest)
		}
	}

	reports := make([]eval.Report, 0, len(scenarios))
	completed := map[string]eval.Report{}
	if *resume && *out == "" {
		must(fmt.Errorf("memory eval: -resume needs -out: there is nowhere to read previous results from"))
	}
	if *resume && *out != "" {
		previous := loadPartialRun(*out)
		if !previous.Fingerprint.Empty() && !previous.Fingerprint.Equal(fingerprint) {
			if !*resumeAnyway {
				must(fmt.Errorf("memory eval: %s was produced under a different configuration\n"+
					"  recorded: %s\n  current:  %s\n"+
					"re-run without -resume, or pass -resume-anyway to mix them on purpose",
					*out, previous.Fingerprint, fingerprint))
			}
			fmt.Fprintf(os.Stderr, "warning: resuming across fingerprints (recorded: %s; current: %s)\n",
				previous.Fingerprint, fingerprint)
		}
		completed = previous.Reports
	}
	if *prepareAll && !*skipDerive {
		if err := prepareAllScenarios(context.Background(), deployment.Memory, scenarios, completed); err != nil {
			must(err)
		}
	}
	for index, scenario := range scenarios {
		if report, ok := completed[scenario.Name]; ok {
			reports = append(reports, report)
			fmt.Printf("[%d/%d] scenario %-28s resumed (recall=%.3f answer=%.3f)\n",
				index+1, len(scenarios), report.Scenario, report.HitRate, report.AnswerRate)
			continue
		}
		var report eval.Report
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			if *prepareAll || *skipDerive {
				report, err = eval.Answer(context.Background(), deployment.Memory, scenario, options)
			} else {
				report, err = eval.RunWithOptions(context.Background(), deployment.Memory, scenario, options)
			}
			if err == nil {
				break
			}
			fmt.Fprintf(os.Stderr, "scenario %s attempt %d/3 failed: %v\n", scenario.Name, attempt, err)
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * 15 * time.Second)
			}
		}
		must(err)
		reports = append(reports, report)
		if *answerStage {
			line := fmt.Sprintf("[%d/%d] scenario %-28s questions=%4d recall=%4d/%.3f answer=%4d/%.3f",
				index+1, len(scenarios), report.Scenario, len(report.Questions), report.Hits, report.HitRate,
				report.AnswerHits, report.AnswerRate)
			line += fmt.Sprintf(" evidence=%d/%d(%.3f) raw=%d provenance=%d",
				report.EvidenceFound, report.EvidenceTotal, report.EvidenceRecall,
				report.EvidenceRaw, report.EvidenceProvenance)
			if report.UngradedAnswers > 0 {
				line += fmt.Sprintf(" ungraded_answers=%d", report.UngradedAnswers)
			}
			if report.LenientAnswers > 0 {
				line += fmt.Sprintf(" lenient=%4d/%.3f", report.LenientHits, report.LenientRate)
			}
			fmt.Printf("%s mean_latency=%s\n", line, report.MeanLatency.Round(time.Millisecond))
		} else {
			fmt.Printf("[%d/%d] scenario %-28s questions=%4d hits=%4d hit_rate=%.3f mean_latency=%s\n",
				index+1, len(scenarios), report.Scenario, len(report.Questions), report.Hits, report.HitRate,
				report.MeanLatency.Round(time.Millisecond))
		}
		if *out != "" {
			writeReports(*out, reports, fingerprint, &stats, currentUsage(), currentLibrary())
		}
	}
	wall := time.Since(started)

	var total, hits, ungraded, answers, answerHits int
	var evidenceTotal, evidenceFound, evidenceRaw, evidenceProvenance, ungradedAnswers, lenientAnswers, lenientHits int
	var latency time.Duration
	byCategory := map[int]eval.CategoryStats{}
	for _, report := range reports {
		total += len(report.Questions)
		hits += report.Hits
		ungraded += report.Ungraded
		answers += report.Answers
		answerHits += report.AnswerHits
		evidenceTotal += report.EvidenceTotal
		evidenceFound += report.EvidenceFound
		evidenceRaw += report.EvidenceRaw
		evidenceProvenance += report.EvidenceProvenance
		ungradedAnswers += report.UngradedAnswers
		lenientAnswers += report.LenientAnswers
		lenientHits += report.LenientHits
		latency += report.MeanLatency * time.Duration(len(report.Questions))
		for category, value := range report.ByCategory {
			current := byCategory[category]
			current.Questions += value.Questions
			current.Hits += value.Hits
			current.Answered += value.Answered
			current.AnswerHits += value.AnswerHits
			byCategory[category] = current
		}
	}
	graded := total - ungraded
	mean := time.Duration(0)
	if total > 0 {
		mean = latency / time.Duration(total)
	}
	fmt.Printf("\naggregate: scenarios=%d questions=%d graded=%d ungraded=%d hits=%d hit_rate=%.4f mean_latency=%s wall=%s\n",
		len(reports), total, graded, ungraded, hits, float64(hits)/float64(graded), mean.Round(time.Millisecond), wall.Round(time.Second))
	if evidenceTotal > 0 {
		fmt.Printf("evidence recall: turns=%d/%d recall=%.4f (raw=%d provenance-only=%d) questions_with_full_evidence=%.4f\n",
			evidenceFound, evidenceTotal, float64(evidenceFound)/float64(evidenceTotal),
			evidenceRaw, evidenceProvenance,
			float64(hits)/float64(graded))
	}
	if answers > 0 {
		gradedAnswers := answers - ungradedAnswers
		rate := 0.0
		if gradedAnswers > 0 {
			rate = float64(answerHits) / float64(gradedAnswers)
		}
		fmt.Printf("answered: questions=%d graded=%d ungraded=%d answer_hits=%d answer_rate=%.4f\n",
			answers, gradedAnswers, ungradedAnswers, answerHits, rate)
	}
	if lenientAnswers > 0 {
		fmt.Printf("lenient judge: graded=%d answer_hits=%d answer_rate=%.4f\n",
			lenientAnswers, lenientHits, float64(lenientHits)/float64(lenientAnswers))
	}
	for _, entry := range usage {
		calls, input, output := entry.counts.Stats()
		fmt.Printf("model usage: role=%s model=%s calls=%d input_tokens=%d output_tokens=%d\n",
			entry.role, entry.model, calls, input, output)
	}
	categories := make([]int, 0, len(byCategory))
	for category := range byCategory {
		categories = append(categories, category)
	}
	sort.Ints(categories)
	for _, category := range categories {
		stats := byCategory[category]
		line := fmt.Sprintf("category %d: questions=%4d hits=%4d hit_rate=%.4f",
			category, stats.Questions, stats.Hits, float64(stats.Hits)/float64(stats.Questions))
		if stats.Answered > 0 {
			line += fmt.Sprintf(" answer_hits=%4d answer_rate=%.4f",
				stats.AnswerHits, float64(stats.AnswerHits)/float64(stats.Answered))
		}
		fmt.Println(line)
	}
	if diagnostics, err := deployment.Memory.Diagnostics(context.Background()); err == nil {
		encoded, _ := json.Marshal(diagnostics.Worker)
		fmt.Printf("worker: %s\n", encoded)
	}
	if *out != "" {
		writeReports(*out, reports, fingerprint, &stats, currentUsage(), currentLibrary())
		fmt.Printf("wrote %s\n", *out)
	}
	if *baselinePath != "" {
		checkBaseline(*baselinePath, reports, fingerprint, *baselineTolerance/100)
	}
}

// checkBaseline fails the run when recall or answer rates regress beyond the
// tolerance against a recorded baseline.
func checkBaseline(path string, reports []eval.Report, fingerprint eval.Fingerprint, tolerance float64) {
	file, err := os.Open(path)
	must(err)
	defer func() { _ = file.Close() }()
	baseline, err := eval.LoadBaseline(file)
	must(err)
	if !baseline.Fingerprint.Empty() && !baseline.Fingerprint.Equal(fingerprint) {
		fmt.Fprintf(os.Stderr,
			"warning: baseline was measured under a different configuration, so every delta below compares two experiments\n"+
				"  baseline: %s\n  current:  %s\n",
			baseline.Fingerprint, fingerprint)
	}
	regressions := append(baseline.Compare(reports, tolerance), baseline.CompareAnswers(reports, tolerance)...)
	if len(regressions) == 0 {
		fmt.Printf("baseline: no regressions beyond %.2fpp\n", tolerance*100)
		return
	}
	for _, regression := range regressions {
		fmt.Fprintf(os.Stderr, "regression %s %s: %.3f -> %.3f (%+.3f)\n",
			regression.Scenario, regression.Metric, regression.Baseline, regression.Current, regression.Delta)
	}
	os.Exit(1)
}

// partialRun is what -resume reads back: the scenarios already recorded, plus
// the fingerprint they were measured under.
type partialRun struct {
	Fingerprint eval.Fingerprint
	Reports     map[string]eval.Report
}

// usageRole pairs one model role with its live counters, so the printed line and
// the stored report agree on which model spent what.
type usageRole struct {
	role   string
	model  string
	counts *evalanswer.Model
}

// onOff renders a boolean for the fingerprint values, which are strings.
func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

// scheduleName names the ingestion schedule a run uses, so a report says which
// one produced it.
func scheduleName(prepareAll bool) string {
	if prepareAll {
		return "prepare-all"
	}
	return "per-scenario"
}

// deriveMode names whether the run derived in this process or answered from
// the derivation the workspace already held.
func deriveMode(skipDerive bool) string {
	if skipDerive {
		return "reused"
	}
	return "fresh"
}

// answerPromptVersion names the answering policy the run used.
func answerPromptVersion(style string) string {
	switch style {
	case string(evalanswer.AnswerStyleShort):
		return evalanswer.AnswerShortPromptVersion
	case string(evalanswer.AnswerStyleEvidence):
		return evalanswer.AnswerEvidencePromptVersion
	}
	return evalanswer.AnswerPromptVersion
}

// judgeFingerprint names the grading model, spelling out the inheritance when no
// explicit judge is configured: the judge decides the score, so it belongs in
// the fingerprint.
func judgeFingerprint(judgeProvider, judgeModel, answerProvider, answerModel string) string {
	if strings.TrimSpace(judgeProvider) == "" && strings.TrimSpace(judgeModel) == "" {
		return "inherit:" + answerProvider + "/" + answerModel
	}
	provider, name := judgeProvider, judgeModel
	if strings.TrimSpace(provider) == "" {
		provider = answerProvider
	}
	if strings.TrimSpace(name) == "" {
		name = answerModel
	}
	return provider + "/" + name
}

// evidenceMatching names the rule evidence recall is graded by, and how many of
// the run's conversations the store had tagged for it. A store that predates
// ingest tagging leaves only the committed text to compare, and the two rules do
// not score the same, so the fingerprint carries which one a run measured under
// instead of claiming the code's own.
func evidenceMatching(ctx context.Context, resolver host.MessageProvenance, scenarios []eval.Scenario) (string, int) {
	tagged := 0
	for _, scenario := range scenarios {
		if resolver.CarriesTurnIDs(ctx, scenario.ConversationID, 32) {
			tagged++
		}
	}
	switch {
	case len(scenarios) == 0:
		return "no-scenarios", 0
	case tagged == 0:
		return "committed-text", tagged
	case tagged < len(scenarios):
		return fmt.Sprintf("turn-id %d/%d conversations", tagged, len(scenarios)), tagged
	default:
		return "turn-id", tagged
	}
}

// deployDigest summarizes the deploy document a run was built from, so the
// settings that never reach the derivation policy digest (recent window,
// retrieval toggles, lane weights) still pin the fingerprint.
func deployDigest(path string) string {
	if strings.TrimSpace(path) == "" {
		return "ad-hoc"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	return eval.DatasetDigest(raw)
}

// prepareAllScenarios ingests every scenario that still needs measuring and
// then pays for a single derivation pass. The worker fans out over the
// conversations that have pending commits, which is what makes
// derive.concurrency worth setting; per-scenario runs only ever have one.
//
// A derive pass is resumable: a conversation that fails keeps its watermark, so
// retrying the pass picks it up where it stopped. Parallel derivation raises the
// transient provider error rate, so the retry is part of running this way.
func prepareAllScenarios(ctx context.Context, runner eval.Runner, scenarios []eval.Scenario, completed map[string]eval.Report) error {
	pending := 0
	for _, scenario := range scenarios {
		if _, done := completed[scenario.Name]; done {
			continue
		}
		if err := eval.Ingest(ctx, runner, scenario); err != nil {
			return err
		}
		pending++
	}
	if pending == 0 {
		return nil
	}
	fmt.Printf("prepared %d scenarios; running one derivation pass\n", pending)
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = eval.Derive(ctx, runner); err == nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "derive pass attempt %d/3 failed, retrying from stored watermarks: %v\n", attempt, err)
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 15 * time.Second)
		}
	}
	return err
}

// buildRevision prefers the VCS stamp the binary was built with, and falls back
// to asking git directly: `go run` without -buildvcs=true leaves the binary
// unstamped, and an unstamped revision would silently weaken the fingerprint
// that -resume and -baseline rely on.
func buildRevision() string {
	if revision := eval.BuildRevision(); revision != "" {
		return revision
	}
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	revision := strings.TrimSpace(string(out))
	if revision == "" {
		return ""
	}
	if status, err := exec.Command("git", "status", "--porcelain").Output(); err == nil &&
		len(bytes.TrimSpace(status)) > 0 {
		revision += "+dirty"
	}
	return revision
}

// loadPartialRun reads the scenarios already recorded in a previous partial
// run so -resume can skip them, together with their fingerprint. A missing or
// unreadable file yields an empty result (nothing to skip).
func loadPartialRun(path string) partialRun {
	empty := partialRun{Reports: map[string]eval.Report{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var baseline eval.Baseline
	if err := json.Unmarshal(raw, &baseline); err != nil || len(baseline.Reports) == 0 {
		var stored struct {
			Scenarios []eval.Report `json:"scenarios"`
		}
		if err := json.Unmarshal(raw, &stored); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s is unreadable (%v); the run will start from scratch\n", path, err)
			return empty
		}
		baseline.Reports = stored.Scenarios
	}
	result := partialRun{Fingerprint: baseline.Fingerprint, Reports: make(map[string]eval.Report, len(baseline.Reports))}
	for _, report := range baseline.Reports {
		if len(report.Questions) > 0 {
			result.Reports[report.Scenario] = report
		}
	}
	return result
}

// writeReports writes the reports collected so far, so a long run keeps a
// readable partial result on disk. Alongside the per-scenario reports it stores
// what the run was measured on (the loader's conversion, the store's derivation
// state) and what it cost (per-role token usage), because a rate without its
// material and its price cannot be compared with the next run.
func writeReports(
	path string,
	reports []eval.Report,
	fingerprint eval.Fingerprint,
	loaderStats *eval.LoaderStats,
	usage []eval.ModelUsage,
	library *eval.LibraryState,
) {
	baseline := eval.NewBaseline(filepath.Base(path), reports)
	baseline.Fingerprint = fingerprint
	baseline.Loader = loaderStats
	baseline.Usage = usage
	baseline.Library = library
	// Write to a temporary file and rename: a crash mid-write used to leave a
	// truncated report that -resume then refused to parse (and silently started
	// over).
	temp := path + ".tmp"
	file, err := os.Create(temp)
	must(err)
	if saveErr := eval.SaveBaseline(file, baseline); saveErr != nil {
		_ = file.Close()
		must(saveErr)
	}
	must(file.Close())
	must(os.Rename(temp, path))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// loadEnvFile sets KEY=VALUE pairs from path, so deploy documents can resolve
// ${env:...} without shell scripting.
func loadEnvFile(path string) {
	must(host.LoadEnvFile(path))
}

// deployment is the host's built assembly pair. The alias keeps the runner's
// call sites unchanged while the wiring itself lives where the diagnostic can
// share it.
type deployment = host.Deployment

// buildAssembly returns the memory assembly either from an ad-hoc settings
// document (no inference) or from a full deploy document with a real
// inference assembly wired in.
func buildAssembly(deployPath string, recentItems int) deployment {
	built, err := host.Build(deployPath, recentItems)
	must(err)
	return built
}
