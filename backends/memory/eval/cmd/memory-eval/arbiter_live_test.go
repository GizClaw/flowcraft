package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/backends/memory/eval/internal/host"
	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// The live half of the arbiter reading, and the half the corpus cannot answer:
// the corpus says what a model decides about pairs someone wrote, this says
// whether the pass reaches it at all -- extractor, candidate pairs, decision
// call, overlay -- on facts the deployment's own extractor produced, in a
// language its two rules cannot read.
//
// liveDeployDocument is the probe document this lane builds. It is not
// deploy.yaml: that document reaches the gateway and OpenAI, so building it
// resolves two credentials, and its memories block leaves the arbiter out.
//
// One model extracts and one judges the pairs, both from the document's single
// provider. The names are not repeated here: a lane that named a model the
// document does not would be measuring a deployment nobody can build.
const liveDeployDocument = "../../deploy-arbiter.yaml"

// The two Chinese conversations the probe derives. The first states an update
// with wording that moved with it -- the shape the vocabulary cannot name and
// the similarity rule scores at 0.29, so both shipped rules leave the old value
// current. The second states two things that can hold at once, which is what a
// third rule must not turn into a finding.
//
// Neither statement of a pair carries a date, which is the shape the corpus is
// in and, as the reading showed, the shape the arbiter calls an update: a
// statement that says "on 3 May 2018" reads as a record of that day, and a
// record does not stop being true, so a dated pair is answered both-current.
// The pair is ordered the way the pass orders it -- by event time when a
// statement states one, and by when it was said otherwise -- and a pair of one
// dated and one undated statement is ordered by that mix rather than by what
// the two statements say, which is why both are undated here.
const (
	liveArbiterUpdateOlder  = "郑凯住在柏林。"
	liveArbiterUpdateNewer  = "郑凯搬到了上海。"
	liveArbiterControlOlder = "郑凯养了一只叫豆豆的猫。"
	liveArbiterControlNewer = "郑凯参加了一个合唱团。"
)

// TestArbiterJudgesDerivedChineseTurns derives two Chinese conversations with
// the real extractor and runs one maintenance pass over them, so what it
// measures is the whole path: the facts the extractor wrote, the pairs the pass
// proposed from them, the call the arbiter made, and the overlay that call
// wrote. It prints the derived facts (with the predicates the vocabulary is
// folded over), the findings with their reason, and the pass's counters --
// asked, undecided, skipped -- because those are what a deployment reads to
// tell an arbiter that is working from one that is paying for nothing.
func TestArbiterJudgesDerivedChineseTurns(t *testing.T) {
	loadEnvFile(liveEnvFile(t))
	if os.Getenv("MEMORY_EVAL_LIVE") != "1" {
		t.Skip("set MEMORY_EVAL_LIVE=1 to run the credentialed live pass")
	}
	key := strings.TrimSpace(os.Getenv("GATEWAY_API_KEY"))
	if key == "" {
		t.Skip("GATEWAY_API_KEY is empty")
	}
	ctx := context.Background()
	// The document's workspace root is a variable so a build writes into the
	// directory this lane deletes, never into the checkout.
	t.Setenv("MEMORY_EVAL_WORKSPACE", filepath.Join(t.TempDir(), "workspace"))
	deployment, err := host.Build(liveDeployDocument, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deployment.Close)
	assembly := deployment.Memory

	scope := corememory.Scope{RuntimeID: "memories"}
	conversations := []struct {
		id     string
		older  string
		newer  string
		update bool
	}{
		{"zh-update", liveArbiterUpdateOlder, liveArbiterUpdateNewer, true},
		{"zh-control", liveArbiterControlOlder, liveArbiterControlNewer, false},
	}
	for _, conversation := range conversations {
		for index, text := range []string{conversation.older, conversation.newer} {
			turn := corememory.Turn{
				Scope: scope, ConversationID: conversation.id,
				IdempotencyKey: fmt.Sprintf("%s-turn-%d", conversation.id, index),
				Messages:       []coremessage.Message{liveUserMessage(text)},
			}
			if err := assembly.CommitTurn(ctx, turn); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The extraction lane samples provider errors, so a pass is retried from
	// the stored watermarks rather than read as a fact about the arbiter.
	for attempt := 1; ; attempt++ {
		if err := eval.Derive(ctx, assembly); err == nil {
			break
		} else if attempt == 3 {
			t.Fatalf("derive: %v", err)
		} else {
			t.Logf("derive attempt %d/3 failed, retrying from the stored watermarks: %v", attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}

	facts := make(map[string][]factview.Fact, len(conversations))
	for _, conversation := range conversations {
		listed, err := assembly.Facts().List(ctx, scope, conversation.id, factview.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		facts[conversation.id] = listed
		for _, fact := range listed {
			t.Logf("%-11s %s text=%q predicate=%q entities=%v event=%s temporal=%q",
				conversation.id, fact.ID, fact.Text, fact.Predicate, fact.Entities,
				fact.EventTime.Format(time.RFC3339), fact.TemporalDetail)
		}
	}

	started := time.Now()
	plan, err := assembly.Maintain(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pass %s: supersedes=%d decays=%d arbiter asked=%d undecided=%d skipped=%d",
		time.Since(started).Round(time.Millisecond), len(plan.Supersedes), len(plan.Decays),
		plan.ArbiterAsked, plan.ArbiterUndecided, plan.ArbiterSkipped)
	findings := make(map[string]maintain.Supersede, len(plan.Supersedes))
	for _, finding := range plan.Supersedes {
		findings[finding.FactID] = finding
		t.Logf("  %s -> %s reason=%s relation=%s confidence=%.2f",
			finding.FactID, finding.SupersededBy, finding.Reason, finding.Relation, finding.Confidence)
	}
	if plan.ArbiterAsked == 0 {
		t.Fatalf("the pass proposed no pair; facts=%v", facts)
	}

	older := factSaying(t, facts["zh-update"], "柏林")
	finding, replaced := findings[older.ID]
	if !replaced {
		t.Fatalf("the older update fact is still current: %q", older.Text)
	}
	if finding.Reason != maintain.ReasonArbiter {
		t.Errorf("the update was found by %q, not by the arbiter", finding.Reason)
	}
	if finding.Confidence <= 0 {
		t.Errorf("the arbiter's finding records no confidence: %+v", finding)
	}
	newer := factSaying(t, facts["zh-update"], "上海")
	if finding.SupersededBy != newer.ID {
		t.Errorf("the update names %s, want the Shanghai fact %s", finding.SupersededBy, newer.ID)
	}
	for _, fact := range facts["zh-control"] {
		if finding, replaced := findings[fact.ID]; replaced {
			t.Errorf("control fact %q was superseded by %s (reason %s)",
				fact.Text, finding.SupersededBy, finding.Reason)
		}
	}
	// The control pair is asked and answered both-current: a pass that asked
	// nothing about it cannot say the arbiter declined it.
	if plan.ArbiterUndecided == 0 {
		t.Errorf("no pair was left current; the control was not put to the arbiter")
	}
}

// factSaying returns the one derived fact whose text carries a phrase. It fails
// when the extractor wrote none or more than one, because either way the
// assertion below would be about a different fact than the probe meant.
func factSaying(t *testing.T, facts []factview.Fact, phrase string) factview.Fact {
	t.Helper()
	var found []factview.Fact
	for _, fact := range facts {
		if strings.Contains(fact.Text, phrase) {
			found = append(found, fact)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d derived facts say %q: %v", len(found), phrase, facts)
	}
	return found[0]
}

func liveUserMessage(text string) coremessage.Message {
	return coremessage.Message{
		Role:    coremessage.RoleUser,
		Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text}}},
	}
}
