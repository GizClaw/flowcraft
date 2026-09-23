package eval

import (
	"bytes"
	"testing"
)

func TestFingerprintEqualityCoversEveryField(t *testing.T) {
	base := Fingerprint{
		Revision: "abc123", Dataset: "deadbeef", Questions: 1540, PolicyDigest: "cabe2319",
		Values: map[string]string{"judge_style": "strict", "max_items": "30"},
	}
	if !base.Equal(base.Clone()) {
		t.Fatal("identical fingerprints differ")
	}
	for name, mutated := range map[string]Fingerprint{
		"revision":      {Revision: "abc124", Dataset: base.Dataset, Questions: base.Questions, PolicyDigest: base.PolicyDigest, Values: base.Values},
		"dataset":       {Revision: base.Revision, Dataset: "deadbeee", Questions: base.Questions, PolicyDigest: base.PolicyDigest, Values: base.Values},
		"questions":     {Revision: base.Revision, Dataset: base.Dataset, Questions: 1539, PolicyDigest: base.PolicyDigest, Values: base.Values},
		"policy_digest": {Revision: base.Revision, Dataset: base.Dataset, Questions: base.Questions, PolicyDigest: "bdfee5aa", Values: base.Values},
		"values": {Revision: base.Revision, Dataset: base.Dataset, Questions: base.Questions,
			PolicyDigest: base.PolicyDigest, Values: map[string]string{"judge_style": "locomo", "max_items": "30"}},
		"extra value": {Revision: base.Revision, Dataset: base.Dataset, Questions: base.Questions,
			PolicyDigest: base.PolicyDigest, Values: map[string]string{"judge_style": "strict", "max_items": "30", "samples": "10"}},
	} {
		if base.Equal(mutated) {
			t.Fatalf("fingerprints differing in %s compare equal", name)
		}
	}
}

func TestFingerprintRoundTripsThroughBaseline(t *testing.T) {
	fingerprint := Fingerprint{
		Revision: "abc123", Dataset: "deadbeef", Questions: 1540, PolicyDigest: "cabe2319",
		Values: map[string]string{"judge_style": "strict"},
	}
	baseline := NewBaseline("nightly", []Report{{Scenario: "locomo/conv-26", HitRate: 1}})
	baseline.Fingerprint = fingerprint
	var buffer bytes.Buffer
	if err := SaveBaseline(&buffer, baseline); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBaseline(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Fingerprint.Equal(fingerprint) {
		t.Fatalf("fingerprint = %#v, want %#v", loaded.Fingerprint, fingerprint)
	}
	if loaded.Fingerprint.Empty() {
		t.Fatal("a stamped fingerprint reads back as empty")
	}
	if (Fingerprint{}).Empty() != true {
		t.Fatal("zero fingerprint must read as empty")
	}
}

func TestDatasetDigestFollowsTheBytes(t *testing.T) {
	if DatasetDigest([]byte("a")) == DatasetDigest([]byte("b")) {
		t.Fatal("different bytes share a digest")
	}
	if len(DatasetDigest([]byte("a"))) != 12 {
		t.Fatalf("digest = %q", DatasetDigest([]byte("a")))
	}
}
