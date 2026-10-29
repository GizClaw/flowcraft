package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/eval/internal/host"
	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// The live half of the relation reading: the same labelled corpus, scored
// against the third rule rather than against the two shipped ones. The static
// reading says the vocabulary and the similarity measure both return nothing on
// Chinese; this one says what a model reading the same pairs returns, and what
// that costs.
//
// It is a test rather than a flag of the command because it needs a credential
// and a network: the command stays the offline measurement, and this lane runs
// only where the repository's .env is present.
const (
	liveArbiterProvider = "gateway"
	liveArbiterModel    = "azure-decision"
)

// liveDeployDocument is the probe document both arbiter lanes build: one
// provider serves extraction, embedding, and the decision surface, so a
// reading needs one key. It is not deploy.yaml, and the difference is the
// reading: deploy.yaml pins each surface to the provider that serves it, and
// building it would need every credential a harness run needs.
const liveDeployDocument = "../../deploy-arbiter.yaml"

// liveWorkspace sets the probe document's workspace root. A lane must write
// into the directory it deletes, and the root is a variable in the document
// so that no build can fall back to writing into the checkout.
func liveWorkspace(t *testing.T) {
	t.Helper()
	t.Setenv("MEMORY_EVAL_WORKSPACE", filepath.Join(t.TempDir(), "workspace"))
}

// liveEnvFile resolves the credentials file the live lanes need: the repository
// root, or the older location beside the eval module.
func liveEnvFile(t *testing.T) string {
	t.Helper()
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	candidates := []string{
		filepath.Join(workdir, "..", "..", ".env"),
		filepath.Join(workdir, "..", "..", "..", "..", "..", ".env"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skipf("no credentials file in %v", candidates)
	return ""
}

// liveDeployment builds the probe document the arbiter lanes measure, and
// returns the inference assembly it wires: one provider, one credential, the
// decision surface the arbiter asks. The document is built rather than its
// assembly wired by hand so this reading can be reproduced by building the
// same file, and so the models it asks for are the ones the file declares.
func liveDeployment(t *testing.T) *host.Deployment {
	t.Helper()
	deployment, err := host.Build(liveDeployDocument, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deployment.Close)
	if deployment.Inference == nil {
		t.Fatalf("%s wires no inference assembly", liveDeployDocument)
	}
	return &deployment
}

// corpusPairs turns the labelled conversations into the pairs the pass would
// propose: the first turn is the older fact, the second is the one that may
// replace it. A scenario whose turns carry no relation label is a negative
// control -- two statements that can both hold.
func corpusPairs() ([]maintain.Pair, []bool) {
	pairs := make([]maintain.Pair, 0, len(zhScenarios))
	updates := make([]bool, 0, len(zhScenarios))
	for _, scenario := range zhScenarios {
		if len(scenario.Turns) != 2 {
			continue
		}
		older := factview.Fact{
			ID: scenario.ID + "-1", Text: scenario.Turns[0].Says,
			EventTime: zhTime(scenario.Turns[0].At),
		}
		newer := factview.Fact{
			ID: scenario.ID + "-2", Text: scenario.Turns[1].Says,
			EventTime: zhTime(scenario.Turns[1].At),
		}
		pairs = append(pairs, maintain.Pair{Conversation: scenario.ID, Older: older, Newer: newer})
		updates = append(updates, scenario.Turns[1].Want != "")
	}
	return pairs, updates
}

func zhTime(day string) time.Time {
	parsed, err := time.Parse("2006-01-02", day)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// TestLiveArbiterOnTheRelationCorpus scores the arbiter against the same
// labelled pairs the static reading scores the rules against: every update
// should be called replaced, and no repeatable pair should be. It prints the
// per-pair verdict, the two rates, and the tokens one pass spent, because the
// reason the arbiter is off by default is what it costs.
func TestLiveArbiterOnTheRelationCorpus(t *testing.T) {
	if err := host.LoadEnvFile(liveEnvFile(t)); err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(os.Getenv("GATEWAY_API_KEY"))
	if key == "" {
		t.Skip("GATEWAY_API_KEY is empty")
	}
	pairs, updates := corpusPairs()
	if len(pairs) != len(zhScenarios) {
		t.Fatalf("%d pairs for %d scenarios", len(pairs), len(zhScenarios))
	}
	wanted := 0
	for _, update := range updates {
		if update {
			wanted++
		}
	}
	liveWorkspace(t)
	recorder := &recordingDecisions{inner: liveDeployment(t).Inference}
	arbiter := &maintain.DecisionArbiter{
		Decisions: recorder, Model: model.ModelRef{
			ID: model.ModelID{Provider: liveArbiterProvider, Name: liveArbiterModel},
		},
	}
	started := time.Now()
	judgments, err := arbiter.Adjudicate(context.Background(), pairs)
	if err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	hits, falsePositives := 0, 0
	for index, judgment := range judgments {
		replaced := judgment.Verdict == maintain.VerdictReplaced
		if updates[index] && replaced {
			hits++
		}
		if !updates[index] && replaced {
			falsePositives++
		}
		t.Logf("%-32s update=%-5t verdict=%-12s confidence=%.2f",
			pairs[index].Conversation, updates[index], judgment.Verdict, judgment.Confidence)
	}
	t.Logf("arbiter %s: %d/%d updates detected, %d false positive(s) over %d controls, %d tokens, %s",
		arbiter.Identity(), hits, wanted, falsePositives, len(pairs)-wanted,
		recorder.usage.TotalTokens, time.Since(started).Round(time.Millisecond))
}

// recordingDecisions keeps the usage the arbiter's caller would otherwise have
// to ask for: the reading reports what one pass costs, and the pass itself only
// reads verdicts.
type recordingDecisions struct {
	inner *inference.Assembly
	usage inference.Usage
}

func (recorder *recordingDecisions) Decision(
	ctx context.Context,
	ref model.ModelRef,
	request inference.DecisionRequest,
) (inference.DecisionResponse, error) {
	response, err := recorder.inner.Decision(ctx, ref, request)
	if err == nil {
		recorder.usage.InputTokens += response.Usage.InputTokens
		recorder.usage.TotalTokens += response.Usage.TotalTokens
	}
	return response, err
}
