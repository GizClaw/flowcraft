package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The contract is only as good as its tables: one that lost a row would still
// be a valid document. These tests hold the document to what it claims to be,
// and hold it to being the same on every run -- it is committed and compared
// byte for byte, so an unordered map leaking into the output would show up as
// a flaky gate rather than a diff.

func generate(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeErrorContract(&buf); err != nil {
		t.Fatalf("writeErrorContract: %v", err)
	}
	return buf.Bytes()
}

func decode(t *testing.T) errorContract {
	t.Helper()
	var doc errorContract
	if err := json.Unmarshal(generate(t), &doc); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return doc
}

func TestErrorContractIsReproducible(t *testing.T) {
	first, second := generate(t), generate(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two generations differ; the document must be a function of core alone")
	}
}

// TestErrorContractCoversEveryKind checks the table's rows against the same
// errdefs table they are dumped from, one kind at a time, so a kind that lost
// its row fails here rather than at the next regeneration of the file.
func TestErrorContractCoversEveryKind(t *testing.T) {
	doc := decode(t)
	if len(doc.Kinds) != len(kindSpecs) {
		t.Fatalf("kinds: %d rows for %d kinds", len(doc.Kinds), len(kindSpecs))
	}
	seen := map[string]bool{}
	for _, row := range doc.Kinds {
		if seen[row.Name] {
			t.Fatalf("kind %q appears twice", row.Name)
		}
		seen[row.Name] = true
		spec, err := kindSpecOf(row.Name)
		if err != nil {
			t.Fatalf("row %q: %v", row.Name, err)
		}
		// The row must be the status HTTPStatus answers, not a restatement.
		want := errdefs.HTTPStatus(spec.wrap(errors.New("probe")))
		if row.HTTPStatus != want {
			t.Errorf("kind %q: dumped %d, core answers %d", row.Name, row.HTTPStatus, want)
		}
	}
	for _, spec := range kindSpecs {
		if !seen[spec.name] {
			t.Errorf("kind %q has no row", spec.name)
		}
	}
}

// TestErrorContractCoversEveryCategory checks that each category is reachable
// from the samples and named exactly once: the six rows of the code table are
// where five of them come from, and the keyword scan is the only source of
// context_overflow.
func TestErrorContractCoversEveryCategory(t *testing.T) {
	doc := decode(t)
	seen := map[string]int{}
	for _, row := range doc.ProviderCategoryFromHTTPCode {
		seen[row.Category]++
	}
	for _, row := range doc.ClassifyProvider {
		seen[row.Category]++
	}
	for _, spec := range categorySpecs {
		if seen[spec.name] == 0 {
			t.Errorf("category %q is not exercised by any sample", spec.name)
		}
	}
	if len(seen) != len(categorySpecs) {
		t.Errorf("%d category names in the document, %d in core", len(seen), len(categorySpecs))
	}
}

// TestErrorContractScansBothPaths makes sure the classify_provider samples
// still cover both routing paths and the default: with a table that only ever
// hits keywords, a broken embedded-code scan would pass.
func TestErrorContractScansBothPaths(t *testing.T) {
	doc := decode(t)
	codes, keywords, transient := 0, 0, 0
	joined := false
	for _, row := range doc.ClassifyProvider {
		switch row.Category {
		case "transient":
			transient++
		case "context_overflow", "auth", "billing", "rate_limit":
			keywords++
		case "permanent":
			codes++
		}
		if len(row.Chain) > 1 {
			joined = true
		}
	}
	for what, count := range map[string]int{"code path": codes, "default path": transient} {
		if count == 0 {
			t.Errorf("no sample takes the %s", what)
		}
	}
	if !joined {
		t.Error("no sample joins two chain elements, so the separator is untested")
	}
}

func TestErrorContractLabelsAreUnique(t *testing.T) {
	doc := decode(t)
	sections := map[string][]string{
		"http_status_priority":    {},
		"classify_status":         {},
		"classify_http_status":    {},
		"classify_provider_error": {},
		"classify_provider":       {},
	}
	for _, row := range doc.HTTPStatusPriority {
		sections["http_status_priority"] = append(sections["http_status_priority"], row.Label)
	}
	for _, row := range doc.ClassifyStatus {
		sections["classify_status"] = append(sections["classify_status"], row.Label)
	}
	for _, row := range doc.ClassifyHTTPStatus {
		sections["classify_http_status"] = append(sections["classify_http_status"], row.Label)
	}
	for _, row := range doc.ClassifyProviderError {
		sections["classify_provider_error"] = append(sections["classify_provider_error"], row.Label)
	}
	for _, row := range doc.ClassifyProvider {
		sections["classify_provider"] = append(sections["classify_provider"], row.Label)
	}
	for name, labels := range sections {
		if len(labels) == 0 {
			t.Errorf("%s: no rows", name)
			continue
		}
		seen := map[string]bool{}
		for _, label := range labels {
			if seen[label] {
				t.Errorf("%s: label %q appears twice; a diff would not say which row moved", name, label)
			}
			seen[label] = true
		}
	}
}

// TestErrorContractNamesAreKnown keeps the document's vocabulary closed: every
// kind and category name it uses has to be one the tables above define.
func TestErrorContractNamesAreKnown(t *testing.T) {
	doc := decode(t)
	knownKind := func(name string) error {
		if name == "" {
			return fmt.Errorf("no kind")
		}
		_, err := kindSpecOf(name)
		return err
	}
	for _, row := range doc.HTTPStatusPriority {
		for _, name := range row.Chain {
			if _, err := kindSpecOf(name); err != nil {
				t.Errorf("http_status_priority %q: %v", row.Label, err)
			}
		}
	}
	for _, row := range doc.ClassifyStatus {
		if err := knownKind(row.Kind); err != nil {
			t.Errorf("classify_status %q: %v", row.Label, err)
		}
	}
	for _, row := range doc.ClassifyHTTPStatus {
		if err := knownKind(row.Kind); err != nil {
			t.Errorf("classify_http_status %q: %v", row.Label, err)
		}
	}
	for _, row := range doc.ClassifyProviderError {
		if err := knownKind(row.Kind); err != nil {
			t.Errorf("classify_provider_error %q: %v", row.Label, err)
		}
		if row.Inner.Kind != nil {
			if _, err := kindSpecOf(*row.Inner.Kind); err != nil {
				t.Errorf("classify_provider_error %q: %v", row.Label, err)
			}
		}
	}
	for _, row := range doc.ClassifyProvider {
		if _, err := categorySpecOf(row.Category); err != nil {
			t.Errorf("classify_provider %q: %v", row.Label, err)
		}
	}
	for _, row := range doc.ProviderCategoryFromHTTPCode {
		if _, err := categorySpecOf(row.Category); err != nil {
			t.Errorf("provider_category_from_http_code %d: %v", row.Code, err)
		}
	}
}
