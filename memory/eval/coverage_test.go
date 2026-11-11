package eval

import (
	"context"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

const coverageTurnText = "Melanie: I ran a charity race for mental health."

func coverageItem(id string, kind corememory.ContextItemKind, text string, sources ...corememory.SourceRef) corememory.ContextItem {
	return corememory.ContextItem{
		ID: id, Kind: kind, SourceClass: corememory.ContextSourceLongTerm,
		Content: coremessage.NewTextContent(text), Sources: sources,
	}
}

func coverageSource(id, turnID, text string) corememory.SourceRef {
	return corememory.SourceRef{Kind: corememory.SourceMessage, ID: id}
}

// stubSources resolves by source message id, so a test can give the same
// canonical message different texts or turn ids depending on what it pins.
type stubSources map[string][]ResolvedSource

func (resolver stubSources) ResolveSources(_ context.Context, item corememory.ContextItem) []ResolvedSource {
	return resolver[item.ID]
}

// The wording the dataset graded is what a report classifies as a raw hit, so a
// pack that holds both the turn's message and a fact derived from it reports the
// message, whatever the pack order is.
func TestMatcherPrefersTheTurnTextOverProvenanceIdentity(t *testing.T) {
	fact := coverageItem("fact-1", corememory.ContextFact, "Melanie finds running rewarding.",
		coverageSource("conv-1/msg-1", "", ""))
	raw := coverageItem("msg-1", corememory.ContextRawMessage, coverageTurnText,
		coverageSource("conv-1/msg-1", "", ""))
	resolver := stubSources{
		"fact-1": {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: "Melanie: I ran a race."}},
		"msg-1":  {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: coverageTurnText}},
	}
	coverage := NewMatcher(resolver).Cover(context.Background(),
		[]corememory.ContextItem{fact, raw}, "D1:1", coverageTurnText)
	if !coverage.Covered() || coverage.Rank != 1 || !coverage.ViaRaw {
		t.Fatalf("coverage = %#v, want the raw item at rank 1", coverage)
	}
}

// The shadowing the pack order used to cause: a derived item standing on the
// turn outranks the turn's own message lower in the pack, and the label then
// said the pack held a paraphrase while the prompt held the graded wording.
func TestMatcherPrefersTheTurnMessageOverAnEarlierProvenanceHit(t *testing.T) {
	source := coverageSource("conv-1/msg-1", "", "")
	for _, test := range []struct {
		name string
		// The derived item reaches the turn the one way its store allows: by
		// text, when nothing tagged the message, or by the turn id.
		fact ResolvedSource
		turn string
	}{
		{
			name: "the derived item stands on the turn by text",
			fact: ResolvedSource{ConversationID: "conv-1", MessageID: "msg-1", Text: coverageTurnText},
		},
		{
			name: "the derived item stands on the turn by id",
			fact: ResolvedSource{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: "Melanie: I ran a race."},
			turn: "D1:1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fact := coverageItem("fact-1", corememory.ContextFact, "Melanie finds running rewarding.", source)
			raw := coverageItem("msg-1", corememory.ContextRawMessage, coverageTurnText, source)
			resolver := stubSources{
				"fact-1": {test.fact},
				"msg-1":  {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: coverageTurnText}},
			}
			coverage := NewMatcher(resolver).Cover(context.Background(),
				[]corememory.ContextItem{fact, raw}, test.turn, coverageTurnText)
			if !coverage.Covered() || coverage.Rank != 1 || !coverage.ViaRaw {
				t.Fatalf("coverage = %#v, want the turn's own message at rank 1", coverage)
			}
		})
	}
}

// The same shadowing where no text can decide it: a native store and a loader
// rendering disagree about the caption, so neither side contains the other. The
// fact reaches the turn by id at rank 0 and the turn's own message sits at rank
// 1 -- still the message the dataset graded, and still what the model read.
func TestMatcherPrefersTheTurnMessageOverAnEarlierIdentityHit(t *testing.T) {
	const (
		rendered = coverageTurnText + " [shared image: finish line]"
		stored   = coverageTurnText
	)
	source := coverageSource("conv-1/msg-1", "", "")
	fact := coverageItem("fact-1", corememory.ContextFact, "Melanie finds running rewarding.", source)
	raw := coverageItem("msg-1", corememory.ContextRawMessage, stored, source)
	resolver := stubSources{
		"fact-1": {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: "Melanie: I ran a race."}},
		"msg-1":  {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: stored}},
	}
	coverage := NewMatcher(resolver).Cover(context.Background(),
		[]corememory.ContextItem{fact, raw}, "D1:1", rendered)
	if !coverage.Covered() || coverage.Rank != 1 || !coverage.ViaRaw {
		t.Fatalf("coverage = %#v, want the turn's own message at rank 1", coverage)
	}
}

// A derived item carrying the wording carries it wherever the pack put the
// message: retrieval.source_quotes folds the source turn into the fact, so the
// prompt shows the graded wording through that item.
func TestMatcherCountsAFoldedSourceQuoteAsTheTurnWording(t *testing.T) {
	source := coverageSource("conv-1/msg-1", "", "")
	fact := coverageItem("fact-1", corememory.ContextFact,
		"Melanie finds running rewarding. Source turn: "+coverageTurnText, source)
	raw := coverageItem("msg-1", corememory.ContextRawMessage, coverageTurnText, source)
	resolver := stubSources{
		"fact-1": {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: coverageTurnText}},
		"msg-1":  {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: coverageTurnText}},
	}
	coverage := NewMatcher(resolver).Cover(context.Background(),
		[]corememory.ContextItem{fact, raw}, "D1:1", coverageTurnText)
	if !coverage.Covered() || coverage.Rank != 0 || !coverage.ViaRaw {
		t.Fatalf("coverage = %#v, want the folded quote at rank 0", coverage)
	}
}

// An item's source is a canonical message, and a store written without dataset
// turn ids leaves only the committed text to compare: the text of the source is
// enough to attribute the turn, but it is not the graded wording.
func TestMatcherCoversBySourceTextWithoutTurnIDs(t *testing.T) {
	fact := coverageItem("fact-1", corememory.ContextFact, "Melanie finds running rewarding.",
		coverageSource("conv-1/msg-1", "", ""))
	resolver := stubSources{
		"fact-1": {{ConversationID: "conv-1", MessageID: "msg-1", Text: coverageTurnText}},
	}
	coverage := NewMatcher(resolver).Cover(context.Background(),
		[]corememory.ContextItem{fact}, "", coverageTurnText)
	if !coverage.Covered() || coverage.Rank != 0 || coverage.ViaRaw {
		t.Fatalf("coverage = %#v, want a provenance hit at rank 0", coverage)
	}
}

// The case text alone cannot see: the loader renders an image turn with a
// caption the store does not carry ("-images native" attaches the image as a
// part instead), or the store carries a caption the loader's mode does not
// render. Neither side contains the other, and in both directions the turn id
// ingest tagged the message with still covers it -- as a raw hit, because the
// item *is* the turn's message.
func TestMatcherCoversByTurnIdentityWhenTheTextDiffers(t *testing.T) {
	const stored = "Melanie: I ran a charity race."
	rendered := stored + " [shared image: finish line]"
	for _, test := range []struct {
		name  string
		item  string
		turn  string
		store string
		// bare is whether the untagged store leaves nothing to match by text:
		// in the other direction the item content still contains the turn text.
		bare bool
	}{
		{name: "the loader renders a caption the store does not carry", item: stored, turn: rendered, store: stored, bare: true},
		{name: "the store carries a caption the loader does not render", item: rendered, turn: stored, store: rendered},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := coverageItem("msg-1", corememory.ContextRawMessage, test.item,
				coverageSource("conv-1/msg-1", "", ""))
			resolver := stubSources{
				"msg-1": {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: test.store}},
			}
			coverage := NewMatcher(resolver).Cover(context.Background(),
				[]corememory.ContextItem{raw}, "D1:1", test.turn)
			if !coverage.Covered() || coverage.Rank != 0 || !coverage.ViaRaw {
				t.Fatalf("coverage = %#v, want a raw identity hit at rank 0", coverage)
			}
			if test.bare {
				// The same item without the id on its source is the failure this
				// covers for: nothing but text to compare, and no text to match.
				untagged := stubSources{
					"msg-1": {{ConversationID: "conv-1", MessageID: "msg-1", Text: test.store}},
				}
				if stale := NewMatcher(untagged).Cover(context.Background(),
					[]corememory.ContextItem{raw}, "D1:1", test.turn); stale.Covered() {
					t.Fatalf("coverage without a turn id = %#v, want none", stale)
				}
			}
		})
	}
}

// A derived item covers a turn through its sources, and a summary reads as a
// paraphrase however its source was tagged.
func TestMatcherCoversDerivedItemsThroughIdentity(t *testing.T) {
	fact := coverageItem("fact-1", corememory.ContextFact, "Melanie finds running rewarding.",
		coverageSource("conv-1/msg-1", "", ""))
	resolver := stubSources{
		"fact-1": {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: "Melanie: I ran a race."}},
	}
	coverage := NewMatcher(resolver).Cover(context.Background(),
		[]corememory.ContextItem{fact}, "D1:1", coverageTurnText)
	if !coverage.Covered() || coverage.Rank != 0 || coverage.ViaRaw {
		t.Fatalf("coverage = %#v, want a provenance hit at rank 0", coverage)
	}
	// A different turn of the same commit is not this turn: identity is exact.
	if other := NewMatcher(resolver).Cover(context.Background(),
		[]corememory.ContextItem{fact}, "D1:9", coverageTurnText); other.Covered() {
		t.Fatalf("coverage = %#v, want the turn untouched", other)
	}
}

func TestMatcherReportsUncoveredTurns(t *testing.T) {
	items := []corememory.ContextItem{coverageItem("fact-1", corememory.ContextFact, "something else",
		coverageSource("conv-1/msg-1", "", ""))}
	resolver := stubSources{"fact-1": {{ConversationID: "conv-1", MessageID: "msg-1"}}}
	coverage := NewMatcher(resolver).Cover(context.Background(), items, "D1:1", coverageTurnText)
	if coverage.Covered() || coverage.Rank != -1 {
		t.Fatalf("coverage = %#v, want no coverage", coverage)
	}
	// Without a resolver the contents are all there is, and the harness must not
	// panic on the nil case a host that wires no provenance leaves behind.
	bare := NewMatcher(nil).Cover(context.Background(), items, "D1:1", coverageTurnText)
	if bare.Covered() {
		t.Fatalf("coverage without a resolver = %#v, want none", bare)
	}
	if !NewMatcher(nil).Cover(context.Background(), items, "D1:1", "something").Covered() {
		t.Fatal("content matching must survive a nil resolver")
	}
}
