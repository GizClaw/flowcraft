// Package maintain implements host-invoked memory maintenance: it detects
// superseded facts (soft merge) and stale facts (decay) and records a
// read-path overlay, keeping the canonical fact text immutable and ADD-only.
package maintain

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/internal/textsim"
	"github.com/GizClaw/flowcraft/backends/memory/recall"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// AlgorithmVersion names the maintenance policy. v2 measures fact similarity
// over the shared kernel's tokens, so facts without whitespace word boundaries
// (CJK text) compare by character and bigram overlap instead of reading as
// entirely dissimilar. v3 reads the read path's receipts: a fact the caller
// keeps being served decays from its most recent recall rather than from its
// event time, and each recall forgives a saturating share of the decay that
// remains. Reinforcement only ever compensates decay -- a factor never rises
// above the un-decayed 1 -- so an overlay stays a penalty, never a boost.
//
// v4 reads the predicate the extractor wrote: two facts stating the same
// single-valued relation about the same subject cannot both be current, so the
// older one is superseded even when the texts share almost no tokens -- an
// update whose wording moved with it, which similarity alone cannot see. Every
// supersede finding also carries the window the older fact was current over
// (valid until the superseding fact's own date), which is what a "was it true
// then" read needs and what the similarity rule never recorded.
//
// v5 is v4 plus the arbiter: the pairs the vocabulary cannot read and the
// texts cannot decide are put to a model, and its verdict is a third reason a
// finding can be made for. The version stays v4-shaped because the addition is
// opt-in -- a deployment without an arbiter judges exactly what v4 judged --
// and the arbiter's own identity is in ProducerVersion, so an arbiter's
// findings are not served where a different arbiter, or none, runs.
const AlgorithmVersion = "maintain-v4"

// DefaultSimilarityThreshold is the token Jaccard similarity two facts about
// the same entity need before the older one counts as superseded. It is
// exported because a reading has to measure the threshold the pass ships
// rather than a copy of it: a tool that hardcodes 0.5 measures itself the day
// the default moves.
const DefaultSimilarityThreshold = 0.5

// Config tunes one maintenance pass.
type Config struct {
	// SimilarityThreshold is the token Jaccard similarity two facts about the
	// same entity need before the older one counts as superseded (default 0.5).
	SimilarityThreshold float64
	// SupersedeFactor multiplies the retrieval score of a superseded fact
	// (default 0.5).
	SupersedeFactor float64
	// DecayHalfLife halves a fact's retrieval score every half-life
	// (default 720h).
	DecayHalfLife time.Duration
	// DecayFloor is the score below which an aged fact starts being decayed;
	// a fresh fact scores 1 (default 0.5).
	DecayFloor float64
	// RecallSaturation is the receipt count at which half of the decay a
	// recalled fact still carries is forgiven (default 8). Each receipt
	// forgives count/(count+RecallSaturation) of the gap between the decayed
	// score and 1, so the compensated factor stays in [decay, 1]: more
	// recalls are worth more, and never more than the un-decayed score.
	RecallSaturation float64
	// Relations is the vocabulary of single-valued relations the pass
	// recognizes by predicate. Nil is DefaultRelations; an empty (non-nil)
	// map turns the rule off. The vocabulary is part of what the pass is, so
	// it is digested into ProducerVersion: an overlay judged under one
	// vocabulary is not served under another.
	Relations Relations
	// Arbiter, when set, decides the candidate pairs the two rules cannot:
	// the pair whose predicates the vocabulary does not recognize, and whose
	// texts share too few tokens for the similarity rule. Nil is the
	// default, and leaves the pass with its own rules -- the shape that
	// needs no model.
	//
	// Its identity is digested into ProducerVersion, the same way the
	// vocabulary's is: a judgment one arbiter made is not served where
	// another runs.
	Arbiter Arbiter
	// ArbiterBudget caps how many pairs one pass may put to the arbiter
	// (default 1000). Every pair is one question in a model call, and a
	// scope of ten thousand facts has more candidate pairs than one pass
	// should pay for. It bounds the pairs the scope has no answer for yet:
	// a pair an earlier pass already asked is answered from the scope's
	// remembered verdicts and costs nothing, so the budget decides how much
	// new surface one pass covers rather than how many pairs it looks at.
	// The pairs it leaves out are counted in Plan.ArbiterSkipped and asked by
	// the next pass. It bounds cost rather than judgment, so it stays out of
	// ProducerVersion.
	ArbiterBudget int
}

func (config Config) withDefaults() Config {
	if config.SimilarityThreshold == 0 {
		config.SimilarityThreshold = DefaultSimilarityThreshold
	}
	if config.SupersedeFactor == 0 {
		config.SupersedeFactor = 0.5
	}
	if config.DecayHalfLife == 0 {
		config.DecayHalfLife = 720 * time.Hour
	}
	if config.DecayFloor == 0 {
		config.DecayFloor = 0.5
	}
	if config.RecallSaturation == 0 {
		config.RecallSaturation = 8
	}
	if config.Relations == nil {
		config.Relations = DefaultRelations()
	}
	if config.ArbiterBudget == 0 {
		config.ArbiterBudget = 1000
	}
	return config
}

func (config Config) validate() error {
	if !finite(config.SimilarityThreshold) || config.SimilarityThreshold < 0 || config.SimilarityThreshold > 1 {
		return errors.New("maintain: similarity threshold must be in [0,1]")
	}
	if !finite(config.SupersedeFactor) || config.SupersedeFactor <= 0 || config.SupersedeFactor >= 1 {
		return errors.New("maintain: supersede factor must be in (0,1)")
	}
	if config.DecayHalfLife <= 0 {
		return errors.New("maintain: decay half-life must be positive")
	}
	if !finite(config.DecayFloor) || config.DecayFloor <= 0 || config.DecayFloor > 1 {
		return errors.New("maintain: decay floor must be in (0,1]")
	}
	if !finite(config.RecallSaturation) || config.RecallSaturation <= 0 {
		return errors.New("maintain: recall saturation must be positive")
	}
	if _, problems := canonicalVocabulary(config.Relations); len(problems) > 0 {
		return fmt.Errorf("maintain: relation vocabulary: %s", strings.Join(problems, "; "))
	}
	if config.ArbiterBudget < 0 {
		return errors.New("maintain: arbiter budget must not be negative")
	}
	return nil
}

// ProducerVersion names the exact pass whose findings an overlay holds: the
// policy version plus the tuning those findings were measured with. Two
// deployments that disagree about the tuning judge the same facts differently,
// so an overlay records the pass that wrote it and is served only where the
// same pass runs.
func (config Config) ProducerVersion() string {
	config = config.withDefaults()
	stamp := fmt.Sprintf("%s+similarity=%g,supersede=%g,half_life=%s,floor=%g,recall_saturation=%g,relations=%s",
		AlgorithmVersion, config.SimilarityThreshold, config.SupersedeFactor,
		config.DecayHalfLife, config.DecayFloor, config.RecallSaturation,
		newRelationIndex(config.Relations).digest)
	if arbiterSet(config.Arbiter) {
		// Absent rather than "none" when no arbiter is configured: a
		// deployment that adds one invalidates the overlays it judged with
		// its own rules, and a deployment that has none keeps serving the
		// overlays it already wrote.
		stamp += ",arbiter=" + config.Arbiter.Identity()
	}
	return stamp
}

// arbiterSet reports whether a config carries an arbiter to ask. A nil
// interface is the obvious no; a typed nil inside it is a wiring bug, and one
// the pass must not take for a judgment -- asking it would panic on the first
// pair, and stamping it would name a judgment that is not there.
func arbiterSet(arbiter Arbiter) bool {
	if arbiter == nil {
		return false
	}
	value := reflect.ValueOf(arbiter)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	}
	return true
}

// Reasons a supersede finding was made for. A finding is made for exactly one
// of them, and the reason names the evidence that decided it.
const (
	// ReasonRelation: the two facts state the same single-valued relation
	// about the same subject, so the older one is not the current value of
	// it. The finding's Relation names the relation.
	ReasonRelation = "relation"
	// ReasonSimilarity: the two texts are similar enough that the older fact
	// reads as a restatement of the newer one.
	ReasonSimilarity = "similarity"
	// ReasonArbiter: the pair was put to the arbiter, and it judged the
	// older fact no longer current. The finding records the probability it
	// was judged with, because this reason is the only one whose evidence is
	// not a rule a reader can re-apply to the two facts.
	ReasonArbiter = "arbiter"
)

// Supersede marks one older fact as superseded by a newer one: the older fact
// is not removed and not edited, its retrieval score is reduced, and the
// window it was current over is recorded beside it.
type Supersede struct {
	FactID       string  `json:"fact_id"`
	Conversation string  `json:"conversation_id"`
	SupersededBy string  `json:"superseded_by"`
	Similarity   float64 `json:"similarity"`
	// Reason is ReasonRelation or ReasonSimilarity (see above), and Relation
	// names the single-valued relation the pair states -- empty for a
	// similarity finding, because sharing a relation is not what it read.
	Reason   string `json:"reason"`
	Relation string `json:"relation,omitempty"`
	// Confidence is the arbiter's probability that the older fact is no
	// longer current; it is zero for a finding the pass's own rules made,
	// which are decided rather than estimated.
	Confidence float64 `json:"confidence,omitempty"`
	// ValidAt and InvalidAt bracket the window the superseded fact was current
	// over: it was dated from ValidAt (its own event time, or when the fact
	// was created if the turn stated no time) and replaced at InvalidAt (the
	// superseding fact's own date, which is the same key the pass ordered the
	// pair by, so the window never runs backwards). A read that has to answer
	// "was this true then" compares its time against the two.
	ValidAt   time.Time `json:"valid_at"`
	InvalidAt time.Time `json:"invalid_at"`
	// ValidDetail and InvalidDetail are the two facts' own time expressions,
	// as the extractor stated them ("since 2019", "on 5 March 2023"). The
	// absolute times above are this pass's resolution of those expressions;
	// these are what a human checks the resolution against, and either is
	// empty when that turn gave no expression at all.
	ValidDetail   string `json:"valid_detail,omitempty"`
	InvalidDetail string `json:"invalid_detail,omitempty"`
}

// Decay marks one aged fact with its retention score.
type Decay struct {
	FactID       string    `json:"fact_id"`
	Conversation string    `json:"conversation_id"`
	Score        float64   `json:"score"`
	EventTime    time.Time `json:"event_time"`
	// Recalls and LastRecall name the receipt this factor was measured with:
	// how many context results had returned the fact when the pass ran, and
	// when the most recent one did. They carry the count even when the
	// compensation did not change an entry, so a plan explains its own
	// factors without the receipt store.
	Recalls    int       `json:"recalls,omitempty"`
	LastRecall time.Time `json:"last_recall,omitempty"`
}

// Plan is the output of one maintenance pass, and it is pure data: applying what
// it detects writes a read-path overlay and never edits canonical facts.
// Supersedes and Decays are what the pass detected and are deterministic in the
// scope's facts; ReclaimedChunks describes what the same pass removed from the
// derived views, which is a property of the stored state rather than of the
// facts, so it is reported beside the detection rather than detected with it.
type Plan struct {
	Scope      corememory.Scope `json:"scope"`
	Supersedes []Supersede      `json:"supersedes,omitempty"`
	Decays     []Decay          `json:"decays,omitempty"`
	// ReclaimedChunks counts the superseded document chunk records this pass
	// removed: an immutable build leaves one record per chunk when a document
	// is published again, and reads reach only the active build. See
	// document.DocumentViewStore.RetireScopeBuilds for what is kept.
	ReclaimedChunks int `json:"reclaimed_chunks,omitempty"`
	// ReclaimedProjectionObjects counts the immutable projection objects this
	// pass removed: the bases and segments the lanes compacted or rebuilt away
	// and their active manifests stopped pointing at. Reads reach only what a
	// manifest names, so what it does not name is storage nothing serves -- see
	// component.Reclaimer for what is kept.
	ReclaimedProjectionObjects int    `json:"reclaimed_projection_objects,omitempty"`
	AlgorithmVersion           string `json:"algorithm_version"`
	// RecalledFacts counts the scope's facts that carried a receipt: how many
	// of them the read path has actually returned. It is the wiring signal --
	// a deployment whose receipts never arrive reports zero here however long
	// it runs -- and it stays out of the overlay.
	RecalledFacts int `json:"recalled_facts,omitempty"`
	// ArbiterAsked counts the candidate pairs this pass put to the arbiter,
	// and ArbiterSkipped the ones the budget left for a later pass. Together
	// they say how much of the undecidable surface one pass covered: a scope
	// that keeps skipping pairs is a budget the deployment should raise, and
	// a scope that asks and never finds is a rule that is paying for nothing.
	//
	// Asked counts calls, not pairs examined: a pair an earlier pass already
	// put to this arbiter is answered from the scope's remembered verdicts
	// and counted in ArbiterRemembered instead.
	ArbiterAsked   int `json:"arbiter_asked,omitempty"`
	ArbiterSkipped int `json:"arbiter_skipped,omitempty"`
	// ArbiterRemembered counts the pairs this pass answered from an earlier
	// pass's verdicts rather than from a call. It is the number a deployment
	// reads to tell a converging pass (remembered grows, asked shrinks) from
	// one that keeps paying for the same questions.
	ArbiterRemembered int `json:"arbiter_remembered,omitempty"`
	// ArbiterReplaced, ArbiterBothCurrent and ArbiterUndecided split the
	// pairs this pass was answered about: the ones that became a finding, the
	// ones the arbiter found can hold at once, and the ones it declined or
	// answered below its threshold. The three are disjoint and sum to the
	// asked count, so a pass that asks and never replaces says so with the
	// other two instead of hiding behind one number.
	ArbiterReplaced    int `json:"arbiter_replaced,omitempty"`
	ArbiterBothCurrent int `json:"arbiter_both_current,omitempty"`
	ArbiterUndecided   int `json:"arbiter_undecided,omitempty"`
	// ProducerVersion is the stamp the pass writes with its overlay: the
	// policy version plus the tuning (see Config.ProducerVersion). A host
	// reads an overlay only when the stamp is its own, so a policy or tuning
	// change leaves the earlier judgments behind instead of applying them
	// under the new rules.
	ProducerVersion string `json:"producer_version"`
	// IgnoredOverlay reports that this pass replaced an overlay its own policy
	// could not read: judgments an earlier producer version or tuning wrote,
	// which the read path was not applying. It is the flag a host logs, and
	// the pass that sets it clears it.
	IgnoredOverlay bool `json:"ignored_overlay,omitempty"`
}

// Detect plans one scope's soft merges and decay from its conversations.
// conversations maps a conversation ID to its facts.
func Detect(
	scope corememory.Scope,
	conversations map[string][]factview.Fact,
	now time.Time,
) (Plan, error) {
	return DetectWithConfig(scope, conversations, now, Config{})
}

// DetectWithConfig is Detect with explicit tuning.
func DetectWithConfig(
	scope corememory.Scope,
	conversations map[string][]factview.Fact,
	now time.Time,
	config Config,
) (Plan, error) {
	return DetectWithRecalls(scope, conversations, now, config, nil)
}

// DetectWithRecalls is DetectWithConfig with the read path's receipts:
// hard-scope-qualified item identity to how often and how recently a context
// result actually returned it. Nil is the honest input for a scope the read
// path has never served -- and for a caller without a receipt source: the
// pass then plans from event times alone, which is exactly the v2 judgment
// under the v3 stamp.
func DetectWithRecalls(
	scope corememory.Scope,
	conversations map[string][]factview.Fact,
	now time.Time,
	config Config,
	receipts map[string]recall.Receipt,
) (Plan, error) {
	plan, _, err := detect(scope, conversations, now, config, receipts)
	return plan, err
}

// detect is DetectWithRecalls with the pairs the rules could not decide: the
// arbiter's input, in the order the scan proposed them. A caller that has no
// arbiter drops them, which is what DetectWithRecalls does -- the pairs are
// only built when a pass means to ask.
func detect(
	scope corememory.Scope,
	conversations map[string][]factview.Fact,
	now time.Time,
	config Config,
	receipts map[string]recall.Receipt,
) (Plan, []Pair, error) {
	if err := scope.Validate(); err != nil {
		return Plan{}, nil, err
	}
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return Plan{}, nil, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	plan := Plan{
		Scope: scope, AlgorithmVersion: AlgorithmVersion,
		ProducerVersion: config.ProducerVersion(),
	}
	ids := make([]string, 0, len(conversations))
	for conversation := range conversations {
		ids = append(ids, conversation)
	}
	sort.Strings(ids)
	relations := newRelationIndex(config.Relations)
	var pairs []Pair
	for _, conversation := range ids {
		facts := append([]factview.Fact(nil), conversations[conversation]...)
		sort.SliceStable(facts, func(i, j int) bool {
			left, right := recencyKey(facts[i]), recencyKey(facts[j])
			if left.Equal(right) {
				return facts[i].ID < facts[j].ID
			}
			return left.Before(right)
		})
		entities := newEntityIndex(facts)
		for position, older := range facts {
			identity, keyErr := itemKey(scope, conversation, older.ID)
			if keyErr != nil {
				return Plan{}, nil, keyErr
			}
			receipt, recalled := receipts[identity]
			if recalled {
				plan.RecalledFacts++
			}
			if decay := decayScore(older, now, config, receipt); decay < config.DecayFloor {
				plan.Decays = append(plan.Decays, Decay{
					FactID: older.ID, Conversation: conversation, Score: decay, EventTime: recencyKey(older),
					Recalls: receipt.Count, LastRecall: receipt.Last,
				})
			}
			if len(older.Entities) == 0 {
				continue
			}
			// The relation the older fact states, and who it is about: only a
			// fact that states a recognized relation about a subject the text
			// names can be replaced by a newer value of the same relation.
			olderClass := entities.classOf(relations, position)
			// The subject is read whether or not the vocabulary knows the
			// predicate: it is who the fact is about, which is what the
			// arbiter's candidate pairs are drawn by, and it is not the same
			// question as whether the fact states a relation the pass has a
			// name for.
			olderSubject := entities.subject(position)
			decided := false
			// Only a fact sharing an entity can supersede this one, and the
			// pair-wise scan spent its time deciding that of every later
			// fact. The index answers that question with the buckets it was
			// built from: the candidates are exactly the later facts the
			// scan would have tested, in the order it tested them.
			for _, candidate := range entities.candidates(position) {
				newer := facts[candidate]
				similarity := textsim.Similarity(entities.tokens(position), entities.tokens(candidate))
				reason, relation := "", ""
				if olderClass.known && olderSubject != "" {
					if class := entities.classOf(relations, candidate); class.known && class == olderClass &&
						entities.subject(candidate) == olderSubject &&
						entities.statesADifferentValue(position, candidate, olderSubject) {
						reason, relation = ReasonRelation, class.name
					}
				}
				if reason == "" && similarity < config.SimilarityThreshold {
					continue
				}
				if reason == "" {
					reason = ReasonSimilarity
				}
				plan.Supersedes = append(plan.Supersedes, Supersede{
					FactID: older.ID, Conversation: conversation,
					SupersededBy: newer.ID, Similarity: similarity,
					Reason: reason, Relation: relation,
					ValidAt: recencyKey(older), InvalidAt: recencyKey(newer),
					ValidDetail: older.TemporalDetail, InvalidDetail: newer.TemporalDetail,
				})
				decided = true
				break
			}
			if decided || !arbiterSet(config.Arbiter) || olderSubject == "" {
				continue
			}
			// The rules left this fact current. What they could not decide --
			// and what a model can -- are the pairs that share its subject
			// and state something the vocabulary has no name for. A pair
			// whose two facts are about different subjects is not one
			// relation stated twice, so it is not proposed: the subject is
			// the one thing the pass can read without a model.
			for _, candidate := range entities.candidates(position) {
				if entities.subject(candidate) != olderSubject {
					continue
				}
				pairs = append(pairs, Pair{
					Conversation: conversation, Older: older, Newer: facts[candidate],
				})
			}
		}
	}
	// Detection proposes and does not budget: the pairs come back in the
	// scan's own order, and adjudicate spends the pass's budget on the ones
	// the scope has no answer for. That split is what lets a remembered
	// verdict cost nothing against the budget -- a pair the scan proposes
	// again is not a pair the pass has to pay for.
	sort.Slice(plan.Supersedes, func(i, j int) bool { return plan.Supersedes[i].FactID < plan.Supersedes[j].FactID })
	sort.Slice(plan.Decays, func(i, j int) bool { return plan.Decays[i].FactID < plan.Decays[j].FactID })
	return plan, pairs, nil
}

// adjudicate puts the pairs a pass proposed to its arbiter and adds what it
// found to the plan, which is the third rule applied where the first two left
// off. It is the caller's step rather than the detection's because it needs a
// context and a network round trip, and detection is what has neither.
//
// It also spends the pass's budget, and spends it on the pairs the scope has
// no answer for: verdicts an earlier pass remembered are applied for free, and
// only what remains is a question the arbiter has to be asked. The scan's own
// order decides what the budget covers, so two passes over the same facts ask
// the same questions and a scope converges instead of starving whichever
// pairs happen to sort last.
//
// A failed call changes nothing: the plan keeps the rules' findings, and the
// caller decides whether publishing them without the arbiter's half is
// acceptable -- for a pass it is, because the read path applies what was
// judged and the arbiter's silence is reported rather than served.
func adjudicate(
	ctx context.Context,
	plan *Plan,
	pairs []Pair,
	arbiter Arbiter,
	verdicts *Verdicts,
	budget int,
) error {
	if len(pairs) == 0 || !arbiterSet(arbiter) {
		return nil
	}
	judgments := make([]Judgment, len(pairs))
	considered := make([]int, 0, len(pairs))
	asked := make([]int, 0, len(pairs))
	for index, pair := range pairs {
		if judgment, remembered := verdicts.Lookup(pair); remembered {
			judgments[index] = judgment
			considered = append(considered, index)
			plan.ArbiterRemembered++
			continue
		}
		asked = append(asked, index)
	}
	if budget > 0 && len(asked) > budget {
		plan.ArbiterSkipped = len(asked) - budget
		asked = asked[:budget]
	}
	plan.ArbiterAsked = len(asked)
	if len(asked) > 0 {
		subset := make([]Pair, len(asked))
		for position, index := range asked {
			subset[position] = pairs[index]
		}
		answers, err := arbiter.Adjudicate(ctx, subset)
		if err != nil {
			return err
		}
		for position, index := range asked {
			judgments[index] = answers[position]
			verdicts.Remember(pairs[index], answers[position])
		}
		considered = append(considered, asked...)
	}
	// The scan's order decides which later fact is recorded as the one that
	// made an older fact stale, so the remembered and the asked pairs are
	// judged in one ordered walk rather than two.
	sort.Ints(considered)
	counted := make([]bool, len(pairs))
	for _, index := range asked {
		counted[index] = true
	}
	plan.ArbiterReplaced, plan.ArbiterBothCurrent, plan.ArbiterUndecided = applyVerdicts(plan, pairs, judgments, considered, counted)
	return nil
}

// applyVerdicts adds the arbiter's findings to a plan: one finding per older
// fact whose verdict is replaced, and nothing for the rest. The first verdict
// that replaces a fact is the one recorded -- pairs are proposed in the scan's
// order, so that is the earliest later fact that made it stale, which is the
// same choice the rules make when they stop at the first deciding candidate.
//
// The three counts it returns cover the pairs this pass paid for -- the ones
// marked in counted -- and not the ones it remembered: a verdict from an
// earlier pass was counted there. Findings, both-current answers and undecided
// ones are disjoint, so the three numbers add up to what the pass paid for.
func applyVerdicts(
	plan *Plan,
	pairs []Pair,
	judgments []Judgment,
	considered []int,
	counted []bool,
) (replaced, bothCurrent, undecided int) {
	recorded := make(map[string]struct{}, len(pairs))
	for _, finding := range plan.Supersedes {
		recorded[finding.FactID] = struct{}{}
	}
	for _, index := range considered {
		pair := pairs[index]
		countable := index < len(counted) && counted[index]
		if index >= len(judgments) {
			if countable {
				undecided++
			}
			continue
		}
		judgment := judgments[index]
		switch judgment.Verdict {
		case VerdictBothCurrent:
			if countable {
				bothCurrent++
			}
			continue
		case VerdictReplaced:
		default:
			if countable {
				undecided++
			}
			continue
		}
		if _, exists := recorded[pair.Older.ID]; exists {
			continue
		}
		recorded[pair.Older.ID] = struct{}{}
		if countable {
			replaced++
		}
		plan.Supersedes = append(plan.Supersedes, Supersede{
			FactID: pair.Older.ID, Conversation: pair.Conversation,
			SupersededBy: pair.Newer.ID,
			Similarity:   textsim.Similarity(textsim.Set(pair.Older.Text), textsim.Set(pair.Newer.Text)),
			Reason:       ReasonArbiter, Confidence: judgment.Confidence,
			ValidAt: recencyKey(pair.Older), InvalidAt: recencyKey(pair.Newer),
			ValidDetail: pair.Older.TemporalDetail, InvalidDetail: pair.Newer.TemporalDetail,
		})
	}
	if len(plan.Supersedes) > 0 {
		sort.Slice(plan.Supersedes, func(i, j int) bool { return plan.Supersedes[i].FactID < plan.Supersedes[j].FactID })
	}
	return replaced, bothCurrent, undecided
}

// recencyKey orders facts by event time, falling back to creation time so a
// fact without an explicit event still participates in the ordering.
func recencyKey(fact factview.Fact) time.Time {
	if !fact.EventTime.IsZero() {
		return fact.EventTime.UTC()
	}
	return fact.CreatedAt.UTC()
}

// decayBasis returns the time a fact's decay clock reads from: its event
// time, or a later recall when the read path has returned it since. A fact
// the caller keeps being served is not an unread archive entry, and its score
// must not fall as if it were one.
func decayBasis(fact factview.Fact, receipt recall.Receipt) time.Time {
	basis := recencyKey(fact)
	if last := receipt.Last.UTC(); last.After(basis) {
		return last
	}
	return basis
}

// decayScore returns the retention score of one fact. Recalls compensate
// decay and never add to it: each receipt forgives count/(count+RecallSaturation)
// of the gap between the decayed score and 1, so the factor stays in
// [decay, 1] and a fact recalled more often is worth more, never more than a
// fresh one.
func decayScore(fact factview.Fact, now time.Time, config Config, receipt recall.Receipt) float64 {
	score := 1.0
	if age := now.Sub(decayBasis(fact, receipt)); age > 0 {
		score = math.Pow(2, -float64(age)/float64(config.DecayHalfLife))
		if score < 0.01 {
			score = 0.01
		}
	}
	if count := receipt.Count; count > 0 {
		share := float64(count) / (float64(count) + config.RecallSaturation)
		score += (1 - score) * share
	}
	return score
}

func normalizeEntity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// entityIndex resolves the supersede candidates of one conversation: which
// facts a scan of one fact has to compare with. Sharing an entity is the
// necessary condition of a supersede, so the pairs the index leaves out are
// pairs no scan needed to look at -- for a scope whose facts carry a couple
// of entities each, most of them.
//
// The positions are indices into the recency-ordered facts, so "later in the
// scan" is "greater position", and every bucket is ascending by construction.
type entityIndex struct {
	facts    []factview.Fact
	entities [][]string
	byEntity map[string][]int
	// tokenCache caches one fact's token set for the pairs that get compared: the
	// pair-wise scan re-tokenized both texts for every pair it tested, and a
	// fact is the older side once and the newer side as often as it shares an
	// entity with one.
	tokenCache []map[string]struct{}
	// subjectCache caches one fact's subject for the pairs a relation
	// judgment compares: reading it scans the fact's own text for each of its
	// entities, and a fact is the older side of such a comparison at most
	// once, but the newer side as often as it states a recognized relation.
	subjectCache []*string
	// classCache caches one fact's relation class: folding a predicate is a
	// comparison key, not a lookup, and a fact is tested as the older side of
	// a pair once but as the newer side as often as it shares an entity with
	// a later fact.
	classCache []*relationClass
}

// relationClass is one fact's predicate read against the vocabulary: the class
// it states, and whether the vocabulary recognizes the predicate at all.
type relationClass struct {
	name  string
	known bool
}

func newEntityIndex(facts []factview.Fact) *entityIndex {
	index := &entityIndex{
		facts:        facts,
		entities:     make([][]string, len(facts)),
		byEntity:     make(map[string][]int, len(facts)*2),
		tokenCache:   make([]map[string]struct{}, len(facts)),
		subjectCache: make([]*string, len(facts)),
		classCache:   make([]*relationClass, len(facts)),
	}
	for position, fact := range facts {
		if len(fact.Entities) == 0 {
			continue
		}
		normalized := make([]string, 0, len(fact.Entities))
		for _, entity := range fact.Entities {
			normalized = append(normalized, normalizeEntity(entity))
		}
		sort.Strings(normalized)
		normalized = slices.Compact(normalized)
		index.entities[position] = normalized
		for _, entity := range normalized {
			index.byEntity[entity] = append(index.byEntity[entity], position)
		}
	}
	return index
}

// candidates returns the positions of the facts sharing an entity with the
// fact at position, later in the scan's order, ascending. A fact with no
// entities has no candidates, and the caller does not ask for one.
func (index *entityIndex) candidates(position int) []int {
	var merged []int
	for _, entity := range index.entities[position] {
		bucket := index.byEntity[entity]
		// Only the tail of the bucket is later than this fact; the rest was
		// scanned when those positions were the fact being judged.
		merged = append(merged, bucket[sort.SearchInts(bucket, position+1):]...)
	}
	if len(merged) == 0 {
		return nil
	}
	// A fact carrying two entities a later fact also carries appears twice;
	// the pair-wise scan visited it once.
	sort.Ints(merged)
	return slices.Compact(merged)
}

// tokens returns the fact's token set, built on first use.
func (index *entityIndex) tokens(position int) map[string]struct{} {
	if index.tokenCache[position] == nil {
		index.tokenCache[position] = textsim.Set(index.facts[position].Text)
	}
	return index.tokenCache[position]
}

// classOf returns the relation class of one fact, read once per fact.
func (index *entityIndex) classOf(relations relationIndex, position int) relationClass {
	if index.classCache[position] == nil {
		name, known := relations.classOf(index.facts[position].Predicate)
		index.classCache[position] = &relationClass{name: name, known: known}
	}
	return *index.classCache[position]
}

// subject returns the fact's subject as the schema lets it be read: the entity
// the fact's own text names first. The extractor writes facts as self-contained
// sentences that name who they are about, so the earliest name is the subject;
// a fact whose entities do not appear in its text has no subject, and no
// relation judgment is made about it rather than one made from a guess.
func (index *entityIndex) subject(position int) string {
	if index.subjectCache[position] == nil {
		value := index.readSubject(position)
		index.subjectCache[position] = &value
	}
	return *index.subjectCache[position]
}

func (index *entityIndex) readSubject(position int) string {
	entities := index.entities[position]
	if len(entities) == 0 {
		return ""
	}
	text := factview.CanonicalContent(index.facts[position].Text)
	subject, at := "", -1
	for _, entity := range entities {
		// An empty name is a slot the extractor left unfilled, not an entity
		// the fact names: it matches every text at offset 0 and would read as
		// every fact's subject at once.
		if entity == "" {
			continue
		}
		offset := strings.Index(text, entity)
		if offset < 0 {
			continue
		}
		// Ties are broken by name so the choice is a function of the fact:
		// one text naming two entities at the same offset has neither first.
		if at < 0 || offset < at || (offset == at && entity < subject) {
			subject, at = entity, offset
		}
	}
	return subject
}

// statesADifferentValue reports whether two facts about one single-valued
// relation state different values of it, which is what makes the older one
// stale rather than merely restated. Only what the schema can decide counts:
// the entities other than the subject. A pair naming the same object states the
// same value -- "moved to Berlin" and "lives in Berlin" agree about the place,
// so the older is compatible with the newer, not replaced by it -- and a pair
// naming none cannot be told apart here, which leaves the relation's own
// single-valuedness to decide it: one subject lives in one place, and two facts
// stating where it lives are one current value and one stale one.
func (index *entityIndex) statesADifferentValue(left, right int, subject string) bool {
	leftObjects := index.objects(left, subject)
	rightObjects := index.objects(right, subject)
	if len(leftObjects) == 0 || len(rightObjects) == 0 {
		return true
	}
	return !slices.Equal(leftObjects, rightObjects)
}

// objects returns the fact's entities other than its subject: what the fact
// states besides who it is about. These are the index's normalized, sorted,
// compacted names, so two facts stating the same object compare equal.
func (index *entityIndex) objects(position int, subject string) []string {
	entities := index.entities[position]
	objects := make([]string, 0, len(entities))
	for _, entity := range entities {
		if entity != "" && entity != subject {
			objects = append(objects, entity)
		}
	}
	return objects
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
