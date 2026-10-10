// Package bm25 implements a small in-memory BM25 text index over a
// fixed document set. It is deliberately dependency-free: a catalog of
// a few hundred to a few thousand short documents — tool definitions,
// skill files, retrieved records — is exactly the regime where a
// hand-rolled inverted index beats pulling in a search engine.
//
// A document carries a name (an identifier such as a tool or command
// name) and a text (its description). Name hits score higher than text
// hits, and a query term also matches index terms by prefix, so a
// fragment like "webf" still surfaces "webfetch". Tokens are
// lowercased word runs; CJK scripts are indexed as single characters
// plus adjacent bigrams, which lets Chinese queries match fragments
// without a segmentation dictionary.
//
// The index keeps term statistics only — it does not retain document
// text — and is immutable once built, so concurrent searches are safe.
package bm25

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Doc is one indexed document. ID identifies it in results, Name is
// the boosted field (identifiers such as tool or command names) and
// Text is the body field (descriptions). Either field may be empty.
type Doc struct {
	ID   string
	Name string
	Text string
}

// TermDoc is one indexed document whose fields are already tokenized:
// each map holds term frequencies. Callers that persist or cache
// tokens — a projection storing per-document term counts, say — index
// them directly instead of re-tokenizing text.
//
// The terms must come from Tokenize (or a tokenizer that splits the
// same way): queries are always tokenized with Tokenize, so terms
// produced by a different tokenizer may never match. Frequencies must
// be positive; nil maps are fine.
type TermDoc struct {
	ID   string
	Name map[string]int
	Text map[string]int
}

// Result is one match from a Search, ranked by descending score.
type Result struct {
	ID    string
	Score float64
}

// Scoring defaults: the standard Okapi BM25 parameters.
const (
	DefaultK1 = 1.2
	DefaultB  = 0.75
)

// Field weights: a term in the name field scores higher than the same
// term in the text field.
const (
	nameBoost = 3.0
	textBoost = 1.0
)

// defaultPrefixWeight discounts a query term that only prefixes an
// index term, so "re" still surfaces "resume" but an exact match
// outranks it.
const defaultPrefixWeight = 0.5

// Field slots in Index.fields.
const (
	nameField = iota
	textField
)

var fieldBoosts = [2]float64{nameBoost, textBoost}

// config is the resolved scoring configuration.
type config struct {
	k1           float64
	b            float64
	prefixWeight float64
}

func defaultConfig() config {
	return config{k1: DefaultK1, b: DefaultB, prefixWeight: defaultPrefixWeight}
}

// Option adjusts scoring when an index is built. Options are applied in
// order and validated by New and NewFromTerms.
type Option func(*config)

// WithK1 sets the term-frequency saturation parameter (default
// DefaultK1). k1 must be finite and positive.
func WithK1(k1 float64) Option { return func(c *config) { c.k1 = k1 } }

// WithB sets the document-length normalisation strength (default
// DefaultB). b must be in [0,1]; WithB(0) disables length
// normalisation rather than falling back to the default.
func WithB(b float64) Option { return func(c *config) { c.b = b } }

// WithPrefixWeight sets the discount applied when a query term only
// prefixes an index term (default 0.5). The weight must be in [0,1];
// WithPrefixWeight(0) disables prefix matching entirely.
func WithPrefixWeight(weight float64) Option {
	return func(c *config) { c.prefixWeight = weight }
}

func (c config) validate() error {
	if math.IsNaN(c.k1) || math.IsInf(c.k1, 0) || c.k1 <= 0 {
		return errors.New("bm25: k1 must be finite and positive")
	}
	if math.IsNaN(c.b) || math.IsInf(c.b, 0) || c.b < 0 || c.b > 1 {
		return errors.New("bm25: b must be in [0,1]")
	}
	if math.IsNaN(c.prefixWeight) || math.IsInf(c.prefixWeight, 0) ||
		c.prefixWeight < 0 || c.prefixWeight > 1 {
		return errors.New("bm25: prefix weight must be in [0,1]")
	}
	return nil
}

func resolveConfig(opts []Option) (config, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// Index is an immutable BM25 index over a fixed document set. The zero
// value is an empty index that answers every query with no results.
// An Index built by New or NewFromTerms is safe for concurrent use.
type Index struct {
	ids    []string
	fields [2]fieldIndex // nameField, textField
	nDocs  int
	config config
}

// New builds an index over docs, tokenizing Name and Text. Document
// IDs must be unique; ties in Search keep the order docs arrive in.
func New(docs []Doc, opts ...Option) (*Index, error) {
	cfg, err := resolveConfig(opts)
	if err != nil {
		return nil, err
	}
	index := &Index{ids: make([]string, len(docs)), nDocs: len(docs), config: cfg}
	for doc, d := range docs {
		index.ids[doc] = d.ID
		index.fields[nameField].addText(int32(doc), d.Name)
		index.fields[textField].addText(int32(doc), d.Text)
	}
	index.finish()
	return index, nil
}

// NewFromTerms builds an index over documents whose fields are already
// tokenized; see TermDoc for why a caller would want that. Documents
// with invalid term frequencies are rejected.
func NewFromTerms(docs []TermDoc, opts ...Option) (*Index, error) {
	cfg, err := resolveConfig(opts)
	if err != nil {
		return nil, err
	}
	index := &Index{ids: make([]string, len(docs)), nDocs: len(docs), config: cfg}
	for doc, d := range docs {
		index.ids[doc] = d.ID
		if err := index.fields[nameField].addTerms(int32(doc), d.ID, "name", d.Name); err != nil {
			return nil, err
		}
		if err := index.fields[textField].addTerms(int32(doc), d.ID, "text", d.Text); err != nil {
			return nil, err
		}
	}
	index.finish()
	return index, nil
}

// fieldIndex holds the per-field statistics a BM25 score needs: each
// document's field length and the postings of every term.
//
// The postings are a flat, term-major slice instead of a
// map[term]map[doc]tf: a Go map costs ~200 bytes before it holds
// anything, so one map per distinct term would dominate the heap for a
// corpus of a few hundred documents. Terms are kept sorted, so a query
// term resolves with one binary search and its prefix matches with a
// scan from there.
type fieldIndex struct {
	lengths  []int
	totalLen int
	avg      float64
	terms    []termEntry
	postings []posting
	// pending collects postings while documents are added; finalize
	// folds them into terms/postings and drops the slice.
	pending []pendingPosting
}

// termEntry is one distinct term and the slice of postings it owns.
type termEntry struct {
	term string
	off  int32
	df   int32
}

// posting is one document's term frequency for the owning term.
type posting struct {
	doc int32
	tf  int32
}

// pendingPosting is a collected posting before finalize groups it under
// its term.
type pendingPosting struct {
	term string
	doc  int32
	tf   int32
}

// finish finalizes the postings and resolves the per-field average
// length used by the length normalisation.
func (ix *Index) finish() {
	for field := range ix.fields {
		ix.fields[field].finalize()
		if ix.nDocs > 0 {
			ix.fields[field].avg = float64(ix.fields[field].totalLen) / float64(ix.nDocs)
		}
	}
}

// addText tokenizes and adds one document's text for a field. Terms
// are deduplicated by sorting the token slice in place, so adding a
// document allocates no per-document map.
//
// The field length is the total token count, repetitions included —
// the standard BM25 |D|. Term frequencies saturate in the score, so a
// repeated term adds length without exploding its contribution.
func (f *fieldIndex) addText(doc int32, text string) {
	tokens := Tokenize(text)
	f.lengths = append(f.lengths, len(tokens))
	f.totalLen += len(tokens)
	sort.Strings(tokens)
	for i := 0; i < len(tokens); {
		j := i + 1
		for j < len(tokens) && tokens[j] == tokens[i] {
			j++
		}
		f.pending = append(f.pending, pendingPosting{
			term: tokens[i], doc: doc, tf: int32(j - i),
		})
		i = j
	}
}

// addTerms adds one document's pre-tokenized field. Frequencies must
// be positive, so a caller cannot smuggle in a document that
// contributes nothing but length.
func (f *fieldIndex) addTerms(doc int32, id, field string, terms map[string]int) error {
	total := 0
	for term, tf := range terms {
		if term == "" {
			return fmt.Errorf("bm25: document %q: empty %s term", id, field)
		}
		if tf <= 0 || tf > math.MaxInt32 {
			return fmt.Errorf("bm25: document %q: %s term %q: invalid frequency %d",
				id, field, term, tf)
		}
		total += tf
		f.pending = append(f.pending, pendingPosting{term: term, doc: doc, tf: int32(tf)})
	}
	f.lengths = append(f.lengths, total)
	f.totalLen += total
	return nil
}

// finalize sorts the collected postings by term and folds them into the
// flat term table. Both tables are sized from their own counts: postings
// per term average well above one, so sizing the term table from
// len(pending) would reserve mostly empty entries and retain them for
// the index's lifetime. A term's postings stay in document order, which
// is what lets docFreq merge two fields without a set.
func (f *fieldIndex) finalize() {
	pending := f.pending
	f.pending = nil
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].term != pending[j].term {
			return pending[i].term < pending[j].term
		}
		return pending[i].doc < pending[j].doc
	})
	distinct := 0
	for i := range pending {
		if i == 0 || pending[i-1].term != pending[i].term {
			distinct++
		}
	}
	f.postings = make([]posting, 0, len(pending))
	f.terms = make([]termEntry, 0, distinct)
	for i, p := range pending {
		if i == 0 || pending[i-1].term != p.term {
			f.terms = append(f.terms, termEntry{
				term: p.term,
				off:  int32(len(f.postings)),
			})
		}
		f.postings = append(f.postings, posting{doc: p.doc, tf: p.tf})
		f.terms[len(f.terms)-1].df++
	}
}

// lookup returns the entry of an exact term.
func (f *fieldIndex) lookup(term string) (termEntry, bool) {
	i, ok := f.search(term)
	if !ok {
		return termEntry{}, false
	}
	return f.terms[i], true
}

// search returns the position of the first term >= target and whether
// that term is an exact match.
func (f *fieldIndex) search(target string) (int, bool) {
	i := sort.Search(len(f.terms), func(i int) bool {
		return f.terms[i].term >= target
	})
	return i, i < len(f.terms) && f.terms[i].term == target
}

// postingsOf returns the postings of term, which are in document
// order.
func (f *fieldIndex) postingsOf(term string) []posting {
	entry, ok := f.lookup(term)
	if !ok {
		return nil
	}
	return f.postings[entry.off : entry.off+entry.df]
}

// eachPrefix calls fn for every indexed term that has prefix as a
// proper prefix. Terms are sorted, so the scan starts at the first term
// >= prefix and stops at the first term that no longer matches.
func (f *fieldIndex) eachPrefix(prefix string, fn func(termEntry)) {
	i, _ := f.search(prefix)
	for ; i < len(f.terms); i++ {
		term := f.terms[i].term
		if !strings.HasPrefix(term, prefix) {
			return
		}
		if len(term) > len(prefix) {
			fn(f.terms[i])
		}
	}
}

// termMatch is one index term a query term resolves to.
type termMatch struct {
	term   string
	df     int
	prefix bool
}

// termMatches expands the unique query terms to index terms: each term
// itself plus, when prefix weighting is enabled, every index term that
// has it as a proper prefix. A term that is both an exact query term
// and another term's prefix hit keeps the exact form.
func (ix *Index) termMatches(terms []string) []termMatch {
	matches := make([]termMatch, 0, len(terms)*2)
	index := make(map[string]int, len(terms))
	add := func(term string, prefix bool) {
		at, ok := index[term]
		if !ok {
			index[term] = len(matches)
			matches = append(matches, termMatch{
				term: term, df: ix.docFreq(term), prefix: prefix,
			})
			return
		}
		if !prefix {
			matches[at].prefix = false
		}
	}
	for _, term := range terms {
		add(term, false)
		if ix.config.prefixWeight == 0 {
			continue
		}
		for field := range ix.fields {
			ix.fields[field].eachPrefix(term, func(entry termEntry) {
				add(entry.term, true)
			})
		}
	}
	return matches
}

// Search ranks documents against query with BM25 over both the name
// and text fields and returns at most limit results in descending
// score. Query terms also match by prefix when prefix weighting is
// enabled (see WithPrefixWeight).
//
// Ties keep the order of the documents as they were passed to New, so
// a caller that needs a specific tie-break sorts its documents first
// (by name, say). An empty query, a query with no indexable terms or
// a corpus with no match returns nil; limit <= 0 means no limit.
func (ix *Index) Search(query string, limit int) []Result {
	return ix.SearchTerms(uniqueTokens(query), limit)
}

// SearchTerms is Search over an already-tokenized query, for a caller
// whose index terms did not come from Tokenize (see TermDoc): the
// terms must be split the same way the indexed fields were, or
// queries cannot match. Repeated terms count once, as in Search.
// Terms that are not index terms simply score nothing, so a caller
// cannot distinguish "no such term" from "no match for it" — pass the
// terms you would have passed to Search's tokenizer.
func (ix *Index) SearchTerms(query []string, limit int) []Result {
	terms := uniqueTerms(query)
	if len(terms) == 0 || ix.nDocs == 0 {
		return nil
	}

	scores := make([]float64, ix.nDocs)
	k1, b := ix.config.k1, ix.config.b
	// One score per document, accumulated term by term, so the work is
	// proportional to the postings a term actually has rather than to
	// documents × matched terms.
	for _, match := range ix.termMatches(terms) {
		idf := math.Log(1 +
			(float64(ix.nDocs)-float64(match.df)+0.5)/(float64(match.df)+0.5))
		for field, boost := range fieldBoosts {
			f := &ix.fields[field]
			if f.avg == 0 {
				continue
			}
			if match.prefix {
				boost *= ix.config.prefixWeight
			}
			for _, p := range f.postingsOf(match.term) {
				tf := float64(p.tf)
				denom := tf + k1*(1-b+b*float64(f.lengths[p.doc])/f.avg)
				scores[p.doc] += boost * idf * tf * (k1 + 1) / denom
			}
		}
	}

	ranked := make([]Result, 0, ix.nDocs)
	for doc, score := range scores {
		if score > 0 {
			ranked = append(ranked, Result{ID: ix.ids[doc], Score: score})
		}
	}
	if len(ranked) == 0 {
		return nil
	}
	// Stable so equal scores keep the caller's document order.
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].Score > ranked[j].Score
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

// docFreq returns how many documents contain term in either field. A
// document that hits in both its name and its text counts once, so
// this is the union across fields rather than the sum.
func (ix *Index) docFreq(term string) int {
	name := ix.fields[nameField].postingsOf(term)
	text := ix.fields[textField].postingsOf(term)
	switch {
	case len(name) == 0:
		return len(text)
	case len(text) == 0:
		return len(name)
	}
	// Both lists are in document order: merge and count distinct docs.
	df, i, j := 0, 0, 0
	for i < len(name) && j < len(text) {
		switch {
		case name[i].doc == text[j].doc:
			i, j = i+1, j+1
		case name[i].doc < text[j].doc:
			i++
		default:
			j++
		}
		df++
	}
	return df + (len(name) - i) + (len(text) - j)
}

// uniqueTokens tokenizes the query and keeps each term once, so a
// repeated word cannot double-count its IDF contribution.
func uniqueTokens(query string) []string {
	return uniqueTerms(Tokenize(query))
}

// uniqueTerms keeps each distinct non-empty term once, in
// first-appearance order.
func uniqueTerms(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	unique := make([]string, 0, len(terms))
	for _, term := range terms {
		if term == "" {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		unique = append(unique, term)
	}
	return unique
}
