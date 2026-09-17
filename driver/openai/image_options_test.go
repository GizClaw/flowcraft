package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// TestImageOptionsEditsTransport pins the edit-side knobs on the multipart
// images/edits body: the transparency policy, the webp/jpeg compression level
// and the input fidelity all reach the wire as form values alongside the
// uploads.
func TestImageOptionsEditsTransport(t *testing.T) {
	png := testPNG(t)
	server, _ := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		r *http.Request,
		_ map[string]any,
	) {
		if r.URL.Path != "/images/edits" {
			t.Errorf("path = %s, want /images/edits", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		for name, want := range map[string]string{
			"background":         "opaque",
			"output_compression": "80",
			"output_format":      "jpeg",
			"input_fidelity":     "high",
		} {
			values := r.MultipartForm.Value[name]
			if len(values) != 1 || values[0] != want {
				t.Errorf("%s = %v, want [%s]", name, values, want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"data":  []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(png)}},
			"usage": map[string]any{"input_tokens": 12, "output_tokens": 0},
		})
		_, _ = fmt.Fprint(w, string(payload))
	})
	defer server.Close()
	cls := testClients(t, server)

	reference, err := media.NewImageBytes(png, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	compression := 80
	compiled, err := compileImage("gpt-image-2")(
		context.Background(),
		openaiModel("gpt-image-2"),
		inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "make it a red circle"},
						message.ImagePart{Source: reference},
					}},
					Intent: inference.Intent{Image: &inference.ImageIntent{
						OutputFormat: media.ImageFormatJPEG,
					}},
				},
			},
			Extensions: inference.Extensions{
				ImageOptions{
					Background:        "opaque",
					OutputCompression: &compression,
					InputFidelity:     "high",
				},
			},
		},
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileImage: %v", err)
	}
	if got := string(compiled.Wire.params.Background); got != "opaque" {
		t.Fatalf("compiled background = %q, want opaque", got)
	}
	if got := compiled.Wire.params.OutputCompression; !got.Valid() || got.Value != 80 {
		t.Fatalf("compiled output_compression = %+v, want 80", got)
	}
	if got := string(compiled.Wire.inputFidelity); got != "high" {
		t.Fatalf("compiled input_fidelity = %q, want high", got)
	}

	raw, err := transportImage(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("transportImage: %v", err)
	}
	if _, err := decodeImage(context.Background(), raw); err != nil {
		t.Fatalf("decodeImage: %v", err)
	}
}

// TestImageOptionsGenerationsTransport pins the same knobs on the JSON
// images/generations body, where input_fidelity has no field at all.
func TestImageOptionsGenerationsTransport(t *testing.T) {
	png := testPNG(t)
	server, _ := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		r *http.Request,
		body map[string]any,
	) {
		if r.URL.Path != "/images/generations" {
			t.Errorf("path = %s, want /images/generations", r.URL.Path)
		}
		if got := body["background"]; got != "transparent" {
			t.Errorf("background = %v, want transparent", got)
		}
		if got := body["output_compression"]; got != float64(90) {
			t.Errorf("output_compression = %v, want 90", got)
		}
		if got := body["output_format"]; got != "webp" {
			t.Errorf("output_format = %v, want webp", got)
		}
		if got := body["moderation"]; got != "low" {
			t.Errorf("moderation = %v, want low", got)
		}
		if got, ok := body["input_fidelity"]; ok {
			t.Errorf("generations body carries input_fidelity = %v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"data": []map[string]any{
				{"b64_json": base64.StdEncoding.EncodeToString(png)},
			},
		})
		_, _ = fmt.Fprint(w, string(payload))
	})
	defer server.Close()
	cls := testClients(t, server)

	compression := 90
	compiled, err := compileImage("gpt-image-2")(
		context.Background(),
		openaiModel("gpt-image-2"),
		inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "a red circle"},
					}},
					Intent: inference.Intent{Image: &inference.ImageIntent{
						OutputFormat: media.ImageFormatWebP,
					}},
				},
			},
			Extensions: inference.Extensions{
				ImageOptions{
					Background:        "transparent",
					OutputCompression: &compression,
					Moderation:        "low",
				},
			},
		},
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileImage: %v", err)
	}
	if _, err := transportImage(cls.api)(context.Background(), compiled.Wire); err != nil {
		t.Fatalf("transportImage: %v", err)
	}
}

// TestImageOptionsRejections pins the documented constraints that decide
// whether a knob can reach the wire at all: the value sets, the output
// formats that carry transparency or compression, and the edits-only input
// fidelity.
func TestImageOptionsRejections(t *testing.T) {
	png := testPNG(t)
	reference, err := media.NewImageBytes(png, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}

	compile := func(
		options ImageOptions,
		format media.ImageFormat,
		withReference bool,
	) (inference.CompileReport, error) {
		t.Helper()
		parts := []message.Part{message.TextPart{Text: "a red circle"}}
		if withReference {
			parts = append(parts, message.ImagePart{Source: reference})
		}
		compiled, err := compileImage("gpt-image-2")(
			context.Background(),
			openaiModel("gpt-image-2"),
			inference.GenerateRequest{
				Input: inference.GenerateInput{
					Role: inference.InputRoleUser,
					Content: inference.InputContent{
						Content: message.Content{Parts: parts},
						Intent: inference.Intent{Image: &inference.ImageIntent{
							OutputFormat: format,
						}},
					},
				},
				Extensions: inference.Extensions{options},
			},
			inference.GenerateExecutionUnary,
		)
		return compiled.Report, err
	}

	compression := 80
	for _, tc := range []struct {
		name          string
		options       ImageOptions
		format        media.ImageFormat
		withReference bool
		field         string
		reason        string
	}{
		{
			name:    "unknown background",
			options: ImageOptions{Background: "clear"},
			field:   "background",
			reason:  "unknown background",
		},
		{
			name:    "transparent needs an alpha format",
			options: ImageOptions{Background: "transparent"},
			format:  media.ImageFormatJPEG,
			field:   "background",
			reason:  "require png or webp",
		},
		{
			name:    "compression needs a compressed format",
			options: ImageOptions{OutputCompression: &compression},
			field:   "output_compression",
			reason:  "applies to webp and jpeg",
		},
		{
			name:    "compression under png",
			options: ImageOptions{OutputCompression: &compression},
			format:  media.ImageFormatPNG,
			field:   "output_compression",
			reason:  "applies to webp and jpeg",
		},
		{
			name:    "compression out of range",
			options: ImageOptions{OutputCompression: ptrInt(101)},
			format:  media.ImageFormatWebP,
			field:   "output_compression",
			reason:  "between 0 and 100",
		},
		{
			name:    "unknown input fidelity",
			options: ImageOptions{InputFidelity: "medium"},
			field:   "input_fidelity",
			reason:  "unknown input_fidelity",
		},
		{
			name:    "input fidelity needs a reference image",
			options: ImageOptions{InputFidelity: "high"},
			field:   "input_fidelity",
			reason:  "at least one inline reference image",
		},
		{
			name:    "unknown moderation",
			options: ImageOptions{Moderation: "high"},
			field:   "moderation",
			reason:  "unknown moderation",
		},
		{
			name:          "moderation has no edits field",
			options:       ImageOptions{Moderation: "low"},
			withReference: true,
			field:         "moderation",
			reason:        "the edits body has no such field",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := compile(tc.options, tc.format, tc.withReference)
			field := inference.ExtensionField(tc.field).Qualify(ImageOptions{})
			assertOptionReject(t, field, report, err, tc.reason)
		})
	}

	// The fidelity knob needs a reference image to measure against, but it
	// does not need a mask: edits are enough.
	if _, err := compile(
		ImageOptions{InputFidelity: "high"},
		"",
		true,
	); err != nil {
		t.Fatalf("input_fidelity with a reference image: %v", err)
	}
}

// ptrInt returns a pointer to value, for the extension's optional knobs.
func ptrInt(value int) *int { return &value }
