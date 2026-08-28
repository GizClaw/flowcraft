package resource

import (
	"context"
	"strings"
	"testing"
)

type scalarSettings struct {
	Capacity *Int     `json:"capacity,omitempty"`
	Resume   *Bool    `json:"resume,omitempty"`
	Ratio    *Float64 `json:"ratio,omitempty"`
	Millis   *Int64   `json:"millis,omitempty"`
	Plain    Int      `json:"plain,omitempty"`
}

func TestDecodeSettingsScalarTypes(t *testing.T) {
	t.Run("literals decode unchanged", func(t *testing.T) {
		var s scalarSettings
		err := DecodeSettings(context.Background(), &s, []byte(`{
			"capacity": 8,
			"resume": true,
			"ratio": 1.5,
			"millis": 3000,
			"plain": 3
		}`))
		if err != nil {
			t.Fatalf("DecodeSettings: %v", err)
		}
		if s.Capacity == nil || *s.Capacity != 8 {
			t.Fatalf("Capacity = %v, want 8", s.Capacity)
		}
		if s.Resume == nil || !bool(*s.Resume) {
			t.Fatalf("Resume = %v, want true", s.Resume)
		}
		if s.Ratio == nil || float64(*s.Ratio) != 1.5 {
			t.Fatalf("Ratio = %v, want 1.5", s.Ratio)
		}
		if s.Millis == nil || *s.Millis != 3000 {
			t.Fatalf("Millis = %v, want 3000", s.Millis)
		}
		if s.Plain != 3 {
			t.Fatalf("Plain = %v, want 3", s.Plain)
		}
	})

	t.Run("env strings decode through expansion", func(t *testing.T) {
		lookup := map[string]string{
			"FC_CAPACITY": "8",
			"FC_RESUME":   "1",
			"FC_RATIO":    "0.25",
			"FC_MILLIS":   "3000",
		}
		var s scalarSettings
		err := DecodeSettings(context.Background(), &s, []byte(`{
			"capacity": "${env:FC_CAPACITY}",
			"resume": "${env:FC_RESUME}",
			"ratio": "${env:FC_RATIO}",
			"millis": "${env:FC_MILLIS}"
		}`), WithEnv(func(name string) (string, bool) {
			v, ok := lookup[name]
			return v, ok
		}))
		if err != nil {
			t.Fatalf("DecodeSettings: %v", err)
		}
		if s.Capacity == nil || *s.Capacity != 8 {
			t.Fatalf("Capacity = %v, want 8", s.Capacity)
		}
		if s.Resume == nil || !bool(*s.Resume) {
			t.Fatalf("Resume = %v, want true", s.Resume)
		}
		if s.Ratio == nil || float64(*s.Ratio) != 0.25 {
			t.Fatalf("Ratio = %v, want 0.25", s.Ratio)
		}
		if s.Millis == nil || *s.Millis != 3000 {
			t.Fatalf("Millis = %v, want 3000", s.Millis)
		}
	})

	t.Run("absent stays nil", func(t *testing.T) {
		var s scalarSettings
		if err := DecodeSettings(context.Background(), &s, []byte(`{"plain": 1}`)); err != nil {
			t.Fatalf("DecodeSettings: %v", err)
		}
		if s.Capacity != nil || s.Resume != nil || s.Ratio != nil ||
			s.Millis != nil {
			t.Fatalf("absent pointer fields must stay nil: %+v", s)
		}
	})

	t.Run("invalid strings are validation errors", func(t *testing.T) {
		for name, raw := range map[string]string{
			"int":   `{"capacity": "abc"}`,
			"bool":  `{"resume": "maybe"}`,
			"float": `{"ratio": "fast"}`,
			"int64": `{"millis": "soon"}`,
		} {
			t.Run(name, func(t *testing.T) {
				var s scalarSettings
				err := DecodeSettings(context.Background(), &s, []byte(raw))
				if err == nil {
					t.Fatal("DecodeSettings unexpectedly succeeded")
				}
				if !strings.Contains(err.Error(), "resource settings:") {
					t.Fatalf("error = %v, want resource settings prefix", err)
				}
			})
		}
	})

	t.Run("missing env still fails before decode", func(t *testing.T) {
		var s scalarSettings
		err := DecodeSettings(context.Background(), &s, []byte(`{"capacity": "${env:FC_UNSET}"}`),
			WithEnv(func(string) (string, bool) { return "", false }))
		if err == nil {
			t.Fatal("DecodeSettings unexpectedly succeeded")
		}
		if !strings.Contains(err.Error(), "not set") {
			t.Fatalf("error = %v, want missing env mention", err)
		}
	})

	t.Run("strict unknown fields still rejected", func(t *testing.T) {
		var s scalarSettings
		err := DecodeSettings(context.Background(), &s, []byte(`{"capacity": 8, "typo": true}`))
		if err == nil {
			t.Fatal("DecodeSettings unexpectedly succeeded")
		}
		if !strings.Contains(err.Error(), "typo") {
			t.Fatalf("error = %v, want unknown field mention", err)
		}
	})
}
