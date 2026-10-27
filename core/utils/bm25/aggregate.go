package bm25

import "math"

// Aggregate is a scoring set's statistics without its documents: how many
// documents it holds, how long their text fields are in total, and how many of
// them contain each term.
//
// It exists for readers that cannot hold their whole corpus in one place. BM25
// weights are properties of the set being scored, not of the part a reader
// happens to hold: scoring one shard against its own counts and the next against
// its own would weigh the same term differently in each, and the scores would no
// longer be comparable across the two. A reader that accumulates an Aggregate
// first and then scores every part against it gets exactly the ranking it would
// have got from one index over the whole set.
type Aggregate struct {
	// Documents counts the documents the scoring set holds.
	Documents int
	// TextLength is the summed length of their text fields, which is what the
	// length normalisation divides by.
	TextLength int
	// TextTerms is the document frequency of every term in the text field: how
	// many documents contain it, each document counted once.
	TextTerms map[string]int
}

// Aggregate returns the statistics this index scores with, over its text field.
func (ix *Index) Aggregate() Aggregate {
	aggregate := Aggregate{
		Documents:  ix.nDocs,
		TextLength: ix.fields[textField].totalLen,
		TextTerms:  make(map[string]int, len(ix.fields[textField].terms)),
	}
	for _, entry := range ix.fields[textField].terms {
		aggregate.TextTerms[entry.term] = int(entry.df)
	}
	return aggregate
}

// Add folds other into aggregate. The two are assumed to describe disjoint
// document sets -- one document is indexed in one part of a sharded corpus --
// so counts add and a term's document frequency adds with them.
func (aggregate *Aggregate) Add(other Aggregate) {
	if aggregate.TextTerms == nil {
		aggregate.TextTerms = make(map[string]int, len(other.TextTerms))
	}
	aggregate.Documents += other.Documents
	aggregate.TextLength += other.TextLength
	for term, frequency := range other.TextTerms {
		aggregate.TextTerms[term] += frequency
	}
}

// Scorer scores single documents against an Aggregate. It is the scoring half
// of an Index without the postings: what a reader needs when it decides
// itself which documents to look at -- because they are stored one shard at a
// time -- and only needs the kernel to say what one of them scores.
//
// The parameters are the index's: NewScorer takes the same options New and
// NewFromTerms do. Only the text field is scored, and only exact terms match: a
// document indexed with no name field and a caller that disabled prefix
// matching see the same scores here as they would from SearchTerms.
type Scorer struct {
	documents int
	average   float64
	frequency map[string]int
	config    config
}

// NewScorer builds a scorer over an aggregate. The aggregate's document count
// and lengths decide the length normalisation, and its document frequencies
// decide the term weights; the options decide k1, b and the prefix weight.
func NewScorer(aggregate Aggregate, opts ...Option) (*Scorer, error) {
	cfg, err := resolveConfig(opts)
	if err != nil {
		return nil, err
	}
	scorer := &Scorer{documents: aggregate.Documents, frequency: aggregate.TextTerms, config: cfg}
	if scorer.documents > 0 {
		scorer.average = float64(aggregate.TextLength) / float64(scorer.documents)
	}
	return scorer, nil
}

// Score returns the BM25 score of one document's text field for a query that
// has already been tokenized. length is the document's text-field length --
// the sum of its term frequencies -- and terms its term frequencies.
//
// The value is the one SearchTerms would give the same document as part of the
// aggregate's whole set: term saturation, length normalisation and inverse
// document frequency are the kernel's own, computed from the aggregate rather
// than from the documents this scorer happens to be shown.
func (scorer *Scorer) Score(terms map[string]int, length int, query []string) float64 {
	if scorer == nil || scorer.documents == 0 || scorer.average == 0 || len(terms) == 0 {
		return 0
	}
	k1, b := scorer.config.k1, scorer.config.b
	score := 0.0
	for _, term := range uniqueTerms(query) {
		frequency := terms[term]
		if frequency <= 0 {
			continue
		}
		df := scorer.frequency[term]
		if df == 0 {
			continue
		}
		idf := math.Log(1 + (float64(scorer.documents)-float64(df)+0.5)/(float64(df)+0.5))
		tf := float64(frequency)
		denominator := tf + k1*(1-b+b*float64(length)/scorer.average)
		score += textBoost * idf * tf * (k1 + 1) / denominator
	}
	return score
}
