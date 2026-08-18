package bytedance

import (
	"strings"
	"testing"
)

func TestImageOptionsActiveFields(t *testing.T) {
	enabled := true
	options := ImageOptions{
		LayerDecomposition: &enabled,
		Background:         "transparent",
	}
	fields := options.ActiveFields()
	want := map[string]bool{
		"layer_decomposition": false,
		"background":          false,
	}
	for _, field := range fields {
		if _, ok := want[string(field)]; ok {
			want[string(field)] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("ActiveFields missing %q; got %#v", name, fields)
		}
	}
}

func TestImageOptionsValidateSizeToken(t *testing.T) {
	for _, token := range []string{"1k", "1.5k", "2k", "3k", "4k", "adaptive"} {
		options := ImageOptions{SizeToken: token}
		if err := options.Validate(); err != nil {
			t.Errorf("SizeToken %q: Validate() = %v, want nil", token, err)
		}
	}
	if err := (ImageOptions{SizeToken: "8k"}).Validate(); err == nil {
		t.Fatal("SizeToken 8k: Validate() = nil, want error")
	}
}

func TestImageOptionsValidateBackground(t *testing.T) {
	for _, value := range []string{"transparent", "opaque"} {
		if err := (ImageOptions{Background: value}).Validate(); err != nil {
			t.Errorf("Background %q: Validate() = %v, want nil", value, err)
		}
	}
	if err := (ImageOptions{Background: "alpha"}).Validate(); err == nil {
		t.Fatal("Background alpha: Validate() = nil, want error")
	}
}

func TestImageOptionsValidateBackgroundLayerConflict(t *testing.T) {
	enabled := true
	err := (ImageOptions{Background: "transparent", LayerDecomposition: &enabled}).Validate()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Validate() = %v, want background/layer_decomposition conflict", err)
	}
}

func TestImageOptionsCloneDeepCopiesNewFields(t *testing.T) {
	enabled := true
	options := ImageOptions{LayerDecomposition: &enabled}
	cloned, ok := options.Clone().(ImageOptions)
	if !ok {
		t.Fatalf("Clone() type = %T, want ImageOptions", options.Clone())
	}
	if cloned.LayerDecomposition == nil {
		t.Fatal("Clone() lost layer_decomposition")
	}
	*cloned.LayerDecomposition = false
	if !*options.LayerDecomposition {
		t.Fatal("Clone() shares the layer_decomposition pointer")
	}
}
