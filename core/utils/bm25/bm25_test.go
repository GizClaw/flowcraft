package bm25

import (
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

func mustIndex(t *testing.T, docs []Doc, opts ...Option) *Index {
	t.Helper()
	index, err := New(docs, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return index
}

func resultIDs(results []Result) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.ID)
	}
	return ids
}

func TestSearchRanksNameAboveText(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "search", Name: "search", Text: "unrelated wording here"},
		{ID: "notes", Name: "notes", Text: "search old notes by keyword"},
	})

	// The name hit outranks the text-only hit for the same term.
	if ids := resultIDs(index.Search("search", 10)); !slices.Equal(ids, []string{"search", "notes"}) {
		t.Fatalf("Search(search) = %v, want [search notes]", ids)
	}

	// A text-only term still matches.
	if ids := resultIDs(index.Search("keyword", 10)); !slices.Equal(ids, []string{"notes"}) {
		t.Fatalf("Search(keyword) = %v, want [notes]", ids)
	}

	// No match returns nothing.
	if results := index.Search("zzzz", 10); len(results) != 0 {
		t.Fatalf("Search(zzzz) = %v, want none", results)
	}
}

func TestSearchMatchesCJKFragments(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "clear", Name: "clear", Text: "清理当前会话历史"},
		{ID: "compact", Name: "compact", Text: "压缩并总结当前会话"},
	})

	if ids := resultIDs(index.Search("清理", 10)); !slices.Equal(ids, []string{"clear"}) {
		t.Fatalf("Search(清理) = %v, want [clear]", ids)
	}
	if ids := resultIDs(index.Search("压缩", 10)); !slices.Equal(ids, []string{"compact"}) {
		t.Fatalf("Search(压缩) = %v, want [compact]", ids)
	}
	// A shared fragment matches both documents.
	ids := resultIDs(index.Search("会话", 10))
	if len(ids) != 2 || !slices.Contains(ids, "clear") || !slices.Contains(ids, "compact") {
		t.Fatalf("Search(会话) = %v, want both documents", ids)
	}
}

func TestSearchPrefixMatches(t *testing.T) {
	docs := []Doc{
		{ID: "resume", Name: "resume", Text: "pick and resume a past conversation"},
		{ID: "permissions", Name: "permissions", Text: "switch the sandbox permission mode"},
	}
	index := mustIndex(t, docs)

	// A fragment ranks the document whose name starts with it.
	if ids := resultIDs(index.Search("re", 10)); len(ids) == 0 || ids[0] != "resume" {
		t.Fatalf("Search(re) = %v, want resume first", ids)
	}
	if ids := resultIDs(index.Search("per", 10)); len(ids) == 0 || ids[0] != "permissions" {
		t.Fatalf("Search(per) = %v, want permissions first", ids)
	}
	// A prefix of a description word matches too.
	if ids := resultIDs(index.Search("conversa", 10)); !slices.Equal(ids, []string{"resume"}) {
		t.Fatalf("Search(conversa) = %v, want [resume]", ids)
	}

	// With prefix weighting disabled only exact terms match.
	exact := mustIndex(t, docs, WithPrefixWeight(0))
	if results := exact.Search("re", 10); len(results) != 0 {
		t.Fatalf("Search(re) without prefix matching = %v, want none", results)
	}
	if ids := resultIDs(exact.Search("resume", 10)); !slices.Equal(ids, []string{"resume"}) {
		t.Fatalf("Search(resume) without prefix matching = %v, want [resume]", ids)
	}
}

func TestSearchExactOutranksPrefix(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "res", Name: "res", Text: ""},
		{ID: "resume", Name: "resume", Text: ""},
	})

	// "res" is an exact term in one document and a prefix of the
	// other's: the exact match wins.
	if ids := resultIDs(index.Search("res", 10)); !slices.Equal(ids, []string{"res", "resume"}) {
		t.Fatalf("Search(res) = %v, want [res resume]", ids)
	}

	// When a term is both a query term and another query term's prefix
	// hit, it is scored as the exact hit, not the prefix one.
	alone := index.Search("resume", 10)
	combined := index.Search("res resume", 10)
	if len(alone) != 1 || len(combined) != 2 {
		t.Fatalf("Search(resume) = %v, Search(res resume) = %v", alone, combined)
	}
	if math.Abs(combined[0].Score-alone[0].Score) > 1e-12 {
		t.Fatalf("score of resume under (res resume) = %v, want %v",
			combined[0].Score, alone[0].Score)
	}
}

func TestSearchEmptyQueryAndEmptyIndex(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "a", Name: "alpha", Text: "first"},
	})
	for _, query := range []string{"", "   ", "!!!", "、"} {
		if results := index.Search(query, 5); len(results) != 0 {
			t.Errorf("Search(%q) = %v, want none", query, results)
		}
	}
	if results := mustIndex(t, nil).Search("anything", 5); len(results) != 0 {
		t.Errorf("Search on an empty index = %v, want none", results)
	}
	var zero Index
	if results := zero.Search("anything", 5); len(results) != 0 {
		t.Errorf("zero-value Index Search = %v, want none", results)
	}
}

func TestSearchLimit(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "a", Name: "a", Text: "shared alpha"},
		{ID: "b", Name: "b", Text: "shared beta"},
		{ID: "c", Name: "c", Text: "shared gamma"},
	})
	if got := len(index.Search("shared", 2)); got != 2 {
		t.Errorf("Search(limit 2) returned %d results, want 2", got)
	}
	for _, limit := range []int{0, -1, 99} {
		if got := len(index.Search("shared", limit)); got != 3 {
			t.Errorf("Search(limit %d) returned %d results, want 3", limit, got)
		}
	}
}

func TestSearchTieKeepsInputOrder(t *testing.T) {
	// Equal scores keep the caller's document order: a caller that
	// wants a name tie-break sorts its documents by name first.
	index := mustIndex(t, []Doc{
		{ID: "b", Name: "search", Text: ""},
		{ID: "a", Name: "search", Text: ""},
	})
	if ids := resultIDs(index.Search("search", 10)); !slices.Equal(ids, []string{"b", "a"}) {
		t.Fatalf("Search(search) = %v, want [b a]", ids)
	}
	sorted := mustIndex(t, []Doc{
		{ID: "a", Name: "search", Text: ""},
		{ID: "b", Name: "search", Text: ""},
	})
	if ids := resultIDs(sorted.Search("search", 10)); !slices.Equal(ids, []string{"a", "b"}) {
		t.Fatalf("Search(search) on sorted docs = %v, want [a b]", ids)
	}
}

func TestDocFreqCountsDocumentOnceAcrossFields(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "a", Name: "alpha", Text: "alpha beta"},
		{ID: "b", Name: "beta", Text: "gamma"},
	})
	// "alpha" appears in a's name and a's text: still one document.
	// Summing the per-field postings would count it twice.
	if got := index.docFreq("alpha"); got != 1 {
		t.Errorf("docFreq(alpha) = %d, want 1 (union across fields)", got)
	}
	// "beta" appears in a's text and b's name: two documents.
	if got := index.docFreq("beta"); got != 2 {
		t.Errorf("docFreq(beta) = %d, want 2", got)
	}
}

func TestFieldLengthCountsAllTokens(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "a", Text: "alpha beta"},
		{ID: "b", Text: "alpha alpha beta"},
	})
	// |D| is the total token count, repetitions included.
	if got := index.fields[textField].lengths; !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("text field lengths = %v, want [2 3]", got)
	}
}

func TestWithBZeroDisablesLengthNormalisation(t *testing.T) {
	docs := []Doc{
		{ID: "short", Text: "alpha"},
		{ID: "long", Text: "alpha beta gamma delta"},
	}

	normalised := resultIDs(mustIndex(t, docs).Search("alpha", 10))
	if !slices.Equal(normalised, []string{"short", "long"}) {
		t.Fatalf("default ranking = %v, want [short long]", normalised)
	}

	flat := mustIndex(t, docs, WithB(0)).Search("alpha", 10)
	if len(flat) != 2 {
		t.Fatalf("WithB(0) ranking = %v, want both documents", flat)
	}
	// With b=0 the length ratio drops out: equal tf scores equally.
	if math.Abs(flat[0].Score-flat[1].Score) > 1e-12 {
		t.Fatalf("WithB(0) scores differ: %v vs %v", flat[0].Score, flat[1].Score)
	}
}

func TestNewFromTermsMatchesNew(t *testing.T) {
	docs := []Doc{
		{ID: "resume", Name: "resume", Text: "pick and resume a past conversation"},
		{ID: "permissions", Name: "permissions", Text: "switch the sandbox permission mode"},
		{ID: "clear", Name: "clear", Text: "清理当前会话历史"},
	}
	termDocs := make([]TermDoc, 0, len(docs))
	for _, doc := range docs {
		termDocs = append(termDocs, TermDoc{
			ID:   doc.ID,
			Name: tokenCounts(doc.Name),
			Text: tokenCounts(doc.Text),
		})
	}
	fromText := mustIndex(t, docs)
	fromTerms, err := NewFromTerms(termDocs)
	if err != nil {
		t.Fatalf("NewFromTerms: %v", err)
	}
	for _, query := range []string{"resume", "sandbox", "permission", "清理", "conversa", "zzzz"} {
		want := fromText.Search(query, 0)
		got := fromTerms.Search(query, 0)
		if len(got) != len(want) {
			t.Fatalf("Search(%q) = %v, want %v", query, got, want)
		}
		for i := range got {
			if got[i].ID != want[i].ID {
				t.Fatalf("Search(%q)[%d].ID = %q, want %q", query, i, got[i].ID, want[i].ID)
			}
			if math.Abs(got[i].Score-want[i].Score) > 1e-12 {
				t.Fatalf("Search(%q)[%d].Score = %v, want %v", query, i, got[i].Score, want[i].Score)
			}
		}
	}
}

func tokenCounts(text string) map[string]int {
	tokens := Tokenize(text)
	counts := make(map[string]int, len(tokens))
	for _, token := range tokens {
		counts[token]++
	}
	return counts
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	docs := []Doc{{ID: "a", Name: "alpha"}}
	cases := []struct {
		name string
		opt  Option
	}{
		{"k1 zero", WithK1(0)},
		{"k1 negative", WithK1(-1)},
		{"k1 NaN", WithK1(math.NaN())},
		{"k1 infinite", WithK1(math.Inf(1))},
		{"b below range", WithB(-0.1)},
		{"b above range", WithB(1.1)},
		{"b NaN", WithB(math.NaN())},
		{"b infinite", WithB(math.Inf(-1))},
		{"prefix weight negative", WithPrefixWeight(-0.1)},
		{"prefix weight above one", WithPrefixWeight(1.1)},
		{"prefix weight NaN", WithPrefixWeight(math.NaN())},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(docs, c.opt); err == nil {
				t.Fatal("New accepted an invalid option, want error")
			}
			if _, err := NewFromTerms([]TermDoc{{ID: "a"}}, c.opt); err == nil {
				t.Fatal("NewFromTerms accepted an invalid option, want error")
			}
		})
	}
	if _, err := New(docs, WithK1(0.1), WithB(0), WithB(1), WithPrefixWeight(0), WithPrefixWeight(1)); err != nil {
		t.Fatalf("New with boundary options: %v", err)
	}
	// A nil option is ignored rather than applied, so a caller can pass an
	// optional one through without checking it first.
	if _, err := New(docs, nil, WithK1(1.4)); err != nil {
		t.Fatalf("New with a nil option: %v", err)
	}
	if _, err := NewFromTerms([]TermDoc{{ID: "a"}}, nil); err != nil {
		t.Fatalf("NewFromTerms with a nil option: %v", err)
	}
}

// TestNewRejectsDuplicateIDs pins the uniqueness precondition both callers
// rely on: core/tool maps a hit back to its definition by name, and the
// memory lane keys its documents by ID, so a duplicate would silently pick
// one of the two.
func TestNewRejectsDuplicateIDs(t *testing.T) {
	if _, err := New([]Doc{{ID: "a", Name: "alpha"}, {ID: "a", Name: "beta"}}); err == nil {
		t.Fatal("New accepted duplicate ids, want error")
	}
	if _, err := NewFromTerms([]TermDoc{{ID: "a"}, {ID: "a"}}); err == nil {
		t.Fatal("NewFromTerms accepted duplicate ids, want error")
	}
}

func TestNewFromTermsRejectsInvalidFrequencies(t *testing.T) {
	cases := []struct {
		name string
		docs []TermDoc
	}{
		{"zero frequency", []TermDoc{{ID: "a", Text: map[string]int{"alpha": 0}}}},
		{"negative frequency", []TermDoc{{ID: "a", Text: map[string]int{"alpha": -1}}}},
		{"empty term", []TermDoc{{ID: "a", Text: map[string]int{"": 1}}}},
		{"oversized frequency", []TermDoc{{ID: "a", Text: map[string]int{"alpha": int(math.MaxInt32) + 1}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewFromTerms(c.docs); err == nil {
				t.Fatal("NewFromTerms accepted invalid terms, want error")
			}
		})
	}
}

// TestIndexStaysFlat guards the postings representation. One Go map per
// distinct term — the shape the flat term table replaced — costs ~200
// bytes before it holds anything plus one live object each; on this
// corpus (500 documents, ~900 distinct terms, ~15k postings) that
// layout measured 5,611 live objects against ~500 for the flat one, so
// the bound below fails loudly if per-term maps come back.
func TestIndexStaysFlat(t *testing.T) {
	docs := corpusDocs(500)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	index := mustIndex(t, docs)
	runtime.GC()
	runtime.ReadMemStats(&after)

	objects := int64(after.HeapObjects) - int64(before.HeapObjects)
	t.Logf("index holds %d live objects", objects)
	if objects > 3000 {
		t.Errorf("index holds %d live objects, want <= 3000: "+
			"the postings went back to one map per term", objects)
	}
	if results := index.Search("w42", 5); len(results) == 0 {
		t.Errorf("Search(w42) = %v, want hits", results)
	}
	runtime.KeepAlive(index)
}

// TestTablesAreSizedFromTheirOwnCounts guards the flat tables' capacity:
// each is sized from its own count, never from the posting count. Postings
// per term average well above one, so a term table sized from postings
// retains mostly empty entries — on a 100k-document corpus (4M postings,
// 30k terms) that was 95MB of a 131MB index. This corpus has ~16k
// postings and ~1k terms, so the same regression still shows as a table
// sized for the postings. The bound is a ratio rather than an exact
// capacity: the point is that the table is not sized from something an
// order of magnitude larger, and a table grown by append is just as fine.
func TestTablesAreSizedFromTheirOwnCounts(t *testing.T) {
	index := mustIndex(t, corpusDocs(500))
	for field, name := range []string{"name", "text"} {
		f := &index.fields[field]
		if len(f.terms) == 0 || len(f.postings) == 0 {
			t.Fatalf("%s field: empty tables (terms=%d, postings=%d)", name, len(f.terms), len(f.postings))
		}
		if cap(f.terms) > 4*len(f.terms) {
			t.Errorf("%s field: term table capacity %d for %d terms: the table is sized from postings",
				name, cap(f.terms), len(f.terms))
		}
		if cap(f.postings) > 4*len(f.postings) {
			t.Errorf("%s field: postings capacity %d for %d postings",
				name, cap(f.postings), len(f.postings))
		}
	}
}

func TestSearchIsConcurrencySafe(t *testing.T) {
	docs := make([]Doc, 0, 200)
	for i := 0; i < 200; i++ {
		docs = append(docs, Doc{
			ID:   fmt.Sprintf("tool-%d", i),
			Name: fmt.Sprintf("tool_%d", i),
			Text: fmt.Sprintf("capability number %d for the catalog", i),
		})
	}
	index := mustIndex(t, docs)
	want := resultIDs(index.Search("capability", 10))
	if len(want) != 10 {
		t.Fatalf("setup: Search(capability) = %v", want)
	}

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				got := resultIDs(index.Search("capability", 10))
				if !slices.Equal(got, want) {
					t.Errorf("concurrent Search = %v, want %v", got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func FuzzSearch(f *testing.F) {
	for _, seed := range []string{"", "search", "清理", "w42", "!!!", "web_fetch", "res"} {
		f.Add(seed)
	}
	index, err := New([]Doc{
		{ID: "resume", Name: "resume", Text: "pick and resume a past conversation"},
		{ID: "clear", Name: "clear", Text: "清理当前会话历史"},
	})
	if err != nil {
		f.Fatalf("New: %v", err)
	}
	f.Fuzz(func(t *testing.T, query string) {
		results := index.Search(query, 3)
		if len(results) > 3 {
			t.Fatalf("Search(%q) returned %d results for limit 3", query, len(results))
		}
		for _, result := range results {
			if result.ID != "resume" && result.ID != "clear" {
				t.Fatalf("Search(%q) returned unknown document %q", query, result.ID)
			}
		}
	})
}

// corpusDocs builds a deterministic corpus of n documents with ~900
// distinct terms, used by the flatness guard and the benchmarks.
func corpusDocs(n int) []Doc {
	docs := make([]Doc, 0, n)
	for i := 0; i < n; i++ {
		var text strings.Builder
		for j := 0; j < 30; j++ {
			fmt.Fprintf(&text, "w%d ", (i*7+j*13)%900)
		}
		id := fmt.Sprintf("skill-%d", i)
		docs = append(docs, Doc{ID: id, Name: id, Text: text.String()})
	}
	return docs
}

// TestSearchTermsMatchesSearch pins the contract a caller with its
// own tokenizer relies on: scoring an already-tokenized query is the
// same as scoring the text it came from.
func TestSearchTermsMatchesSearch(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "a", Text: "resume a past conversation"},
		{ID: "b", Text: "清理当前会话历史"},
		{ID: "c", Text: "search old notes by keyword"},
	})
	for _, query := range []string{"resume", "清理", "会话 历史", "search keyword", "none"} {
		want := index.Search(query, 0)
		got := index.SearchTerms(Tokenize(query), 0)
		if len(got) != len(want) {
			t.Fatalf("SearchTerms(Tokenize(%q)) = %d results, Search = %d", query, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("SearchTerms(Tokenize(%q))[%d] = %+v, Search = %+v", query, i, got[i], want[i])
			}
		}
	}
}

func TestSearchTermsDeduplicatesAndSkipsEmpty(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "a", Text: "resume resume past conversation"},
		{ID: "b", Text: "unrelated wording"},
	})

	// Repeating a term cannot raise its score: the IDF contribution is
	// counted once, exactly as Search's tokenizer does it.
	repeated := index.SearchTerms([]string{"resume", "resume", "", "resume"}, 0)
	once := index.SearchTerms([]string{"resume"}, 0)
	if len(repeated) != 1 || len(once) != 1 || repeated[0] != once[0] {
		t.Fatalf("repeated = %+v, once = %+v", repeated, once)
	}
	if got := index.SearchTerms([]string{"", ""}, 0); got != nil {
		t.Fatalf("SearchTerms(empty terms) = %+v, want nil", got)
	}
}

// TestPrefixExpansionSaturatesPerQueryTerm pins the rule that a query term
// contributes its best match rather than the sum over its expansions. "con"
// is an exact term in one document and the prefix of four terms in the
// other, so summing the expansions at half weight each would put the
// prefix document first.
func TestPrefixExpansionSaturatesPerQueryTerm(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "exact", Name: "con"},
		{ID: "prefixy", Name: "config content connection context"},
	})
	results := index.Search("con", 10)
	if len(results) != 2 || results[0].ID != "exact" {
		t.Fatalf("Search(con) = %+v, want the exact match first", results)
	}
	if results[0].Score <= results[1].Score {
		t.Fatalf("Search(con) = %+v, want a strict win for the exact match", results)
	}
}

// TestScoresMatchTheHandComputedContract freezes the numeric side of the
// ranking contract. The other tests assert relative properties — order,
// counts, limits — which the magic numbers could all keep while moving:
// nameBoost, the default prefix discount, the idf smoothing and the (k1+1)
// saturation read as absolute scores, and the fusion lane calibrates its
// query-length buckets against raw BM25 magnitudes, so a drift would
// re-rank every retrieval lane with the suite still green.
//
// The fixture: alpha sits in d1 and d2 (name) and d4 (text), so its
// document frequency is the union 3. The name field averages 1.5 tokens
// (1+2+2+1 over four documents) and the text field 0.25.
func TestScoresMatchTheHandComputedContract(t *testing.T) {
	index := mustIndex(t, []Doc{
		{ID: "d1", Name: "alpha"},
		{ID: "d2", Name: "alpha beta"},
		{ID: "d3", Name: "beta gamma"},
		{ID: "d4", Name: "delta", Text: "alpha"},
	})
	cases := []struct {
		query string
		want  []Result
	}{
		{
			query: "alpha",
			want: []Result{
				{ID: "d1", Score: 1.2389761210503338},
				{ID: "d2", Score: 0.9416218519982535},
				{ID: "d4", Score: 0.1601397707480023},
			},
		},
		{
			// The same three documents at exactly half of the previous
			// scores: a term that only prefixes an index term is discounted
			// by the default 0.5.
			query: "alph",
			want: []Result{
				{ID: "d1", Score: 0.6194880605251669},
				{ID: "d2", Score: 0.4708109259991268},
				{ID: "d4", Score: 0.08006988537400116},
			},
		},
		{
			// Equal scores keep document order, here d2 before d3.
			query: "beta",
			want: []Result{
				{ID: "d2", Score: 1.8299085566782556},
				{ID: "d3", Score: 1.8299085566782556},
			},
		},
		{
			// Two query terms on one document: a name hit plus a text hit
			// add up (d4, 4.182221320290094 + 0.1601397707480023), and a
			// name hit outranks a text hit for the same term (d1 1.2389
			// against d4 0.1601) — the field weight is 3x, and the name
			// field's length normalisation is gentler here as well.
			query: "delta alpha",
			want: []Result{
				{ID: "d4", Score: 4.3423610910380965},
				{ID: "d1", Score: 1.2389761210503338},
				{ID: "d2", Score: 0.9416218519982535},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			got := index.Search(c.query, 0)
			if len(got) != len(c.want) {
				t.Fatalf("Search(%q) = %+v, want %+v", c.query, got, c.want)
			}
			for i := range c.want {
				if got[i].ID != c.want[i].ID || math.Abs(got[i].Score-c.want[i].Score) > 1e-12 {
					t.Fatalf("Search(%q)[%d] = %+v, want %+v", c.query, i, got[i], c.want[i])
				}
			}
		})
	}
}
