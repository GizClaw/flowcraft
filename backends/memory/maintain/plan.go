// Package maintain implements host-invoked memory maintenance: it detects
// superseded facts (soft merge) and stale facts (decay) and records a
// read-path overlay, keeping the canonical fact text immutable and ADD-only.
package maintain

import (
	"errors"
	"fmt"
	"math"
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
const AlgorithmVersion = "maintain-v4"

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
}

func (config Config) withDefaults() Config {
	if config.SimilarityThreshold == 0 {
		config.SimilarityThreshold = 0.5
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
	return nil
}

// ProducerVersion names the exact pass whose findings an overlay holds: the
// policy version plus the tuning those findings were measured with. Two
// deployments that disagree about the tuning judge the same facts differently,
// so an overlay records the pass that wrote it and is served only where the
// same pass runs.
func (config Config) ProducerVersion() string {
	config = config.withDefaults()
	return fmt.Sprintf("%s+similarity=%g,supersede=%g,half_life=%s,floor=%g,recall_saturation=%g,relations=%s",
		AlgorithmVersion, config.SimilarityThreshold, config.SupersedeFactor,
		config.DecayHalfLife, config.DecayFloor, config.RecallSaturation,
		newRelationIndex(config.Relations).digest)
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
	ReclaimedChunks  int    `json:"reclaimed_chunks,omitempty"`
	AlgorithmVersion string `json:"algorithm_version"`
	// RecalledFacts counts the scope's facts that carried a receipt: how many
	// of them the read path has actually returned. It is the wiring signal --
	// a deployment whose receipts never arrive reports zero here however long
	// it runs -- and it stays out of the overlay.
	RecalledFacts int `json:"recalled_facts,omitempty"`
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
	if err := scope.Validate(); err != nil {
		return Plan{}, err
	}
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return Plan{}, err
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
				return Plan{}, keyErr
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
			olderSubject := ""
			if olderClass.known {
				olderSubject = entities.subject(position)
			}
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
				break
			}
		}
	}
	sort.Slice(plan.Supersedes, func(i, j int) bool { return plan.Supersedes[i].FactID < plan.Supersedes[j].FactID })
	sort.Slice(plan.Decays, func(i, j int) bool { return plan.Decays[i].FactID < plan.Decays[j].FactID })
	return plan, nil
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
