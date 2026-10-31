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

	flowmemory "github.com/GizClaw/flowcraft/backends/memory"
	"github.com/GizClaw/flowcraft/backends/memory/eval"
	evalanswer "github.com/GizClaw/flowcraft/backends/memory/eval/answer"
	"github.com/GizClaw/flowcraft/backends/memory/eval/internal/host"
	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/rerank"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func main() {
	dataset := flag.String("dataset", "locomo10.json", "LoCoMo JSON path")
	loaderName := flag.String("loader", "locomo",
		"dataset loader: locomo | longmemeval. It reads -dataset and decides which conversion runs, "+
			"which categories a report names, and which judge protocol -judge-style defaults to")
	samples := flag.Int("samples", 1, "number of conversations to run")
	questions := flag.Int("questions", 0, "max questions per conversation (0 = all)")
	categoryFilter := flag.String("categories", "", "only run these question categories (comma-separated, empty = all)")
	imageMode := flag.String("images", "native",
		"how turns with an image are ingested: annotation | native | both. "+
			"native attaches the image itself, but remote urls are rejected by the multimodal embedder, "+
			"so it needs images materialized inline first")
	noDatePrefix := flag.Bool("no-date-prefix", false,
		"convert turns without the session date prefix the loader otherwise writes into their text; "+
			"the two conversions differ in exactly this step, so running with and without it measures how much of a temporal score rests on the prefix")
	observedAt := flag.String("observed-at", "off",
		"write each session date time into the message metadata as observed_at instead of the text: "+
			"off | metadata. metadata is the dateless conversion plus the anchor, so it requires -no-date-prefix; "+
			"it is how a prefix-less production path receives its temporal anchor")
	maxItems := flag.Int("max-items", 20, "context budget items")
	maxTokens := flag.Int("max-tokens", 4096, "context budget tokens")
	recentItems := flag.Int("recent-items", 20, "assembly recent.max_items")
	deployPath := flag.String("deploy", "", "build the assembly from a deploy.yaml instead of ad-hoc settings")
	envFile := flag.String("env-file", "", "load KEY=VALUE lines before building the deployment")
	buildOnly := flag.Bool("build-only", false, "build and wire the deployment, then exit")
	answerStage := flag.Bool("answer", false, "generate an answer from the recalled context and grade it")
	resume := flag.Bool("resume", false, "skip scenarios already present in -out and retry failed ones")
	answerProvider := flag.String("answer-provider", "gateway", "provider id of the answering model")
	answerModel := flag.String("answer-model", "deepseek-flash", "name of the answering model (the deploy also declares deepseek-v4-pro, a text-only model)")
	answerContextRunes := flag.Int("answer-context-runes", 24_000, "runes of recalled context rendered into one answer prompt")
	judgeProvider := flag.String("judge-provider", "", "provider id of the judge model (default: answer provider)")
	judgeModel := flag.String("judge-model", "", "name of the judge model (default: answer model)")
	judgeStyle := flag.String("judge-style", "strict",
		"judge prompt style: strict | locomo | lme | both. both grades every answer twice -- once with the "+
			"dataset's own rubric, once with LoCoMo's -- so one report carries both columns; on the longmemeval "+
			"loader that pair is lme + locomo")
	answerStyle := flag.String("answer-style", "long",
		"answering protocol: long (product, used for strict/lenient), short (official LoCoMo protocol, for token-F1), or evidence (short, but the model quotes its evidence first)")
	temporalHint := flag.Bool("temporal-hint", false,
		"append the reference harness's category-2 suffix (\"Use DATE of CONVERSATION to answer with an approximate date.\") "+
			"to temporal questions; off by default, because it routes on the dataset's category label")
	temperature := flag.Float64("temperature", 0,
		"sampling temperature for answers and judgments; 0 makes a re-run agree with itself, a negative value "+
			"leaves it to the provider (which is how every report before 2026-10-20 was measured, and why a "+
			"configuration re-run over one library flips 6% of the graded questions)")
	answerConcurrency := flag.Int("answer-concurrency", 4,
		"questions processed in parallel (1 = sequential); only the schedule changes, never the prompts or the report order")
	prepareAll := flag.Bool("prepare-all", false,
		"ingest every scenario, run one derivation pass, then answer: lets derive.concurrency fan out across all conversations")
	skipDerive := flag.Bool("skip-derive", false,
		"answer from the derivation the workspace already holds: no ingest, no derive pass")
	maintain := flag.Bool("maintain", false,
		"run one maintenance pass per scope after the derivation: it saves the read-path overlay and fills the profile block")
	reclaim := flag.Bool("reclaim", false,
		"sweep every registered scope's projection lanes and exit: the bases and segments a compaction or a rebuild "+
			"superseded, which no manifest names, plus the whole tree of a projection layout this build does not read "+
			"and no sweep of a lane can reach. It is the sweep a maintenance pass runs beside the overlay, without "+
			"the pass, so it judges nothing and writes only deletes -- it is what shrinks a workspace an earlier build filled")
	rebuildLanes := flag.String("rebuild-lanes", "",
		"rebuild projection lanes from canonical state and exit: one repair per scope the workspace's catalog holds. "+
			"The value names the lanes (bm25, entity, vector); an empty value rebuilds every lane whose rebuild needs "+
			"no inference, and `vector` is the one that embeds every artifact again. A repair is a re-index, not a "+
			"re-derivation: watermarks and the published generation stay where they are")
	baselinePath := flag.String("baseline", "", "compare the run against this baseline JSON and fail on regression")
	baselineTolerance := flag.Float64("baseline-tolerance", 2.0, "allowed regression in percentage points")
	out := flag.String("out", "", "write full reports JSON here")
	resumeAnyway := flag.Bool("resume-anyway", false,
		"reuse scenarios from -out even when their fingerprint differs from this run")
	flag.Parse()
	if *envFile != "" {
		loadEnvFile(*envFile)
	}
	if *reclaim {
		// Before the dataset and the judge plan: a sweep names no library and
		// grades nothing, so it must not need either.
		reclaimScopes(*deployPath, *recentItems)
		return
	}
	if flagWasSet(flag.CommandLine, "rebuild-lanes") {
		// Same place as the sweep, for the same reason: a repair re-indexes
		// canonical state, so it names no library and grades nothing.
		rebuildLaneScopes(*deployPath, *recentItems, *rebuildLanes)
		return
	}
	// The LongMemEval gold is a full clause (and, for two of its types, a stated
	// preference or a refusal), which is not what the strict rubric scores, so a
	// LongMemEval run that names no judge style gets the dataset's own. An
	// explicit -judge-style still wins: grading one library with both protocols
	// is how the two are compared.
	if !flagWasSet(flag.CommandLine, "judge-style") && *loaderName == "longmemeval" {
		*judgeStyle = string(evalanswer.JudgeLongMemEval)
	}
	// Which rubrics a style names depends on the loader, so it is resolved once,
	// before the run, and both the wiring and the fingerprint read the result.
	grading, err := resolveJudgePlan(*loaderName, *judgeStyle)
	must(err)
	if *skipDerive && *prepareAll {
		must(fmt.Errorf("memory eval: -skip-derive answers from the stored derivation, so it cannot run -prepare-all's ingest and derive pass"))
	}
	if *maintain && !*prepareAll && !*skipDerive {
		must(fmt.Errorf("memory eval: -maintain runs one pass per scope after every conversation is derived, " +
			"so it needs -prepare-all (or -skip-derive, to pass over the stored derivation)"))
	}

	raw, err := os.ReadFile(*dataset)
	must(err)
	evalScope := eval.Scope{RuntimeID: "memories"}
	loaderOptions := eval.LoaderOptions{
		Scope:      evalScope,
		Budget:     corememory.Budget{MaxItems: *maxItems, MaxTokens: *maxTokens},
		Images:     *imageMode,
		Samples:    *samples,
		Dateless:   *noDatePrefix,
		ObservedAt: *observedAt,
	}
	var (
		scenarios []eval.Scenario
		stats     eval.LoaderStats
	)
	switch *loaderName {
	case "locomo":
		scenarios, stats, err = eval.LoadLoCoMo(raw, loaderOptions)
	case "longmemeval":
		scenarios, stats, err = eval.LoadLongMemEval(raw, loaderOptions)
	default:
		err = fmt.Errorf("unknown -loader %q (want locomo|longmemeval)", *loaderName)
	}
	must(err)
	fmt.Printf("dataset: loader=%s scenarios=%d turns=%d questions=%d skipped=%d",
		*loaderName, stats.Conversations, stats.Turns, stats.Questions, stats.Skipped)
	if *loaderName == "locomo" {
		fmt.Printf(" skipped_adversarial=%d", stats.SkippedAdversarial)
	}
	if stats.EvidenceUnresolved > 0 {
		fmt.Printf(" unresolved_evidence=%d", stats.EvidenceUnresolved)
	}
	if stats.ObservedAtAnchored > 0 || stats.ObservedAtUnparsed > 0 {
		// Anchor coverage is a reading on the conversion, not an assumption:
		// a session whose date did not parse carries no anchor, and a run over
		// it cannot resolve a relative-time question.
		fmt.Printf(" observed_at_anchored=%d observed_at_unparsed=%d",
			stats.ObservedAtAnchored, stats.ObservedAtUnparsed)
	}
	if *loaderName == "locomo" && (*imageMode == "native" || *imageMode == "both") {
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
		allowed, labels, err := parseCategoryFilter(*categoryFilter)
		must(err)
		for index := range scenarios {
			filtered := scenarios[index].Questions[:0]
			for _, question := range scenarios[index].Questions {
				if categoryMatches(question, allowed, labels) {
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
	options := eval.Options{
		Provenance:  resolver,
		Concurrency: *answerConcurrency,
	}
	var usage []usageRole
	if *answerStage && *temperature < 0 {
		fmt.Fprintln(os.Stderr,
			"warning: -temperature is provider-default: answers and judgments are sampled, so a re-run of this "+
				"configuration will not reproduce these numbers")
	}
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
			WithTemporalHint(*temporalHint).
			WithTemperature(temperatureSetting(*temperature))
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
			return judge.WithJudgeStyle(style).WithTemperature(temperatureSetting(*temperature))
		}
		switch {
		case grading.primary == "":
			// The official metric is computed offline from the stored answers,
			// so this run needs no judge at all.
		case grading.lenient == "":
			judge := newJudge(grading.primary)
			options.Judge = judge
			usage = append(usage, usageRole{role: "judge", counts: judge})
		default:
			primary := newJudge(grading.primary)
			lenient := newJudge(grading.lenient)
			options.Judge = primary
			options.LenientJudge = lenient
			usage = append(usage,
				usageRole{role: "judge", counts: primary},
				usageRole{role: "lenient_judge", counts: lenient})
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
	completed := map[string]eval.Report{}
	var previous partialRun
	if *resume && *out == "" {
		must(fmt.Errorf("memory eval: -resume needs -out: there is nowhere to read previous results from"))
	}
	if *resume {
		previous = loadPartialRun(*out)
		completed = previous.Reports
	}
	pending := 0
	if *prepareAll && !*skipDerive {
		var err error
		pending, err = ingestAllScenarios(context.Background(), deployment.Memory, scenarios, completed)
		must(err)
	}
	// The rule evidence recall is graded by is a property of the store the run grades,
	// and ingest is what writes the dataset turn ids: measured before the ingest phase,
	// a fresh workspace would be stamped committed-text while this run matches every hit
	// by turn id. So the fingerprint is fixed once the store is prepared -- and with it
	// the resume check, which is why the previous run is read above but compared below,
	// and why the derivation pass waits until after both. A run without -prepare-all
	// ingests inside the loop, so its label can only describe the store as it found it.
	evidenceRule, taggedConversations := evidenceMatching(context.Background(), resolver, scenarios)
	if taggedConversations < len(scenarios) {
		fmt.Fprintf(os.Stderr,
			"warning: %d of %d conversations carry no dataset turn ids (evidence recall is matched by committed\n"+
				"  text): a rendering difference between the loader and the store hides a turn, and the two rules do\n"+
				"  not score the same. Ingest writes the ids on the first commit; a store that already holds these\n"+
				"  conversations replays their commits, so a fresh workspace is the only way to bring them up to date.\n",
			len(scenarios)-taggedConversations, len(scenarios))
	}
	fingerprint := eval.Fingerprint{
		Revision:     buildRevision(),
		Dataset:      eval.DatasetDigest(raw),
		Questions:    stats.Questions,
		PolicyDigest: deployment.Memory.PolicyDigest(),
		Values: map[string]string{
			"loader":               loaderFingerprint(*loaderName),
			"images":               imageFingerprint(*loaderName, *imageMode),
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
			"recent_items":  strconv.Itoa(deployment.Recent.MaxItems),
			"recent_tokens": strconv.Itoa(deployment.Recent.MaxTokens),
			"judge_style":   *judgeStyle,
			"answer_model":  *answerProvider + "/" + *answerModel,
			"answer_prompt": answerPromptVersion(*answerStyle),
			"answer_style":  *answerStyle,
			"temporal_hint": strconv.FormatBool(*temporalHint),
			// Whether the answer and judge calls pinned their sampling. The
			// provider's default is a draw: one configuration re-run over one
			// library flipped 93 of 1540 graded questions, so two reports that
			// differ here are not comparable at sub-pp resolution.
			"temperature":          temperatureFingerprint(*temperature),
			"answer_context_runes": strconv.Itoa(*answerContextRunes),
			"judge_model":          judgeFingerprint(*judgeProvider, *judgeModel, *answerProvider, *answerModel),
			// Whether the loader wrote each session's date into its turns'
			// text. The date is dataset content, but carrying it in the
			// prompt is the loader's choice, so the two configurations
			// cannot share a fingerprint.
			"date_prefix": strconv.FormatBool(!*noDatePrefix),
			// Whether the loader wrote each session date into the messages
			// as their observed_at metadata instead of their text. The text
			// prefix and the metadata anchor are alternative renderings of
			// the same fact (the loader refuses to write both), so this
			// key and date_prefix move together: `metadata` here is the
			// prefix-less shape a production path gets its anchor through.
			"observed_at": *observedAt,
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
			"rerank": onOff(deployment.Rerank),
			// Whether a maintenance pass ran before this run answered. The
			// pass saves the read-path overlay and fills the profile block, so
			// a run that took the pass and one that did not measure different
			// stores -- the retrieval summary names the block's settings, not
			// whether anything ran a pass over them.
			"maintain": onOff(*maintain),
			// The retrieval-side configuration the policy digest does not
			// cover -- lane weights and fusion, the bm25 parameters, source
			// quotes, rerank -- read back off the built assembly, so the
			// document's values and the defaults are recorded together. A
			// retrieval knob flipped in the document or in code lands here,
			// and two retrieval configurations cannot share a fingerprint.
			"retrieval":    deployment.Memory.RetrievalSummary(),
			"judge_prompt": grading.promptVersion(),
			"schedule":     scheduleName(*prepareAll),
			// A run that answers from the stored derivation measures a
			// different thing than one that derives first, so the fingerprint
			// keeps the two apart (and -resume refuses to mix them).
			"derive": deriveMode(*skipDerive),
			// The deploy document, byte for byte: it also pins whatever the
			// values above do not name -- further assembly settings, the
			// model declarations -- so editing deploy.yaml cannot change
			// results under an unchanged fingerprint.
			"deploy": deployDigest(*deployPath),
			// -answer-concurrency is deliberately absent: it changes only the
			// schedule, so it must not block resuming a run.
		},
	}
	// A conversion that dropped a gold reference (the haystack does not hold the
	// session or turn the dataset names) is a different measurement, but the key
	// is only written when it happens: a constant "0" would relabel every report
	// measured before the field existed and block -resume on them.
	if stats.EvidenceUnresolved > 0 {
		fingerprint.Values["unresolved_evidence"] = strconv.Itoa(stats.EvidenceUnresolved)
	}
	fmt.Printf("fingerprint: %s\n", fingerprint)
	// Say what the store holds once the ingest phase has landed: the report's
	// own library field is read back at the end of the run, so this line is the
	// state the numbers below build on.
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
	if *resume && !previous.Fingerprint.Empty() && !previous.Fingerprint.Equal(fingerprint) {
		if !*resumeAnyway {
			must(fmt.Errorf("memory eval: %s was produced under a different configuration\n"+
				"  recorded: %s\n  current:  %s\n"+
				"re-run without -resume, or pass -resume-anyway to mix them on purpose",
				*out, previous.Fingerprint, fingerprint))
		}
		fmt.Fprintf(os.Stderr, "warning: resuming across fingerprints (recorded: %s; current: %s)\n",
			previous.Fingerprint, fingerprint)
	}
	// The derivation pass runs after the fingerprint (and the resume check
	// above) so a run that would refuse to resume does not pay for it first.
	if pending > 0 {
		fmt.Printf("prepared %d scenarios; running one derivation pass\n", pending)
		must(deriveAllScenarios(context.Background(), deployment.Memory))
	}
	if deployment.Profile && !*maintain {
		fmt.Fprintf(os.Stderr,
			"warning: the deploy document enables the profile block and no maintenance pass runs: "+
				"the block is served empty, so this run measures the blockless read path\n")
	}
	if *maintain {
		scopes := make([]corememory.Scope, 0, len(scenarios))
		for _, scenario := range scenarios {
			scopes = append(scopes, scenario.Scope.MemoryScope())
		}
		maintained, err := eval.Maintain(context.Background(), deployment.Memory, scopes)
		must(err)
		fmt.Printf("maintained %d scopes; the read-path overlay and the profile block are what a pass wrote, "+
			"and the pass reclaimed %d projection objects and %d document chunks\n",
			maintained.Scopes, maintained.ReclaimedProjectionObjects, maintained.ReclaimedChunks)
		fmt.Printf("maintained findings=%d (arbiter %d); arbiter asked=%d remembered=%d skipped=%d "+
			"replaced=%d both-current=%d undecided=%d evicted=%d\n",
			maintained.Supersedes, maintained.ArbiterSupersedes,
			maintained.ArbiterAsked, maintained.ArbiterRemembered, maintained.ArbiterSkipped,
			maintained.ArbiterReplaced, maintained.ArbiterBothCurrent, maintained.ArbiterUndecided,
			maintained.ArbiterEvicted)
	}
	reports := make([]eval.Report, 0, len(scenarios))
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
	var evidenceTotal, evidenceFound, evidenceRaw, evidenceProvenance, ungradedAnswers int
	var lenientAnswers, lenientHits, lenientUngraded int
	var sessionTotal, sessionFound int
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
		sessionTotal += report.SessionTotal
		sessionFound += report.SessionFound
		ungradedAnswers += report.UngradedAnswers
		lenientAnswers += report.LenientAnswers
		lenientHits += report.LenientHits
		lenientUngraded += report.LenientUngraded
		latency += report.MeanLatency * time.Duration(len(report.Questions))
		mergeCategories(byCategory, report.ByCategory)
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
	if sessionTotal > 0 {
		// Session-level evidence is a separate unit from the turn-level figure
		// above: a session counts once, and a question hits when every gold
		// session was recalled. Adding the two would be a rate over two
		// denominators.
		fmt.Printf("session recall: sessions=%d/%d recall=%.4f questions_with_every_gold_session=%.4f\n",
			sessionFound, sessionTotal, float64(sessionFound)/float64(sessionTotal),
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
	if lenientAnswers > 0 || lenientUngraded > 0 {
		rate := 0.0
		if lenientAnswers > 0 {
			rate = float64(lenientHits) / float64(lenientAnswers)
		}
		fmt.Printf("lenient judge: graded=%d ungraded=%d answer_hits=%d answer_rate=%.4f\n",
			lenientAnswers, lenientUngraded, lenientHits, rate)
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
		name := fmt.Sprintf("category %d", category)
		if stats.Label != "" {
			name += " (" + stats.Label + ")"
		}
		line := fmt.Sprintf("%s: questions=%4d hits=%4d hit_rate=%.4f",
			name, stats.Questions, stats.Hits, float64(stats.Hits)/float64(stats.Questions))
		if stats.Answered > 0 {
			line += fmt.Sprintf(" answer_hits=%4d answer_rate=%.4f",
				stats.AnswerHits, float64(stats.AnswerHits)/float64(stats.Answered))
		}
		if stats.LenientAnswered > 0 {
			line += fmt.Sprintf(" lenient_hits=%4d lenient_rate=%.4f",
				stats.LenientAnswerHits, float64(stats.LenientAnswerHits)/float64(stats.LenientAnswered))
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

// flagWasSet reports whether the command line named a flag, which is how a
// per-loader default tells an explicit choice from an unset one.
func flagWasSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == name {
			set = true
		}
	})
	return set
}

// parseCategoryFilter splits -categories into the ids and the labels a question
// may match. A token that is not a number is a category label: LoCoMo numbers
// its categories, LongMemEval names them ("temporal-reasoning"), and a run
// should not have to know which numbering it is filtering on to ask for one
// question type.
func parseCategoryFilter(value string) (map[int]struct{}, map[string]struct{}, error) {
	ids := map[int]struct{}{}
	labels := map[string]struct{}{}
	for _, token := range strings.Split(value, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if number, err := strconv.Atoi(token); err == nil {
			ids[number] = struct{}{}
			continue
		}
		labels[strings.ToLower(token)] = struct{}{}
	}
	if len(ids) == 0 && len(labels) == 0 {
		return nil, nil, fmt.Errorf("memory eval: -categories names no category")
	}
	return ids, labels, nil
}

// categoryMatches reports whether a question falls under -categories. The id
// and the label are both accepted, so the flag keeps working across the two
// datasets' numbering schemes.
func categoryMatches(question eval.Question, ids map[int]struct{}, labels map[string]struct{}) bool {
	if _, ok := ids[question.Category]; ok {
		return true
	}
	_, ok := labels[strings.ToLower(strings.TrimSpace(question.CategoryLabel))]
	return ok
}

// loaderFingerprint names the conversion a run used. LoCoMo keeps the value
// reports were measured under before -loader existed, so those reports still
// compare.
func loaderFingerprint(loader string) string {
	if loader == "longmemeval" {
		return "longmemeval"
	}
	return "locomo10"
}

// imageFingerprint names the image policy, which only the LoCoMo conversion
// has. LongMemEval carries no images, so two runs over it that differ only in
// -images measure the same conversion and must not be split by the fingerprint.
func imageFingerprint(loader, mode string) string {
	if loader == "longmemeval" {
		return "n/a"
	}
	return mode
}

// judgePlan is what -judge-style resolves to on one loader: the rubric that
// scores the report's answer columns, and the optional second, leaderboard
// rubric whose verdicts land in the lenient columns.
type judgePlan struct {
	primary evalanswer.JudgeStyle
	lenient evalanswer.JudgeStyle
}

// resolveJudgePlan maps a -judge-style value onto the prompts a run calls.
// "both" is a pair of rubrics rather than one, and which pair depends on the
// loader: LoCoMo's gold is a short topic phrase that its strict rubric scores,
// while a LongMemEval gold is a full clause -- and 60 of its 500 rows are
// graded on a stated preference or on a refusal, which the strict fact rubric
// cannot see. Pairing strict with locomo there would grade every such row as a
// miss, so the strict half has to be the dataset's own rubric: comparing one
// library under both protocols is lme + locomo.
func resolveJudgePlan(loader, style string) (judgePlan, error) {
	switch style {
	case "none":
		// The official metric is computed offline from the stored answers.
		return judgePlan{}, nil
	case "strict":
		return judgePlan{primary: evalanswer.JudgeStrict}, nil
	case "locomo":
		return judgePlan{primary: evalanswer.JudgeLocoMo}, nil
	case "lme":
		return judgePlan{primary: evalanswer.JudgeLongMemEval}, nil
	case "both":
		if loader == "longmemeval" {
			return judgePlan{primary: evalanswer.JudgeLongMemEval, lenient: evalanswer.JudgeLocoMo}, nil
		}
		return judgePlan{primary: evalanswer.JudgeStrict, lenient: evalanswer.JudgeLocoMo}, nil
	}
	return judgePlan{}, fmt.Errorf("unknown -judge-style %q (want strict|locomo|lme|both)", style)
}

// promptVersion names the rubric that scored the answer columns. The lenient
// half has one prompt and does not need naming; a run that names no judge keeps
// naming the v2 prompt, because that is what its stored reports recorded.
func (plan judgePlan) promptVersion() string {
	return evalanswer.PromptVersion(plan.primary)
}

// mergeCategories adds one scenario's category table into the run's. The
// lenient halves ride along: under -judge-style both every scenario line and
// every category line carries a second rate, and summing only the strict halves
// would print an empty lenient column over a report that holds both.
func mergeCategories(into, from map[int]eval.CategoryStats) {
	for category, value := range from {
		current := into[category]
		current.Label = value.Label
		current.Questions += value.Questions
		current.Hits += value.Hits
		current.Answered += value.Answered
		current.AnswerHits += value.AnswerHits
		current.LenientAnswered += value.LenientAnswered
		current.LenientAnswerHits += value.LenientAnswerHits
		into[category] = current
	}
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

// temperatureSetting renders the -temperature flag as the pointer the answer
// model takes: a negative value means "leave it to the provider".
func temperatureSetting(value float64) *float64 {
	if value < 0 {
		return nil
	}
	return &value
}

// temperatureFingerprint names what the sampling was pinned to. "provider"
// means it was not pinned at all, which is a different measurement from any
// number.
func temperatureFingerprint(value float64) string {
	if value < 0 {
		return "provider"
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
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
//
// It reads the store, so it must run once the store holds what the run grades:
// after the ingest phase, never before it. A fresh workspace carries no ids
// until ingest writes them, and re-ingesting a store that already holds the
// sessions replays their commits instead of tagging them, so a measurement
// taken earlier names a rule the run does not use.
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

// ingestAllScenarios commits every scenario that still needs measuring and
// reports how many it prepared. -prepare-all ingests before the fingerprint is
// fixed, because ingest is what writes the dataset turn ids the evidence label
// is measured from; the caller then pays for a single derivation pass over the
// result (deriveAllScenarios) once the run knows it is keeping this output.
func ingestAllScenarios(ctx context.Context, runner eval.Runner, scenarios []eval.Scenario, completed map[string]eval.Report) (int, error) {
	pending := 0
	for _, scenario := range scenarios {
		if _, done := completed[scenario.Name]; done {
			continue
		}
		if err := eval.Ingest(ctx, runner, scenario); err != nil {
			return pending, err
		}
		pending++
	}
	return pending, nil
}

// deriveAllScenarios runs one derivation pass over everything ingested so far.
// The worker fans out over the conversations that have pending commits, which
// is what makes derive.concurrency worth setting; per-scenario runs only ever
// have one.
//
// A derive pass is resumable: a conversation that fails keeps its watermark, so
// retrying the pass picks it up where it stopped. Parallel derivation raises the
// transient provider error rate, so the retry is part of running this way.
func deriveAllScenarios(ctx context.Context, runner eval.Runner) error {
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

// reclaimScopes sweeps every scope the workspace's catalog holds and prints
// what each lane removed, then the total.
//
// The sweep is per lane and per scope because that is what the state is: a
// lane's manifest is the only pointer to the objects it published, and a scope
// is what the lanes partition by. It reads no facts and writes no overlay, so
// it is safe on a workspace whose derivation is the one a report was measured
// on: what it removes is state no manifest names.
//
// The catalog is the authority on which scopes the workspace holds, not the
// deploy document's scope list: a document names what a run will write, and a
// sweep has to reach the scopes an earlier document wrote.
func reclaimScopes(deployPath string, recentItems int) {
	if deployPath == "" {
		must(fmt.Errorf("memory eval: -reclaim sweeps the workspace a deploy document binds, so it needs -deploy"))
	}
	deployment := buildAssembly(deployPath, recentItems)
	defer deployment.Close()
	ctx := context.Background()
	catalog := deployment.Memory.Catalog()
	if catalog == nil {
		must(fmt.Errorf("memory eval: -reclaim needs the workspace's scope catalog, and this deployment has none"))
	}
	scopes, err := catalog.List(ctx)
	must(err)
	if len(scopes) == 0 {
		fmt.Println("reclaim: the workspace catalog holds no scope, so there is nothing to sweep")
		return
	}
	started := time.Now()
	var total maintain.ReclaimResult
	for _, scope := range scopes {
		result, err := deployment.Memory.ReclaimProjections(ctx, scope)
		must(err)
		fmt.Printf("reclaim: scope=%s lanes=%d bases=%d segments=%d legacy_keys=%d legacy_layouts=%s\n",
			scope.String(), result.Lanes, result.Bases, result.Segments,
			result.LegacyKeys, strings.Join(result.LegacyLayouts, ","))
		total.Lanes += result.Lanes
		total.Bases += result.Bases
		total.Segments += result.Segments
		total.LegacyKeys += result.LegacyKeys
	}
	fmt.Printf("reclaim: scopes=%d objects=%d (bases=%d segments=%d legacy_keys=%d) elapsed=%s\n",
		len(scopes), total.Bases+total.Segments, total.Bases, total.Segments, total.LegacyKeys,
		time.Since(started).Round(time.Millisecond))
}

// rebuildLaneScopes re-indexes the named projection lanes of every scope the
// workspace's catalog holds, and prints what each repair published.
//
// It is what carries a workspace across a change to a lane's producer or to the
// form of what it stores: the vector lane's state lives under a storage name
// that covers both, so a build with a new form reads a name nothing was ever
// written under and leaves the generation an earlier build filled beside it,
// unread. A repair re-indexes canonical state -- the messages, the facts of the
// active generation, the chunks of the active document builds -- through the
// shapes the derivation worker writes, so nothing is derived again and no
// deriver runs; the vector lane is the one whose repair embeds every artifact,
// which is why it is named rather than rebuilt by default.
//
// The catalog is the authority on which scopes the workspace holds, for the
// reason the sweep gives: a deploy document names what a run will write, and a
// repair has to reach what an earlier document wrote.
func rebuildLaneScopes(deployPath string, recentItems int, lanes string) {
	if deployPath == "" {
		must(fmt.Errorf("memory eval: -rebuild-lanes rebuilds the lanes a deploy document binds, so it needs -deploy"))
	}
	names := splitLaneNames(lanes)
	deployment := buildAssembly(deployPath, recentItems)
	defer deployment.Close()
	ctx := context.Background()
	catalog := deployment.Memory.Catalog()
	if catalog == nil {
		must(fmt.Errorf("memory eval: -rebuild-lanes needs the workspace's scope catalog, and this deployment has none"))
	}
	scopes, err := catalog.List(ctx)
	must(err)
	if len(scopes) == 0 {
		fmt.Println("rebuild: the workspace catalog holds no scope, so there is nothing to rebuild")
		return
	}
	started := time.Now()
	artifacts, laneCount := 0, 0
	for _, scope := range scopes {
		result, err := deployment.Memory.Repair(ctx, flowmemory.RepairRequest{Scope: scope, Lanes: names})
		must(err)
		artifacts += result.Artifacts
		for _, lane := range result.Lanes {
			laneCount++
			fmt.Printf("rebuild: scope=%s lane=%s artifacts=%d stale_before=%t source=%s build=%s\n",
				scope.String(), lane.Name, lane.Artifacts, lane.StaleBefore,
				shortDigest(lane.SourceDigest), shortDigest(lane.BuildDigest))
		}
	}
	fmt.Printf("rebuild: scopes=%d lanes=%d artifacts=%d elapsed=%s\n",
		len(scopes), laneCount, artifacts, time.Since(started).Round(time.Millisecond))
}

// splitLaneNames parses -rebuild-lanes. An empty value names no lane, which is
// how a repair asks for every configured lane whose rebuild needs no inference.
func splitLaneNames(value string) []string {
	var names []string
	for _, token := range strings.Split(value, ",") {
		if token = strings.TrimSpace(token); token != "" {
			names = append(names, token)
		}
	}
	return names
}

// shortDigest names a cumulative digest by its first bytes: a repair prints one
// per lane per scope, and the reading is compared between runs rather than with
// anything outside the digest's own comparison.
func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
