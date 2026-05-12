package anthropic

import (
	"strings"
	"testing"

	asdk "github.com/anthropics/anthropic-sdk-go"
)

// long produces a deterministic filler string just over the cache
// threshold. Using a fixed character so each test's "long" segment
// is easy to spot in failure diffs.
func long(marker rune) string {
	return strings.Repeat(string(marker), anthropicCacheMinChars+1)
}

// short produces a sub-threshold filler string. Anchored at half the
// threshold so it's unambiguously below the floor.
func short(marker rune) string {
	return strings.Repeat(string(marker), anthropicCacheMinChars/2)
}

// makeSystem builds a []TextBlockParam from raw segment strings,
// matching what convertMessages produces after the multi-block
// refactor.
func makeSystem(segs ...string) []asdk.TextBlockParam {
	out := make([]asdk.TextBlockParam, len(segs))
	for i, s := range segs {
		out[i] = asdk.TextBlockParam{Text: s}
	}
	return out
}

// makeMessages builds a []MessageParam of alternating user/assistant
// text-content turns from the given strings. Index 0 is user, 1 is
// assistant, 2 is user, etc. — mirroring a real multi-turn shape.
func makeMessages(turns ...string) []asdk.MessageParam {
	out := make([]asdk.MessageParam, 0, len(turns))
	for i, t := range turns {
		if i%2 == 0 {
			out = append(out, asdk.NewUserMessage(asdk.NewTextBlock(t)))
		} else {
			out = append(out, asdk.NewAssistantMessage(asdk.NewTextBlock(t)))
		}
	}
	return out
}

// TestPlanCacheAnchors_SystemOnly covers the basic case the design
// targets: caller splits the system prompt into stable + volatile
// segments, and only the long stable segments become anchors.
func TestPlanCacheAnchors_SystemOnly(t *testing.T) {
	tests := []struct {
		name     string
		segments []string
		wantSys  []int
	}{
		{
			name:     "single long segment anchors at index 0",
			segments: []string{long('A')},
			wantSys:  []int{0},
		},
		{
			name:     "single short segment never anchors",
			segments: []string{short('a')},
			wantSys:  nil,
		},
		{
			name:     "stable+volatile (long, short): only stable anchors",
			segments: []string{long('A'), short('b')},
			wantSys:  []int{0},
		},
		{
			name:     "volatile in the middle: both stable ends anchor",
			segments: []string{long('A'), short('b'), long('C')},
			// last-first: anchors[0]=2 (highest priority), anchors[1]=0
			wantSys: []int{2, 0},
		},
		{
			name:     "five long segments capped at 4 anchors, earliest dropped",
			segments: []string{long('A'), long('B'), long('C'), long('D'), long('E')},
			// last-first order, total 4 (budget cap): 4,3,2,1 — index 0 dropped
			wantSys: []int{4, 3, 2, 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sys := makeSystem(tc.segments...)
			plan := planCacheAnchors(sys, nil, nil)
			if !equalIntSlices(plan.systemBlocks, tc.wantSys) {
				t.Fatalf("systemBlocks = %v, want %v", plan.systemBlocks, tc.wantSys)
			}
			if plan.historyMsgIdx != -1 {
				t.Fatalf("historyMsgIdx = %d, want -1", plan.historyMsgIdx)
			}
			if plan.toolsLast {
				t.Fatalf("toolsLast = true, want false")
			}
		})
	}
}

// TestPlanCacheAnchors_HistoryGate verifies the multi-turn gate:
// short conversations or short histories don't get the history
// anchor (no payoff before the 5-min TTL expires).
func TestPlanCacheAnchors_HistoryGate(t *testing.T) {
	tests := []struct {
		name        string
		messages    []string
		wantHistory int
	}{
		{
			name:        "single-turn (1 msg): no history anchor",
			messages:    []string{long('U')},
			wantHistory: -1,
		},
		{
			name:        "below minimum message count (3 msgs): no anchor",
			messages:    []string{long('U'), long('A'), short('u')},
			wantHistory: -1,
		},
		{
			name: "4 msgs, history below threshold: no anchor",
			// 3 very short history messages, 1 final user — history
			// sum (3 * 100 = 300 chars) is well below
			// anthropicCacheMinChars (4096). Use 100-char fillers
			// here instead of the standard short() helper because
			// three short()s would sum past the threshold.
			messages: []string{
				strings.Repeat("u", 100),
				strings.Repeat("a", 100),
				strings.Repeat("u", 100),
				strings.Repeat("q", 100),
			},
			wantHistory: -1,
		},
		{
			name: "4 msgs, history above threshold: anchor on idx 2",
			// 3 long history messages + 1 final user → anchor at
			// the second-to-last message (idx 2), caching turns 0–2.
			messages:    []string{long('U'), long('A'), long('U'), short('q')},
			wantHistory: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msgs := makeMessages(tc.messages...)
			plan := planCacheAnchors(nil, msgs, nil)
			if plan.historyMsgIdx != tc.wantHistory {
				t.Fatalf("historyMsgIdx = %d, want %d", plan.historyMsgIdx, tc.wantHistory)
			}
		})
	}
}

// TestPlanCacheAnchors_ToolsGate covers the tools-end anchor: tools
// need to collectively exceed the cache size threshold to be worth
// anchoring (otherwise we burn the 25% write surcharge with no payoff).
func TestPlanCacheAnchors_ToolsGate(t *testing.T) {
	tests := []struct {
		name      string
		tools     []asdk.ToolUnionParam
		wantTools bool
	}{
		{name: "no tools", tools: nil, wantTools: false},
		{
			name: "one tiny tool",
			tools: []asdk.ToolUnionParam{
				{OfTool: &asdk.ToolParam{
					Name:        "ping",
					Description: asdk.String("trivial"),
				}},
			},
			wantTools: false,
		},
		{
			name: "one big tool",
			tools: []asdk.ToolUnionParam{
				{OfTool: &asdk.ToolParam{
					Name:        "search",
					Description: asdk.String(long('D')),
				}},
			},
			wantTools: true,
		},
		{
			name: "several small tools summing past threshold",
			tools: func() []asdk.ToolUnionParam {
				out := make([]asdk.ToolUnionParam, 6)
				for i := range out {
					out[i] = asdk.ToolUnionParam{OfTool: &asdk.ToolParam{
						Name:        "t",
						Description: asdk.String(strings.Repeat("x", 800)),
					}}
				}
				return out
			}(),
			wantTools: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := planCacheAnchors(nil, nil, tc.tools)
			if plan.toolsLast != tc.wantTools {
				t.Fatalf("toolsLast = %v, want %v", plan.toolsLast, tc.wantTools)
			}
		})
	}
}

// TestPlanCacheAnchors_BudgetSharing checks that the 4-breakpoint
// global budget is honoured when system, history, and tools all
// have candidates. Priority: tools > history > system-latest >
// earlier-system.
func TestPlanCacheAnchors_BudgetSharing(t *testing.T) {
	sys := makeSystem(long('A'), long('B'), long('C'), long('D'))
	// Build a 4-turn conversation with long history.
	msgs := makeMessages(long('U'), long('A'), long('U'), short('q'))
	tools := []asdk.ToolUnionParam{
		{OfTool: &asdk.ToolParam{
			Name:        "search",
			Description: asdk.String(long('T')),
		}},
	}
	plan := planCacheAnchors(sys, msgs, tools)

	if !plan.toolsLast {
		t.Fatalf("expected tools anchor (highest priority)")
	}
	if plan.historyMsgIdx != 2 {
		t.Fatalf("expected history anchor at idx 2, got %d", plan.historyMsgIdx)
	}
	// 4 long system segments, 2 anchors remaining after tools+history:
	// anchors should be the last two, i.e. [3, 2].
	if !equalIntSlices(plan.systemBlocks, []int{3, 2}) {
		t.Fatalf("systemBlocks = %v, want [3 2]", plan.systemBlocks)
	}
}

// TestApplyAnchorsToSystem confirms the helper stamps cache_control
// on exactly the named indices and leaves the others zero-valued.
func TestApplyAnchorsToSystem(t *testing.T) {
	sys := makeSystem("a", "b", "c")
	applyAnchorsToSystem(sys, []int{0, 2})
	if isZeroCacheControl(sys[0].CacheControl) {
		t.Errorf("expected cache_control on sys[0]")
	}
	if !isZeroCacheControl(sys[1].CacheControl) {
		t.Errorf("expected sys[1] unmarked, got %+v", sys[1].CacheControl)
	}
	if isZeroCacheControl(sys[2].CacheControl) {
		t.Errorf("expected cache_control on sys[2]")
	}
	// Out-of-range index should be a no-op rather than a panic.
	applyAnchorsToSystem(sys, []int{99, -1})
}

// TestApplyAnchorToHistory stamps cache_control on the final content
// block of the targeted message.
func TestApplyAnchorToHistory(t *testing.T) {
	msgs := makeMessages("u1", "a1", "u2", "q")
	applyAnchorToHistory(msgs, 2)
	content := msgs[2].Content
	if len(content) == 0 {
		t.Fatal("msg[2] has no content")
	}
	last := content[len(content)-1]
	if cc := last.GetCacheControl(); cc == nil || isZeroCacheControl(*cc) {
		t.Fatalf("expected cache_control on last block of msg[2], got %+v", cc)
	}
	// Other messages must remain unmarked.
	if cc := msgs[0].Content[0].GetCacheControl(); cc != nil && !isZeroCacheControl(*cc) {
		t.Errorf("expected msg[0] unmarked, got %+v", *cc)
	}
}

// TestApplyAnchorToTools stamps cache_control on the final tool only.
func TestApplyAnchorToTools(t *testing.T) {
	tools := []asdk.ToolUnionParam{
		{OfTool: &asdk.ToolParam{Name: "first"}},
		{OfTool: &asdk.ToolParam{Name: "second"}},
	}
	applyAnchorToTools(tools)
	if !isZeroCacheControl(tools[0].OfTool.CacheControl) {
		t.Errorf("expected first tool unmarked")
	}
	if isZeroCacheControl(tools[1].OfTool.CacheControl) {
		t.Errorf("expected cache_control on last tool")
	}
}

func equalIntSlices(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isZeroCacheControl(cc asdk.CacheControlEphemeralParam) bool {
	// The SDK's Type field is a constant.CacheControlEphemeral that's
	// empty ("") when zero-initialised; NewCacheControlEphemeralParam
	// populates it with the literal "ephemeral". Comparing the Type
	// against its zero value distinguishes "marked" from "unmarked"
	// without needing the SDK-internal IsKnown helper (not exported
	// on CacheControlEphemeralTTL).
	return cc.Type == ""
}
