package main

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/utils/bm25"
)

// The bm25 dump is committed and compared byte for byte, so it has to be a
// function of core alone, and its rows have to stay samples of the calls they
// claim to be. The spot checks at the end are the ones worth writing down
// twice, because they are the properties the port is most likely to get wrong
// quietly: a script range that is one code point too wide, a prefix expansion
// scored twice, a frequency accepted where core refuses it.

func generateBM25(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeBM25Contract(&buffer); err != nil {
		t.Fatalf("writeBM25Contract: %v", err)
	}
	return buffer.Bytes()
}

func decodeBM25(t *testing.T) bm25Contract {
	t.Helper()
	var document bm25Contract
	if err := json.Unmarshal(generateBM25(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

func TestBM25ContractIsReproducible(t *testing.T) {
	first, second := generateBM25(t), generateBM25(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two generations differ; the document must be a function of core alone")
	}
}

func TestBM25ContractHasRows(t *testing.T) {
	document := decodeBM25(t)
	if len(document.Tokenize) < 20 || len(document.Scripts) < 50 {
		t.Fatalf("tokenize %d, scripts %d: too few rows to be a contract",
			len(document.Tokenize), len(document.Scripts))
	}
	if len(document.Searches) == 0 || len(document.Configs) == 0 || len(document.Frequencies) == 0 {
		t.Fatal("a dump without rows is not a contract")
	}
	for _, row := range document.Searches {
		// An empty corpus is a sample of its own -- the index answers nothing --
		// so a row without documents needs to say that is what it meant.
		if len(row.Docs) == 0 && row.Query != "" && len(row.Results) != 0 {
			t.Fatalf("an empty corpus that still matched: %+v", row)
		}
	}
}

// TestBM25ScriptRowsSeparateTheClasses pins the trick the dump relies on: two
// adjacent CJK characters tokenize to three terms (two singles and a bigram),
// two adjacent letters to one. If that ever stops holding, the script rows stop
// measuring anything.
func TestBM25ScriptRowsSeparateTheClasses(t *testing.T) {
	document := decodeBM25(t)
	classified := 0
	for _, row := range document.Scripts {
		character := string(rune(row.Code))
		cjk := len(bm25.Tokenize(character+character)) == 3
		letters := len(bm25.Tokenize(character+character)) == 1
		switch {
		case cjk && len(row.Double) != 3:
			t.Fatalf("%#x: core says CJK, the dump has %d tokens", row.Code, len(row.Double))
		case letters && len(row.Double) != 1:
			t.Fatalf("%#x: core says letter, the dump has %d tokens", row.Code, len(row.Double))
		case !cjk && !letters && len(row.Double) != 0:
			t.Fatalf("%#x: neither CJK nor a letter, the dump has %d tokens", row.Code, len(row.Double))
		default:
			classified++
		}
	}
	if classified != len(document.Scripts) {
		t.Fatalf("only %d of %d rows classified", classified, len(document.Scripts))
	}
}

func TestBM25ExactBeatsPrefix(t *testing.T) {
	index, err := bm25.New([]bm25.Doc{
		{ID: "prefix", Name: "webfetch"},
		{ID: "exact", Name: "web"},
	})
	if err != nil {
		t.Fatal(err)
	}
	results := index.Search("web", 0)
	if len(results) != 2 || results[0].ID != "exact" {
		t.Fatalf("an exact name hit must outrank a prefix hit: %+v", results)
	}
}

func TestBM25ScoresAreFiniteAndPositive(t *testing.T) {
	for _, row := range decodeBM25(t).Searches {
		for _, hit := range row.Results {
			score, err := strconv.ParseFloat(hit.Score, 64)
			if err != nil {
				t.Fatalf("%s: score %q: %v", hit.ID, hit.Score, err)
			}
			if math.IsNaN(score) || math.IsInf(score, 0) || score <= 0 {
				t.Fatalf("%s: score %v is not a positive finite number", hit.ID, score)
			}
			// The dump carries twelve significant digits and no more: the
			// thirteenth is libm's, not core's. See pinnedScore.
			if again := strconv.FormatFloat(score, 'g', 12, 64); again != hit.Score {
				t.Fatalf("%s: score %q is not twelve significant digits of %v (it would print %q)",
					hit.ID, hit.Score, score, again)
			}
		}
	}
}

func TestBM25RefusalsReadLikeCores(t *testing.T) {
	document := decodeBM25(t)
	var messages []string
	for _, row := range document.Configs {
		messages = append(messages, row.Error)
	}
	for _, row := range document.Frequencies {
		messages = append(messages, row.Error)
	}
	for _, row := range document.Searches {
		messages = append(messages, row.Error)
	}
	var checked int
	for _, message := range messages {
		if message == "" {
			continue
		}
		if !strings.HasPrefix(message, "bm25: ") {
			t.Fatalf("a refusal without the package prefix: %q", message)
		}
		checked++
	}
	if checked < 6 {
		t.Fatalf("only %d refusals sampled; the corpus lost its edge cases", checked)
	}
}
