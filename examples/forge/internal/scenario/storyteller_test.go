package scenario_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
)

// The narrated raid's rules, offline. A story has no winner to announce and no
// transcript to replay: what it has is a blueprint, a set of speakers a phase allows,
// and a moment to hand back to the reader.
const storyteller = "../../scenarios/raids/storyteller"

const (
	storySeedConfig = `{"backend":"starlark","path":"rules/seed.star",` +
		`"reads":["story","messages"],"writes":["story"]}`
	storyPrepareConfig = `{"backend":"starlark","path":"rules/prepare.star",` +
		`"reads":["story","messages"],"writes":["story","plan","plan_messages"]}`
	storyPublishConfig = `{"backend":"starlark","path":"rules/publish.star",` +
		`"reads":["story","narrated","spoken"],"writes":["story","prompt"]}`
)

// story deals one arc from the reader's opening line.
func story(t *testing.T, opening string) graph.Value {
	t.Helper()
	writes := runRuleIn(t, storyteller, "rules/seed.star", storySeedConfig, map[string]graph.Value{
		"story":    graph.JSON([]byte(`null`)),
		"messages": speechValue(opening),
	})
	return writes["story"]
}

// storyWith poses an arc at a phase, without playing the beats that would reach it.
func storyWith(t *testing.T, base graph.Value, extra map[string]any) graph.Value {
	t.Helper()
	raw, err := base.JSON()
	if err != nil {
		t.Fatalf("story: %v", err)
	}
	return gameFrom(t, raw, extra)
}

// TestStoryDealsABlueprint: the arc is data, and the first phase starts with the
// speaker the blueprint names.
func TestStoryDealsABlueprint(t *testing.T) {
	arc := decodeGame(t, story(t, "我们四个在破庙里避雨。"))
	if arc["started"] != true {
		t.Fatalf("started = %v", arc["started"])
	}
	if arc["phase"] != "threshold" {
		t.Fatalf("phase = %v", arc["phase"])
	}
	phases, _ := arc["phases"].(map[string]any)
	if len(phases) < 10 {
		t.Fatalf("phases = %d, want the whole arc", len(phases))
	}
	spoken, _ := arc["phase_spoken"].([]any)
	if len(spoken) != 0 {
		t.Fatalf("phase_spoken = %v, want none", spoken)
	}
	if arc["opening"] != "我们四个在破庙里避雨。" {
		t.Fatalf("opening = %v", arc["opening"])
	}
}

// TestStoryCandidatesFollowThePhase: the rules own the list the scheduler picks from.
func TestStoryCandidatesFollowThePhase(t *testing.T) {
	base := story(t, "我们四个在破庙里避雨。")
	for name, test := range map[string]struct {
		spoken  []any
		want    []string
		handoff bool
	}{
		"first beat is the blueprint's first speaker": {nil, []string{"narrator"}, false},
		"unused speakers come next":                   {[]any{"narrator"}, []string{"wukong", "tangseng", "bajie"}, false},
		"the beat before the handoff goes to the narrator": {
			[]any{"narrator", "wukong"}, []string{"narrator"}, true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			writes := runRuleIn(t, storyteller, "rules/prepare.star", storyPrepareConfig,
				map[string]graph.Value{
					"story":         storyWith(t, base, map[string]any{"phase_spoken": test.spoken}),
					"messages":      speechValue("我们四个在破庙里避雨。"),
					"plan":          graph.JSON([]byte(`{}`)),
					"plan_messages": graph.MessagesValue(graph.MessagesFromSlice(nil)),
				})
			arc := decodeGame(t, writes["story"])
			candidates, _ := arc["candidates"].([]any)
			got := make([]string, 0, len(candidates))
			for _, name := range candidates {
				got = append(got, name.(string))
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("candidates = %v, want %v", got, test.want)
			}
			if due, _ := arc["handoff_due"].(bool); due != test.handoff {
				t.Fatalf("handoff_due = %v, want %v", due, test.handoff)
			}
		})
	}
}

// TestStoryPublishPausesAtTheHandoff: the beat that was due hands back to the reader
// and names the phase the next line will open.
func TestStoryPublishPausesAtTheHandoff(t *testing.T) {
	base := storyWith(t, story(t, "我们四个在破庙里避雨。"), map[string]any{
		"handoff_due":  true,
		"phase_spoken": []any{"narrator", "wukong"},
	})
	writes := runRuleIn(t, storyteller, "rules/publish.star", storyPublishConfig,
		map[string]graph.Value{
			"story":    base,
			"narrated": graph.Text("镜头停在紧闭的庙门上。"),
			"spoken":   graph.Text(""),
			"prompt":   graph.Text(""),
		})
	arc := decodeGame(t, writes["story"])
	if arc["awaiting_user"] != true {
		t.Fatalf("awaiting_user = %v, want a pause", arc["awaiting_user"])
	}
	if arc["next_phase"] != "reveal" {
		t.Fatalf("next_phase = %v", arc["next_phase"])
	}
	rows, _ := arc["transcript"].([]any)
	if len(rows) != 1 {
		t.Fatalf("transcript = %v", arc["transcript"])
	}
	prompt, _ := writes["prompt"].Text()
	if !strings.Contains(prompt, "reveal") {
		t.Fatalf("prompt = %q, want the next phase", prompt)
	}
}

// TestStoryEpilogueClosesTheArc: the last phase marks the arc done and says the phrase
// an autoplay stops on.
func TestStoryEpilogueClosesTheArc(t *testing.T) {
	base := storyWith(t, story(t, "我们四个在破庙里避雨。"), map[string]any{
		"phase": "epilogue",
	})
	writes := runRuleIn(t, storyteller, "rules/publish.star", storyPublishConfig,
		map[string]graph.Value{
			"story":    base,
			"narrated": graph.Text("雨停了，四人重新上路。"),
			"spoken":   graph.Text(""),
			"prompt":   graph.Text(""),
		})
	arc := decodeGame(t, writes["story"])
	if arc["completed"] != true {
		t.Fatalf("completed = %v", arc["completed"])
	}
	prompt, _ := writes["prompt"].Text()
	if !strings.Contains(prompt, "故事收束") {
		t.Fatalf("prompt = %q, want the closing phrase", prompt)
	}
}
