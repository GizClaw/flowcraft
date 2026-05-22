package ranker

import (
	"context"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/evolution"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/text/stem"
	"github.com/GizClaw/flowcraft/sdk/text/tokenize"
)

// rankTokenizer folds query / fact text into the canonical BM25
// vocabulary keys used for term-match scoring. Stem.Porter (legacy
// Porter1) is pinned so the ranker stays compatible with the
// existing rank_boost telemetry; switching to Snowball Porter2
// (tokenize.Simple's default) would shift collapsed forms (e.g.
// "general"/"generic") and invalidate persisted boost baselines.
var rankTokenizer = &tokenize.Simple{Stemmer: stem.Porter}

const (
	defaultHalfLife         = 30 * 24 * time.Hour
	defaultSupersededFactor = 0.5

	sourceTimeline = "timeline"
	sourceRelation = "relation"
	sourceProfile  = "profile"
)

// Default is the deterministic post-materialize ranker (Phase E.1).
type Default struct {
	halfLife         time.Duration
	supersededFactor float64
}

// NewDefault constructs a ranker with v1-aligned decay defaults.
func NewDefault(opts ...Option) *Default {
	d := &Default{
		halfLife:         defaultHalfLife,
		supersededFactor: defaultSupersededFactor,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

var _ port.Ranker = (*Default)(nil)

// Rank implements port.Ranker.
func (d *Default) Rank(_ context.Context, in port.RankInput) port.RankOutput {
	items := append([]domain.ContextItem(nil), in.Items...)
	if len(items) == 0 {
		return port.RankOutput{}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	terms := significantQueryTerms(in.Intent.Text)
	termDF := queryTermDocumentFrequency(items, terms)
	var boosts, timeDecay, superseded int
	for i := range items {
		boost := factRankBoost(items[i], in.Intent, terms, termDF)
		if boost != 0 {
			boosts++
			items[i].Candidate.Score += boost
			if items[i].Candidate.Metadata == nil {
				items[i].Candidate.Metadata = map[string]any{}
			}
			items[i].Candidate.Metadata["rank_boost"] = boost
		}
		if d.halfLife > 0 {
			if decay := d.timeDecayFactor(items[i].Fact, now); decay < 1 {
				items[i].Candidate.Score *= decay
				timeDecay++
				if items[i].Candidate.Metadata == nil {
					items[i].Candidate.Metadata = map[string]any{}
				}
				items[i].Candidate.Metadata["time_decay"] = decay
			}
		}
		if domain.IsSuperseded(items[i].Fact) && d.supersededFactor > 0 && d.supersededFactor < 1 {
			items[i].Candidate.Score *= d.supersededFactor
			superseded++
			if items[i].Candidate.Metadata == nil {
				items[i].Candidate.Metadata = map[string]any{}
			}
			items[i].Candidate.Metadata["superseded_decay"] = d.supersededFactor
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Candidate.Score > items[j].Candidate.Score
	})
	if in.FinalCap > 0 && len(items) > in.FinalCap {
		items = items[:in.FinalCap]
	}
	return port.RankOutput{
		Items:                  items,
		BoostsApplied:          boosts,
		TimeDecayApplied:       timeDecay,
		SupersededDecayApplied: superseded,
	}
}

func (d *Default) timeDecayFactor(f domain.TemporalFact, now time.Time) float64 {
	ts := domain.EffectiveTimestamp(f)
	if ts.IsZero() {
		return 1
	}
	age := now.Sub(ts).Seconds()
	if age < 0 {
		age = 0
	}
	return math.Exp(-math.Ln2 * age / d.halfLife.Seconds())
}

func factRankBoost(item domain.ContextItem, intent domain.QueryIntent, terms []string, termDF map[string]int) float64 {
	f := item.Fact
	var boost float64
	termMatches := factTermMatchCount(f, terms)
	hasTextMatch := termMatches > 0
	if hasTextMatch && factMatchesAnyEntity(f, intent.Entities) {
		boost += 0.015
	}
	if hasTextMatch && intent.Subject != "" && factMatchesEntity(f, intent.Subject) {
		boost += 0.015
	}
	if hasTextMatch && factKindMatches(f.Kind, intent.Kinds) {
		boost += 0.012
	}
	if hasTextMatch && temporalIntent(intent) {
		if f.ValidFrom != nil || !f.ObservedAt.IsZero() {
			boost += 0.006
		}
		if candidateHasSource(item.Candidate, sourceTimeline) {
			boost += 0.008
		}
	}
	if hasTextMatch && intentActivatesRelation(intent) && candidateHasSource(item.Candidate, sourceRelation) {
		boost += 0.01
	}
	if hasTextMatch && intentActivatesProfile(intent) && candidateHasSource(item.Candidate, sourceProfile) {
		boost += 0.01
	}
	boost += termMatchBoost(termMatches)
	boost += queryCoverageBoost(f, terms, termDF)
	boost += selectedEvidenceCoverageBoost(item, terms, termDF)
	boost += numericRelevanceBoost(item, intent, terms, hasTextMatch)
	if f.Confidence > 0 {
		c := f.Confidence
		if c > 1 {
			c = 1
		}
		boost += c * 0.004
	}
	boost += (evolution.FeedbackBoost(f.Reinforcement, f.Penalty) - 1) * 0.02
	return boost
}

func selectedEvidenceCoverageBoost(item domain.ContextItem, terms []string, termDF map[string]int) float64 {
	if len(item.Evidence) == 0 || len(terms) == 0 {
		return 0
	}
	matched := evidenceMatchedTerms(item.Evidence, terms)
	if len(matched) == 0 {
		return 0
	}
	coverage := float64(len(matched)) / float64(len(terms))
	var rarity float64
	for _, term := range matched {
		df := termDF[term]
		if df <= 0 {
			continue
		}
		rarity += 1 / float64(df)
	}
	rarity /= float64(len(terms))
	boost := coverage*0.006 + rarity*0.009
	if boost > 0.015 {
		return 0.015
	}
	return boost
}

func numericRelevanceBoost(item domain.ContextItem, intent domain.QueryIntent, terms []string, hasTextMatch bool) float64 {
	var boost float64
	matchedNumeric := 0
	for _, term := range factMatchedTerms(item.Fact, numericTerms(terms)) {
		if containsDigit(term) {
			matchedNumeric++
		}
	}
	if matchedNumeric > 0 {
		if matchedNumeric > 3 {
			matchedNumeric = 3
		}
		boost += float64(matchedNumeric) * 0.006
	}
	if numericIntent(intent.Text) && hasTextMatch && factHasNumericToken(item) {
		boost += 0.006
	}
	return boost
}

func queryCoverageBoost(f domain.TemporalFact, terms []string, termDF map[string]int) float64 {
	if len(terms) == 0 {
		return 0
	}
	matched := factMatchedTerms(f, terms)
	if len(matched) == 0 {
		return 0
	}
	coverage := float64(len(matched)) / float64(len(terms))
	var rarity float64
	for _, term := range matched {
		df := termDF[term]
		if df <= 0 {
			continue
		}
		rarity += 1 / float64(df)
	}
	rarity /= float64(len(terms))
	// Coverage rewards facts that answer more of the query, while
	// rarity keeps common subject-only matches from swamping specific
	// evidence such as "degree", "Tampa", or "25 minutes".
	return coverage*0.012 + rarity*0.018
}

func temporalIntent(intent domain.QueryIntent) bool {
	if !intent.TimeRange.IsZero() {
		return true
	}
	for _, k := range intent.Kinds {
		switch k {
		case domain.KindEvent, domain.KindState, domain.KindPlan:
			return true
		}
	}
	return false
}

func factKindMatches(kind domain.FactKind, kinds []domain.FactKind) bool {
	return slices.Contains(kinds, kind)
}

func factMatchesAnyEntity(f domain.TemporalFact, entities []string) bool {
	for _, e := range entities {
		if factMatchesEntity(f, e) {
			return true
		}
	}
	return false
}

func factMatchesEntity(f domain.TemporalFact, entity string) bool {
	entity = normalizeRankText(entity)
	if entity == "" {
		return false
	}
	for _, s := range []string{f.Subject, f.Object, f.Location} {
		if normalizeRankText(s) == entity {
			return true
		}
	}
	for _, e := range f.Entities {
		if normalizeRankText(e) == entity {
			return true
		}
	}
	for _, p := range f.Participants {
		if normalizeRankText(p) == entity {
			return true
		}
	}
	return false
}

func candidateHasSource(c domain.Candidate, source string) bool {
	if c.Source == source {
		return true
	}
	if c.Metadata == nil {
		return false
	}
	sources, _ := c.Metadata["sources"].([]string)
	return slices.Contains(sources, source)
}

func factTermMatchCount(f domain.TemporalFact, terms []string) int {
	if len(terms) == 0 {
		return 0
	}
	primary := tokenizeRankBlob(f.Content + " " + f.Subject + " " + f.Predicate + " " + f.Object)
	var secondary []string
	secondary = append(secondary, tokenizeRankBlob(f.EvidenceText)...)
	for _, ref := range f.EvidenceRefs {
		if ref.Text != "" {
			secondary = append(secondary, tokenizeRankBlob(ref.Text)...)
		}
	}
	matches := 0
	for _, term := range terms {
		if slices.Contains(primary, term) {
			matches += 2
			continue
		}
		if slices.Contains(secondary, term) {
			matches++
		}
	}
	if matches > 10 {
		matches = 10
	}
	return matches
}

func queryTermDocumentFrequency(items []domain.ContextItem, terms []string) map[string]int {
	out := make(map[string]int, len(terms))
	if len(items) == 0 || len(terms) == 0 {
		return out
	}
	for _, item := range items {
		seen := map[string]struct{}{}
		for _, term := range factMatchedTerms(item.Fact, terms) {
			seen[term] = struct{}{}
		}
		for term := range seen {
			out[term]++
		}
	}
	return out
}

func factMatchedTerms(f domain.TemporalFact, terms []string) []string {
	if len(terms) == 0 {
		return nil
	}
	tokens := tokenizeRankBlob(f.Content + " " + f.Subject + " " + f.Predicate + " " + f.Object + " " + f.EvidenceText)
	for _, ref := range f.EvidenceRefs {
		tokens = append(tokens, tokenizeRankBlob(ref.Text)...)
	}
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, tok := range tokens {
		tokenSet[tok] = struct{}{}
	}
	var out []string
	for _, term := range terms {
		if _, ok := tokenSet[term]; ok {
			out = append(out, term)
		}
	}
	return out
}

func evidenceMatchedTerms(refs []domain.EvidenceRef, terms []string) []string {
	if len(refs) == 0 || len(terms) == 0 {
		return nil
	}
	var tokens []string
	for _, ref := range refs {
		tokens = append(tokens, tokenizeRankBlob(ref.Text)...)
	}
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, tok := range tokens {
		tokenSet[tok] = struct{}{}
	}
	var out []string
	for _, term := range terms {
		if _, ok := tokenSet[term]; ok {
			out = append(out, term)
		}
	}
	return out
}

func termMatchBoost(matches int) float64 {
	if matches <= 0 {
		return 0
	}
	return float64(matches) * 0.006
}

// significantQueryTerms folds the query text into deduped BM25
// vocabulary keys. tokenize.Simple owns lower-casing, stop-word
// filtering (sdk/text/stopword), and the lemma+stem composition; we
// post-filter to drop short non-numeric tokens while preserving
// numeric tokens such as "2" and "25" that are often the answer.
func significantQueryTerms(text string) []string {
	tokens := append(rankTokenizer.Tokenize(text), numericSurfaceTerms(text)...)
	return uniqueRankTokens(tokens)
}

// tokenizeRankBlob shares the same pipeline as significantQueryTerms
// — kept as a separate symbol so the caller-side intent (query vs
// fact blob) reads cleanly at the call site.
func tokenizeRankBlob(text string) []string {
	tokens := append(rankTokenizer.Tokenize(text), numericSurfaceTerms(text)...)
	return uniqueRankTokens(tokens)
}

func uniqueRankTokens(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if len(t) < 3 && !containsDigit(t) {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func numericTerms(terms []string) []string {
	if len(terms) == 0 {
		return nil
	}
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		if containsDigit(term) {
			out = append(out, term)
		}
	}
	return out
}

func factHasNumericToken(item domain.ContextItem) bool {
	if containsDigit(item.Fact.Content) ||
		containsDigit(item.Fact.Subject) ||
		containsDigit(item.Fact.Predicate) ||
		containsDigit(item.Fact.Object) ||
		containsDigit(item.Fact.EvidenceText) {
		return true
	}
	evidence := item.Evidence
	if len(evidence) == 0 {
		evidence = item.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		if containsDigit(ref.Text) {
			return true
		}
	}
	return false
}

func numericSurfaceTerms(text string) []string {
	if text == "" || !containsDigit(text) {
		return nil
	}
	var out []string
	for _, tok := range tokenize.SplitWords(text) {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok != "" && containsDigit(tok) {
			out = append(out, tok)
		}
	}
	return out
}

func numericIntent(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "how many") ||
		strings.Contains(lower, "how much") ||
		strings.Contains(lower, "how long") ||
		strings.Contains(lower, "number") ||
		strings.Contains(lower, "count") ||
		strings.Contains(lower, "total") ||
		strings.Contains(lower, "duration") ||
		strings.Contains(lower, "age")
}

func containsDigit(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func normalizeRankText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func intentActivatesRelation(intent domain.QueryIntent) bool {
	return intent.Subject != "" || intent.Predicate != "" || intent.Object != ""
}

func intentActivatesProfile(intent domain.QueryIntent) bool {
	return intent.Subject != ""
}

// Option configures a Default ranker.
type Option func(*Default)

// WithTimeDecay sets the half-life for recency decay (default 30d).
func WithTimeDecay(halfLife time.Duration) Option {
	return func(d *Default) { d.halfLife = halfLife }
}

// WithSupersededDecay sets the score multiplier for superseded facts (default 0.5).
func WithSupersededDecay(factor float64) Option {
	return func(d *Default) { d.supersededFactor = factor }
}
