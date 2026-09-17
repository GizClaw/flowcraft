package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// testPNG is a 1x1 transparent PNG. Tests use it as the reference image and
// the mask together, so the two share the dimensions images/edits requires.
func testPNG(t *testing.T) []byte {
	t.Helper()
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
	)
	if err != nil {
		t.Fatalf("decode test PNG: %v", err)
	}
	return png
}

// TestImageMaskTransport pins local inpainting on the plain OpenAI route. A
// mask is part of the standard images/edits multipart schema, so it needs no
// Azure deployment routing: it must reach the wire as a `mask` part carrying
// the PNG bytes and media type, and every execution of a compiled request
// must upload a fresh reader rather than a drained body.
func TestImageMaskTransport(t *testing.T) {
	png := testPNG(t)
	uploads := 0
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
		for _, name := range []string{"image[]", "mask"} {
			files := r.MultipartForm.File[name]
			if len(files) != 1 {
				t.Errorf("%s files = %d, want 1", name, len(files))
				continue
			}
			if contentType := files[0].Header.Get("Content-Type"); contentType != "image/png" {
				t.Errorf("%s content type = %q, want image/png", name, contentType)
			}
			file, err := files[0].Open()
			if err != nil {
				t.Errorf("open %s: %v", name, err)
				continue
			}
			data, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil {
				t.Errorf("read %s: %v", name, err)
				continue
			}
			if !bytes.Equal(data, png) {
				t.Errorf("%s bytes = %d, want %d", name, len(data), len(png))
			}
		}
		uploads++
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
	mask, err := media.NewImageBytes(png, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	compiled, err := compileImage("gpt-image-2")(
		context.Background(),
		openaiModel("gpt-image-2"),
		inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "fill the transparent area"},
						message.ImagePart{Source: reference},
					}},
					Intent: inference.Intent{Image: &inference.ImageIntent{}},
				},
			},
			Extensions: inference.Extensions{
				ImageOptions{Mask: &mask},
			},
		},
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileImage: %v", err)
	}
	if !bytes.Equal(compiled.Wire.mask.data, png) ||
		compiled.Wire.mask.mediaType != "image/png" {
		t.Fatalf("compiled mask = %+v, want the inline PNG", compiled.Wire.mask)
	}

	raw, err := transportImage(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("transportImage: %v", err)
	}
	if _, err := decodeImage(context.Background(), raw); err != nil {
		t.Fatalf("decodeImage: %v", err)
	}
	if _, err := transportImage(cls.api)(context.Background(), compiled.Wire); err != nil {
		t.Fatalf("second transportImage: %v", err)
	}
	if uploads != 2 {
		t.Fatalf("uploads = %d, want one per attempt", uploads)
	}
}

// TestImageMaskCompilerRejections pins the constraints that do hold: the mask
// has a multipart channel only for inline bytes, and images/edits applies it
// to the first reference image, so one inline reference image must be there.
func TestImageMaskCompilerRejections(t *testing.T) {
	png := testPNG(t)
	mask, err := media.NewImageBytes(png, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	urlMask, err := media.NewImageURL("https://example.com/mask.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	reference, err := media.NewImageBytes(png, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	urlReference, err := media.NewImageURL("https://example.com/reference.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}

	compile := func(
		parts []message.Part,
		mask *media.ImageSource,
	) (inference.CompileReport, error) {
		t.Helper()
		request := inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: parts},
					Intent:  inference.Intent{Image: &inference.ImageIntent{}},
				},
			},
		}
		if mask != nil {
			request.Extensions = inference.Extensions{ImageOptions{Mask: mask}}
		}
		compiled, err := compileImage("gpt-image-2")(
			context.Background(),
			openaiModel("gpt-image-2"),
			request,
			inference.GenerateExecutionUnary,
		)
		return compiled.Report, err
	}

	prompt := message.TextPart{Text: "fill the transparent area"}
	inlineReference := message.ImagePart{Source: reference}
	urlReferencePart := message.ImagePart{Source: urlReference}
	field := inference.ExtensionField("mask").Qualify(ImageOptions{})

	// Inline PNG mask plus an inline reference image is the supported shape.
	if _, err := compile([]message.Part{prompt, inlineReference}, &mask); err != nil {
		t.Fatalf("inline mask compile: %v", err)
	}

	// A URL-sourced mask has no multipart channel.
	report, err := compile([]message.Part{prompt, inlineReference}, &urlMask)
	assertOptionReject(t, field, report, err, "inline")

	// The mask edits the first reference image, so a request without one has
	// nothing to apply it to.
	report, err = compile([]message.Part{prompt}, &mask)
	assertOptionReject(t, field, report, err, "at least one inline reference image")

	// A URL-sourced reference image never reaches the edits body, so it
	// leaves the mask without its image too. The compile fails on the
	// reference image first; the mask decision still records why.
	report, err = compile([]message.Part{prompt, urlReferencePart}, &mask)
	if err == nil {
		t.Fatal("URL reference with mask compile succeeded, want rejection")
	}
	assertOptionDecision(t, field, report, "at least one inline reference image")
}

// assertOptionReject asserts one compile failed on field with a reason an
// operator can act on.
func assertOptionReject(
	t *testing.T,
	field inference.FieldID,
	report inference.CompileReport,
	err error,
	reason string,
) {
	t.Helper()
	var inferenceErr *inference.Error
	if !errors.As(err, &inferenceErr) || inferenceErr.Field != field {
		t.Fatalf("compile error = %v, want rejection at %s", err, field)
	}
	assertOptionDecision(t, field, report, reason)
}

// assertOptionDecision asserts the report carries a rejection on field whose
// reason an operator can act on.
func assertOptionDecision(
	t *testing.T,
	field inference.FieldID,
	report inference.CompileReport,
	reason string,
) {
	t.Helper()
	decision, ok := decisionFor(report.Decisions, field)
	if !ok {
		t.Fatalf("no decision for %s: %+v", field, report.Decisions)
	}
	if decision.Disposition != inference.Rejected {
		t.Fatalf("%s disposition = %q, want rejected", field, decision.Disposition)
	}
	if !strings.Contains(decision.Reason, reason) {
		t.Fatalf("%s reason = %q, want it to mention %q", field, decision.Reason, reason)
	}
}
