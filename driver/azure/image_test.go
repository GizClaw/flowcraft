package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// ---------------------------------------------------------------------------
// Shared fixtures.
// ---------------------------------------------------------------------------

// capturedAzure serves the API surface used by the driver and records every
// request body for assertion.
type capturedAzure struct {
	t *testing.T

	bodies  []map[string]any
	handler func(w http.ResponseWriter, r *http.Request, body map[string]any)
}

func newCapturedAzure(
	t *testing.T,
	handler func(w http.ResponseWriter, r *http.Request, body map[string]any),
) (*httptest.Server, *capturedAzure) {
	capture := &capturedAzure{t: t, handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		// Restore the body so handlers can re-read multipart forms.
		r.Body = io.NopCloser(bytes.NewReader(payload))
		var body map[string]any
		if len(payload) > 0 &&
			strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Errorf("body is not JSON: %v", err)
				return
			}
		}
		capture.bodies = append(capture.bodies, body)
		handler(w, r, body)
	}))
	return server, capture
}

func (c *capturedAzure) body(index int) map[string]any {
	c.t.Helper()
	if index >= len(c.bodies) {
		c.t.Fatalf("only %d captured requests", len(c.bodies))
	}
	return c.bodies[index]
}

func azureImageClients(t *testing.T, server *httptest.Server) *clients {
	t.Helper()
	spec, err := decodeSpec([]byte(
		fmt.Sprintf(`{
			"endpoint": %q,
			"models": [{"name": "gpt-image-1", "kind": "image",
				"capabilities": {"outputs": ["image"]}}]
		}`, server.URL),
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	return profileMaterial{apiKey: "test-key"}.newClients(spec)
}

func azureImageModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: driverID, Name: name},
		Profile: "default",
	}
}

// testPNG is a 1x1 transparent PNG used as reference image and mask.
var testPNG = func() []byte {
	data, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
	)
	if err != nil {
		panic(err)
	}
	return data
}()

func imageResponse(w http.ResponseWriter, inputTokens int64) {
	w.Header().Set("Content-Type", "application/json")
	payload, _ := json.Marshal(map[string]any{
		"data":  []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(testPNG)}},
		"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
	})
	_, _ = fmt.Fprint(w, string(payload))
}

// ---------------------------------------------------------------------------
// Transport.
// ---------------------------------------------------------------------------

func TestImageTransport(t *testing.T) {
	server, capture := newCapturedAzure(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		body map[string]any,
	) {
		if size, _ := body["size"].(string); size != "1536x1024" {
			t.Errorf("size = %v", body["size"])
		}
		if fmt.Sprint(body["output_format"]) != "png" {
			t.Errorf("output_format = %v", body["output_format"])
		}
		imageResponse(w, 11)
	})
	defer server.Close()
	cls := azureImageClients(t, server)

	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "a red circle"},
				}},
				Intent: inference.Intent{Image: &inference.ImageIntent{
					Size:         &media.ImageSize{Width: 1536, Height: 1024},
					OutputFormat: media.ImageFormatPNG,
				}},
			},
		},
	}
	compiled, err := compileImage("gpt-image-1")(
		context.Background(),
		azureImageModel("gpt-image-1"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileImage: %v", err)
	}
	raw, err := transportImage(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("transportImage: %v", err)
	}
	response, err := decodeImage(context.Background(), raw)
	if err != nil {
		t.Fatalf("decodeImage: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	part, ok := response.Message.Content.Parts[0].(message.ImagePart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if part.Source.Kind() != media.SourceInline ||
		!bytes.Equal(part.Source.Bytes(), testPNG) {
		t.Fatalf("image source bytes mismatch")
	}
	_ = capture.body(0)
}

func TestImageEditTransport(t *testing.T) {
	source, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	server, capture := newCapturedAzure(t, func(
		w http.ResponseWriter,
		r *http.Request,
		_ map[string]any,
	) {
		if r.URL.Path != "/openai/deployments/gpt-image-1/images/edits" {
			t.Errorf("path = %s, want /openai/deployments/gpt-image-1/images/edits",
				r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		files := r.MultipartForm.File["image[]"]
		if len(files) != 1 {
			t.Errorf("image files = %d, want 1", len(files))
			return
		}
		file, err := files[0].Open()
		if err != nil {
			t.Errorf("open image file: %v", err)
			return
		}
		defer func() { _ = file.Close() }()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Errorf("read image file: %v", err)
			return
		}
		if !bytes.Equal(data, testPNG) {
			t.Errorf("uploaded image bytes = %d, want %d", len(data), len(testPNG))
		}
		if prompt := r.MultipartForm.Value["prompt"]; len(prompt) != 1 ||
			prompt[0] != "make it a red circle" {
			t.Errorf("prompt = %v", r.MultipartForm.Value["prompt"])
		}
		imageResponse(w, 12)
	})
	defer server.Close()
	cls := azureImageClients(t, server)

	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "make it a red circle"},
					message.ImagePart{Source: source},
				}},
				Intent: inference.Intent{Image: &inference.ImageIntent{}},
			},
		},
	}
	compiled, err := compileImage("gpt-image-1")(
		context.Background(),
		azureImageModel("gpt-image-1"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileImage: %v", err)
	}
	if len(compiled.Wire.images) != 1 {
		t.Fatalf("wire images = %d, want 1", len(compiled.Wire.images))
	}
	raw, err := transportImage(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("transportImage: %v", err)
	}
	if _, err := decodeImage(context.Background(), raw); err != nil {
		t.Fatalf("decodeImage: %v", err)
	}
	_ = capture.body(0)
}

func TestImageEditTransportMask(t *testing.T) {
	source, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	mask, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	server, capture := newCapturedAzure(t, func(
		w http.ResponseWriter,
		r *http.Request,
		_ map[string]any,
	) {
		if r.URL.Path != "/openai/deployments/gpt-image-1/images/edits" {
			t.Errorf("path = %s, want /openai/deployments/gpt-image-1/images/edits",
				r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		files := r.MultipartForm.File["image[]"]
		if len(files) != 1 {
			t.Errorf("image files = %d, want 1", len(files))
			return
		}
		masks := r.MultipartForm.File["mask"]
		if len(masks) != 1 {
			t.Errorf("mask files = %d, want 1", len(masks))
			return
		}
		readPart := func(header *multipart.FileHeader) []byte {
			t.Helper()
			file, err := header.Open()
			if err != nil {
				t.Errorf("open file: %v", err)
				return nil
			}
			defer func() { _ = file.Close() }()
			data, err := io.ReadAll(file)
			if err != nil {
				t.Errorf("read file: %v", err)
				return nil
			}
			return data
		}
		if data := readPart(files[0]); !bytes.Equal(data, testPNG) {
			t.Errorf("uploaded image bytes = %d, want %d", len(data), len(testPNG))
		}
		if data := readPart(masks[0]); !bytes.Equal(data, testPNG) {
			t.Errorf("uploaded mask bytes = %d, want %d", len(data), len(testPNG))
		}
		if size := r.MultipartForm.Value["size"]; len(size) != 1 ||
			size[0] != "1024x1024" {
			t.Errorf("size = %v", r.MultipartForm.Value["size"])
		}
		imageResponse(w, 13)
	})
	defer server.Close()
	cls := azureImageClients(t, server)

	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "replace the sky"},
					message.ImagePart{Source: source},
				}},
				Intent: inference.Intent{Image: &inference.ImageIntent{
					Size: &media.ImageSize{Width: 1024, Height: 1024},
				}},
			},
		},
		Extensions: inference.Extensions{
			ImageOptions{Mask: &mask},
		},
	}
	compiled, err := compileImage("gpt-image-1")(
		context.Background(),
		azureImageModel("gpt-image-1"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileImage: %v", err)
	}
	if len(compiled.Wire.images) != 1 {
		t.Fatalf("wire images = %d, want 1", len(compiled.Wire.images))
	}
	if compiled.Wire.mask.Kind() != media.SourceInline ||
		!bytes.Equal(compiled.Wire.mask.Bytes(), testPNG) {
		t.Fatalf("wire mask = %+v", compiled.Wire.mask)
	}
	raw, err := transportImage(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("transportImage: %v", err)
	}
	if _, err := decodeImage(context.Background(), raw); err != nil {
		t.Fatalf("decodeImage: %v", err)
	}
	_ = capture.body(0)
}
