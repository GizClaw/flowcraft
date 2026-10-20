package main

import (
	"encoding/json"
	"io"

	"github.com/GizClaw/flowcraft/core/utils/bm25"
)

// The bm25 contract: the tokenizer, the script classification behind it, the
// scoring, and every refusal the constructor can make.
//
// Same rule as the other halves -- every row is a call into core with the
// outcome computed, and the port replays it. The two things a port can get
// wrong without noticing are exactly what this file pins:
//
//   - the tokenizer's Unicode decisions. core asks the tables whether a rune is
//     Han/Hiragana/Katakana/Hangul and whether it is a letter or a digit; a port
//     that spells the scripts as ranges has to prove the ranges are the same
//     set. The `scripts` rows do that by tokenizing the character twice over:
//     two adjacent CJK characters become three tokens (a bigram and two
//     singles), two adjacent letters become one word.
//   - the score's arithmetic. The dump prints each score to twelve significant
//     digits: enough to pin the formula, the k1/b weighting and the idf, and
//     short of the last bit, which is not core's to fix. Go's `math.Log` is a
//     per-architecture routine (amd64 carries its own assembly), so the same
//     corpus scores one ULP apart on arm64 and amd64; a bit pattern here would
//     fail the gate on whichever host did not write the file.
//
// Which documents are indexed, what the query is, and the options, all travel
// with the row: the port builds the input rather than reading core's answers.

const bm25ContractNote = "core/utils/bm25: the tokenizer's script decisions, the scoring " +
	"and the refusals. Rows carry their input; the port replays them."

type bm25Contract struct {
	Note         string         `json:"note"`
	Tokenize     []tokenizeRow  `json:"tokenize"`
	Scripts      []scriptRow    `json:"scripts"`
	Searches     []searchRow    `json:"searches"`
	TermSearches []searchRow    `json:"term_searches"`
	Configs      []configRow    `json:"configs"`
	Frequencies  []frequencyRow `json:"frequencies"`
}

type tokenizeRow struct {
	Input  string   `json:"input"`
	Tokens []string `json:"tokens"`
}

// scriptRow is one code point, tokenized alone and doubled. Doubling is what
// separates the two classifications: `读读` is three tokens because each
// character is CJK, `aa` is one because it is a letter.
type scriptRow struct {
	Code   int      `json:"code"`
	Single []string `json:"single"`
	Double []string `json:"double"`
}

type docRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
}

type hitRow struct {
	ID    string `json:"id"`
	Score string `json:"score"`
}

// searchRow is one call into Search (or SearchTerms, in term_searches).
type searchRow struct {
	Docs         []docRow `json:"docs"`
	Query        string   `json:"query,omitempty"`
	Terms        []string `json:"terms,omitempty"`
	K1           float64  `json:"k1"`
	B            float64  `json:"b"`
	PrefixWeight float64  `json:"prefix_weight"`
	Limit        int      `json:"limit"`
	Results      []hitRow `json:"results,omitempty"`
	Error        string   `json:"error,omitempty"`
}

type configRow struct {
	K1           float64 `json:"k1"`
	B            float64 `json:"b"`
	PrefixWeight float64 `json:"prefix_weight"`
	Error        string  `json:"error,omitempty"`
}

// frequencyRow is one NewFromTerms call: a document whose fields are already
// tokenized, so the frequency checks are reachable.
type frequencyRow struct {
	ID      string         `json:"id"`
	Name    map[string]int `json:"name,omitempty"`
	Text    map[string]int `json:"text,omitempty"`
	Error   string         `json:"error,omitempty"`
	Results []hitRow       `json:"results,omitempty"`
	Query   string         `json:"query,omitempty"`
}

func writeBM25Contract(w io.Writer) error {
	contract := bm25Contract{
		Note:         bm25ContractNote,
		Tokenize:     make([]tokenizeRow, 0, len(bm25TokenizeSamples)),
		Scripts:      make([]scriptRow, 0, len(bm25ScriptCodes)),
		Searches:     make([]searchRow, 0, len(bm25SearchSamples)),
		TermSearches: make([]searchRow, 0, len(bm25TermSearchSamples)),
		Configs:      make([]configRow, 0, len(bm25ConfigSamples)),
		Frequencies:  make([]frequencyRow, 0, len(bm25FrequencySamples)),
	}

	for _, sample := range bm25TokenizeSamples {
		contract.Tokenize = append(contract.Tokenize, tokenizeRow{
			Input:  sample,
			Tokens: tokensOrEmpty(bm25.Tokenize(sample)),
		})
	}
	for _, code := range bm25ScriptCodes {
		character := string(rune(code))
		contract.Scripts = append(contract.Scripts, scriptRow{
			Code:   code,
			Single: tokensOrEmpty(bm25.Tokenize(character)),
			Double: tokensOrEmpty(bm25.Tokenize(character + character)),
		})
	}
	for _, sample := range bm25SearchSamples {
		contract.Searches = append(contract.Searches, runSearch(sample))
	}
	for _, sample := range bm25TermSearchSamples {
		contract.TermSearches = append(contract.TermSearches, runSearch(sample))
	}
	for _, sample := range bm25ConfigSamples {
		row := configRow{
			K1:           sample.k1,
			B:            sample.b,
			PrefixWeight: sample.prefixWeight,
		}
		_, err := bm25.New(
			[]bm25.Doc{{ID: "x", Name: "a", Text: "b"}},
			bm25.WithK1(sample.k1),
			bm25.WithB(sample.b),
			bm25.WithPrefixWeight(sample.prefixWeight),
		)
		if err != nil {
			row.Error = err.Error()
		}
		contract.Configs = append(contract.Configs, row)
	}
	for _, sample := range bm25FrequencySamples {
		row := frequencyRow{ID: sample.id, Name: sample.name, Text: sample.text, Query: sample.query}
		index, err := bm25.NewFromTerms([]bm25.TermDoc{{
			ID: sample.id, Name: sample.name, Text: sample.text,
		}})
		if err != nil {
			row.Error = err.Error()
		} else {
			row.Results = hits(index.Search(sample.query, 0))
		}
		contract.Frequencies = append(contract.Frequencies, row)
	}

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(contract)
}

func runSearch(sample bm25SearchSample) searchRow {
	row := searchRow{
		Docs:         make([]docRow, 0, len(sample.docs)),
		Query:        sample.query,
		Terms:        sample.terms,
		K1:           sample.k1,
		B:            sample.b,
		PrefixWeight: sample.prefixWeight,
		Limit:        sample.limit,
	}
	docs := make([]bm25.Doc, 0, len(sample.docs))
	for _, doc := range sample.docs {
		docs = append(docs, bm25.Doc{ID: doc.id, Name: doc.name, Text: doc.text})
		row.Docs = append(row.Docs, docRow{ID: doc.id, Name: doc.name, Text: doc.text})
	}
	index, err := bm25.New(docs,
		bm25.WithK1(sample.k1),
		bm25.WithB(sample.b),
		bm25.WithPrefixWeight(sample.prefixWeight),
	)
	if err != nil {
		// A refused corpus (a duplicate id) is reported where the port reports
		// it too: the constructor.
		row.Error = err.Error()
		return row
	}
	if sample.terms != nil {
		row.Results = hits(index.SearchTerms(sample.terms, sample.limit))
	} else {
		row.Results = hits(index.Search(sample.query, sample.limit))
	}
	return row
}

func hits(results []bm25.Result) []hitRow {
	if len(results) == 0 {
		return nil
	}
	rows := make([]hitRow, 0, len(results))
	for _, result := range results {
		rows = append(rows, hitRow{
			ID:    result.ID,
			Score: pinnedScore(result.Score),
		})
	}
	return rows
}

func tokensOrEmpty(tokens []string) []string {
	if tokens == nil {
		return []string{}
	}
	return tokens
}
