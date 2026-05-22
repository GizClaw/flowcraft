package domain

import "testing"

func TestFactOrigin_IsZero(t *testing.T) {
	if !(FactOrigin{}).IsZero() {
		t.Error("zero value FactOrigin must be IsZero")
	}
	if (FactOrigin{RequestID: "r"}).IsZero() {
		t.Error("RequestID set → not zero")
	}
	if (FactOrigin{Kind: OriginKindEpisode}).IsZero() {
		t.Error("Kind set → not zero")
	}
	if (FactOrigin{EpisodeFactIDs: []string{"e1"}}).IsZero() {
		t.Error("EpisodeFactIDs set → not zero")
	}
	if (FactOrigin{
		RequestID:      "r",
		Kind:           OriginKindSemanticDerivation,
		EpisodeFactIDs: []string{"e1"},
	}).IsZero() {
		t.Error("populated FactOrigin must not be IsZero")
	}
}
