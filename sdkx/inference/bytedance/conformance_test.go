package bytedance

// Framework conformance: this file wires the provider into the shared
// inferencetest suites. Generate suites run against a captured HTTP Ark
// server; session suites run against minimal WebSocket endpoints. The
// suites never read session events, so the endpoints only complete the
// transport handshake — the ASR sink drains frames, and the duplex
// endpoint answers session.create with session.created. Event mapping is
// covered by the driver-level tests in asr_test.go / realtime_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	"github.com/GizClaw/flowcraft/sdk/inference/media"

	"github.com/gorilla/websocket"
)

// countingTransport wraps one pipeline transport stage with a probe.
func countingTransport[Wire, Raw any](
	calls *inferencetest.Counter,
	next inference.Transport[Wire, Raw],
) inference.Transport[Wire, Raw] {
	return func(ctx context.Context, wire Wire) (Raw, error) {
		calls.Inc()
		return next(ctx, wire)
	}
}

// instrumentedGenerateDrivers binds the generate pipeline directly (no
// factory) so the transport probe sits inside the bound drivers.
func instrumentedGenerateDrivers(
	t *testing.T,
	server *httptest.Server,
	calls *inferencetest.Counter,
) (inference.GenerateDriver, inference.GenerateStreamDriver) {
	t.Helper()
	spec, err := decodeSpec([]byte(
		fmt.Sprintf(`{"base_url":%q}`, server.URL+"/api/v3"),
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	cls := profileMaterial{apiKey: "test-key"}.newClients(spec)
	ark, err := cls.requireArk("default")
	if err != nil {
		t.Fatalf("requireArk: %v", err)
	}
	operations, err := inference.BindGenerateOperations(
		compileGenerate("doubao-seed-2-1-pro", catalog["doubao-seed-2-1-pro"]),
		countingTransport(calls, transportGenerate(ark)),
		decodeGenerate,
		countingTransport(calls, transportGenerateStream(ark)),
		decodeGenerateStream,
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	return operations.Unary, operations.Stream
}

// wsTestServer upgrades every request and pumps each frame to onMessage.
func wsTestServer(
	t *testing.T,
	onMessage func(conn *websocket.Conn, messageType int, payload []byte),
) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if onMessage != nil {
				onMessage(conn, messageType, payload)
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// wsURL converts an httptest HTTP URL to its WebSocket form.
func wsURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// instrumentedSessionSpec builds the provider Spec pointed at one WebSocket
// endpoint plus the speech client for the default profile.
func instrumentedSessionClients(
	t *testing.T,
	server *httptest.Server,
) *clients {
	t.Helper()
	spec, err := decodeSpec([]byte(
		fmt.Sprintf(`{"speech_web_socket_url":%q}`, wsURL(server)),
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	material := profileMaterial{
		apiKey: "test-key",
		spec:   ProfileSpec{AppID: "test-app"},
	}
	cls := material.newClients(spec)
	if _, err := cls.requireSpeech("default"); err != nil {
		t.Fatalf("requireSpeech: %v", err)
	}
	return cls
}

// instrumentedRuntime assembles a Runtime exposing one model with the given
// openers, bypassing the factory so probes can sit inside the pipelines.
func instrumentedRuntime(
	t *testing.T,
	model string,
	openers inference.Openers,
) *inference.Runtime {
	t.Helper()
	provider := inference.ProviderDefinition{
		ID:       "bytedance",
		Profiles: []inference.ProfileDefinition{{ID: "default"}},
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: "bytedance", Name: model},
			},
			Openers: openers,
		}},
	}
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

func TestConformanceGenerateUnary(t *testing.T) {
	server, _ := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem("ok"),
		}))
	})
	defer server.Close()
	calls := &inferencetest.Counter{}
	unary, _ := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateUnary(t, inferencetest.GenerateUnarySuite{
		Model:   generateModel("doubao-seed-2-1-pro"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Driver:  unary,
		TransportCalls: func() int64 {
			return calls.Load()
		},
		AssertResponse: func(t *testing.T, response inference.GenerateResponse) {
			if len(response.Message.Content.Parts) != 1 {
				t.Fatalf("parts = %d", len(response.Message.Content.Parts))
			}
			text, ok := response.Message.Content.Parts[0].(inference.TextPart)
			if !ok || text.Text != "ok" {
				t.Fatalf("part = %#v", response.Message.Content.Parts[0])
			}
			if response.FinishReason != inference.FinishCompleted {
				t.Fatalf("finish = %q", response.FinishReason)
			}
		},
	})
}

func TestConformanceGenerateStream(t *testing.T) {
	server, _ := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody(
			map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "message"},
			},
			map[string]any{
				"type": "response.output_text.delta", "output_index": 0, "delta": "ok",
			},
			map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": "resp_1", "status": "completed",
					"usage": map[string]any{
						"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
					},
				},
			},
		))
	})
	defer server.Close()
	calls := &inferencetest.Counter{}
	_, stream := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateStream(t, inferencetest.GenerateStreamSuite{
		Model:   generateModel("doubao-seed-2-1-pro"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Driver:  stream,
		TransportCalls: func() int64 {
			return calls.Load()
		},
		AssertResult: func(t *testing.T, response inference.GenerateResponse) {
			if response.FinishReason != inference.FinishCompleted {
				t.Fatalf("finish = %q", response.FinishReason)
			}
			if len(response.Message.Content.Parts) != 1 {
				t.Fatalf("parts = %d", len(response.Message.Content.Parts))
			}
			text, ok := response.Message.Content.Parts[0].(inference.TextPart)
			if !ok || text.Text != "ok" {
				t.Fatalf("part = %#v", response.Message.Content.Parts[0])
			}
			if response.Usage.TotalTokens != 2 {
				t.Fatalf("usage = %+v", response.Usage)
			}
		},
	})
}

func TestConformanceGenerateCompileParity(t *testing.T) {
	server, _ := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		t.Error("parity checks are explain-only; transport must not run")
	})
	defer server.Close()
	calls := &inferencetest.Counter{}
	unary, stream := instrumentedGenerateDrivers(t, server, calls)

	inferencetest.RunGenerateCompileParity(t, inferencetest.GenerateCompileParitySuite{
		Model:   generateModel("doubao-seed-2-1-pro"),
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Unary:   unary,
		Stream:  stream,
	})
}

func TestConformanceGenerateCompiler(t *testing.T) {
	model := generateModel("doubao-seed-2-1-pro")

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[generateWire]{
		Model:   model,
		Shape:   inference.GenerateExecutionUnary,
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileGenerate("doubao-seed-2-1-pro", catalog["doubao-seed-2-1-pro"]),
		AssertWire: func(t *testing.T, wire generateWire) {
			if wire.model != "doubao-seed-2-1-pro" {
				t.Fatalf("wire model = %q", wire.model)
			}
			if wire.stream {
				t.Fatal("unary shape compiled a stream wire")
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "data part has no representation",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						inference.DataPart{
							MediaType: "application/vnd.example",
							Value:     json.RawMessage(`{"k":1}`),
						},
					)
					return request
				},
				Field: inference.FieldGenerateInputData,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "reasoning is assistant-only",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						inference.ReasoningPart{Text: "trace"},
					)
					return request
				},
				Field: inference.FieldGenerateInputReasoning,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "image intent on text model",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Intent.Image = &inference.ImageIntent{}
					return request
				},
				Field: inference.FieldGenerateIntentImage,
				Kind:  inference.UnsupportedFeature,
			},
		},
		Drops: []inferencetest.CompilerDrop[inference.GenerateRequest]{
			{
				Name: "ark does not consume reasoning input",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Context = append(request.Context, inference.Message{
						Role: inference.RoleAssistant,
						Content: inference.Content{Parts: []inference.Part{
							inference.ReasoningPart{Text: "trace", ID: "rs_1"},
							inference.TextPart{Text: "answer"},
						}},
					})
					return request
				},
				Field: inference.FieldGenerateContextReasoning,
			},
		},
	})
}

// The capability matrix also needs a bare model: a custom declaration with
// no vision, video, or reasoning support must reject those channels and
// still keep a complete ledger on plain text.
func TestConformanceGenerateCompilerPlainModel(t *testing.T) {
	spec, err := decodeSpec([]byte(
		`{"models":[{"name":"my-plain-model","kind":"generate"}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	video, err := media.NewVideoURL("https://example.com/v.mp4", "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	model := generateModel("my-plain-model")

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[generateWire]{
		Model:   model,
		Shape:   inference.GenerateExecutionUnary,
		Request: func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileGenerate("my-plain-model", models["my-plain-model"]),
		AssertWire: func(t *testing.T, wire generateWire) {
			if wire.model != "my-plain-model" {
				t.Fatalf("wire model = %q", wire.model)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "video on model without video input",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						inference.VideoPart{Source: video},
					)
					return request
				},
				Field: inference.FieldGenerateInputVideo,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "reasoning on non-thinking model",
				Request: func() inference.GenerateRequest {
					request := simpleTextRequest("hi")
					request.Input.Content.Intent.Text.ReasoningEffort =
						inference.ReasoningLow
					return request
				},
				Field: inference.FieldGenerateIntentReasoningEffort,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})
}

// The video compiler targets the task API and caps resolution per model:
// the suite pins the generic contract while TestVideoRejections covers the
// full capability matrix.
func TestConformanceGenerateCompilerVideo(t *testing.T) {
	video, err := media.NewVideoURL("https://example.com/in.mp4", "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	model := generateModel("doubao-seedance-2-0-fast")

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[videoWire]{
		Model:   model,
		Shape:   inference.GenerateExecutionUnary,
		Request: func() inference.GenerateRequest { return videoRequest() },
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileVideo("doubao-seedance-2-0-fast", catalog["doubao-seedance-2-0-fast"]),
		AssertWire: func(t *testing.T, wire videoWire) {
			if wire.model != "doubao-seedance-2-0-fast" {
				t.Fatalf("wire model = %q", wire.model)
			}
			if wire.firstFrame != "" || wire.lastFrame != "" {
				t.Fatalf("text-only request compiled frames: %+v", wire)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "video reference input",
				Request: func() inference.GenerateRequest {
					request := videoRequest()
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						inference.VideoPart{Source: video},
					)
					return request
				},
				Field: inference.FieldGenerateInputVideo,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "resolution over model cap",
				Request: func() inference.GenerateRequest {
					request := videoRequest()
					request.Input.Content.Intent.Video.Resolution = "1080p"
					return request
				},
				Field: inference.FieldGenerateIntentVideoResolution,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "audio intent",
				Request: func() inference.GenerateRequest {
					request := videoRequest()
					request.Input.Content.Intent.Audio = &inference.AudioIntent{
						Voice:  media.VoiceSpec{ID: "v"},
						Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
					}
					return request
				},
				Field: inference.FieldGenerateIntentAudio,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})
}

// countingTranscriptionSession probes the provider session behind the bound
// driver.
type countingTranscriptionSession struct {
	inference.TranscriptionSession
	sends       *inferencetest.Counter
	inputCloses *inferencetest.Counter
	nexts       *inferencetest.Counter
	closes      *inferencetest.Counter
}

func (s *countingTranscriptionSession) SendAudio(
	ctx context.Context,
	chunk media.AudioChunk,
) error {
	s.sends.Inc()
	return s.TranscriptionSession.SendAudio(ctx, chunk)
}

func (s *countingTranscriptionSession) CloseInput(ctx context.Context) error {
	s.inputCloses.Inc()
	return s.TranscriptionSession.CloseInput(ctx)
}

func (s *countingTranscriptionSession) Next(
	ctx context.Context,
) (inference.TranscriptionEvent, error) {
	s.nexts.Inc()
	return s.TranscriptionSession.Next(ctx)
}

func (s *countingTranscriptionSession) Close() error {
	s.closes.Inc()
	return s.TranscriptionSession.Close()
}

func TestConformanceTranscriptionSession(t *testing.T) {
	server := wsTestServer(t, nil) // drain-only sink; the suite reads no events
	cls := instrumentedSessionClients(t, server)

	opens := &inferencetest.Counter{}
	sends := &inferencetest.Counter{}
	inputCloses := &inferencetest.Counter{}
	nexts := &inferencetest.Counter{}
	closes := &inferencetest.Counter{}

	driver, err := inference.BindTranscriptionSession(
		compileASR("doubao-asr-sauc-2-0"),
		func(
			ctx context.Context,
			wire asrWire,
		) (inference.TranscriptionSession, error) {
			session, err := openASRSession(cls.speech)(ctx, wire)
			if err != nil {
				return nil, err
			}
			opens.Inc()
			return &countingTranscriptionSession{
				TranscriptionSession: session,
				sends:                sends,
				inputCloses:          inputCloses,
				nexts:                nexts,
				closes:               closes,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindTranscriptionSession: %v", err)
	}
	runtime := instrumentedRuntime(t, "doubao-asr-sauc-2-0", inference.Openers{
		Transcription: func(
			context.Context,
			inference.ModelRef,
		) (inference.TranscriptionOperations, error) {
			return inference.TranscriptionOperations{Session: driver}, nil
		},
	})

	inferencetest.RunTranscriptionSession(t, inferencetest.TranscriptionSessionSuite{
		Runtime: runtime,
		Model:   generateModel("doubao-asr-sauc-2-0"),
		Config: func() inference.TranscriptionSessionConfig {
			return inference.TranscriptionSessionConfig{
				InputFormat: media.AudioFormat{
					Encoding:     media.AudioEncodingPCM16,
					SampleRateHz: 16000,
					Channels:     1,
				},
			}
		},
		Chunk: func() media.AudioChunk {
			return media.AudioChunk{Data: []byte{0, 1, 2, 3}}
		},
		SessionOpens:  opens.Load,
		AudioSends:    sends.Load,
		InputCloses:   inputCloses.Load,
		NextCalls:     nexts.Load,
		SessionCloses: closes.Load,
	})
}

// countingRealtimeSession probes the provider session behind the bound
// driver.
type countingRealtimeSession struct {
	inference.ProviderRealtimeSession[realtimeInputWire, realtimeRaw]
	sends   *inferencetest.Counter
	cancels *inferencetest.Counter
	nexts   *inferencetest.Counter
	closes  *inferencetest.Counter
}

func (s *countingRealtimeSession) Send(
	ctx context.Context,
	wire realtimeInputWire,
) error {
	s.sends.Inc()
	return s.ProviderRealtimeSession.Send(ctx, wire)
}

func (s *countingRealtimeSession) CancelResponse(ctx context.Context) error {
	s.cancels.Inc()
	return s.ProviderRealtimeSession.CancelResponse(ctx)
}

func (s *countingRealtimeSession) Next(ctx context.Context) (realtimeRaw, error) {
	s.nexts.Inc()
	return s.ProviderRealtimeSession.Next(ctx)
}

func (s *countingRealtimeSession) Close() error {
	s.closes.Inc()
	return s.ProviderRealtimeSession.Close()
}

func TestConformanceRealtimeSession(t *testing.T) {
	// The duplex handshake blocks on session.created; everything else is
	// drained.
	server := wsTestServer(t, func(
		conn *websocket.Conn,
		_ int,
		payload []byte,
	) {
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Errorf("duplex frame is not JSON: %v", err)
			return
		}
		if event.Type == "session.create" {
			if err := conn.WriteMessage(
				websocket.TextMessage,
				[]byte(`{"type":"session.created","session":{"id":"duplex-conformance"}}`),
			); err != nil {
				t.Errorf("write session.created: %v", err)
			}
		}
	})
	cls := instrumentedSessionClients(t, server)

	opens := &inferencetest.Counter{}
	inputCompiles := &inferencetest.Counter{}
	sends := &inferencetest.Counter{}
	cancels := &inferencetest.Counter{}
	nexts := &inferencetest.Counter{}
	closes := &inferencetest.Counter{}

	driver, err := inference.BindRealtime(
		compileRealtime(cls),
		func(
			ctx context.Context,
			wire realtimeWire,
		) (inference.ProviderRealtimeSession[realtimeInputWire, realtimeRaw], error) {
			session, err := openRealtimeSession(cls.speech)(ctx, wire)
			if err != nil {
				return nil, err
			}
			opens.Inc()
			return &countingRealtimeSession{
				ProviderRealtimeSession: session,
				sends:                   sends,
				cancels:                 cancels,
				nexts:                   nexts,
				closes:                  closes,
			}, nil
		},
		func(
			ctx context.Context,
			model inference.ModelRef,
			input inference.RealtimeInput,
		) (inference.Compiled[realtimeInputWire], error) {
			inputCompiles.Inc()
			return compileRealtimeInput(ctx, model, input)
		},
		decodeRealtimeEvent,
	)
	if err != nil {
		t.Fatalf("BindRealtime: %v", err)
	}
	runtime := instrumentedRuntime(t, "doubao-seeduplex-3-0", inference.Openers{
		Realtime: func(
			context.Context,
			inference.ModelRef,
		) (inference.RealtimeDriver, error) {
			return driver, nil
		},
	})

	inferencetest.RunRealtimeSession(t, inferencetest.RealtimeSessionSuite{
		Runtime: runtime,
		Model:   generateModel("doubao-seeduplex-3-0"),
		Config: func() inference.RealtimeConfig {
			return inference.RealtimeConfig{
				Instructions: "be terse",
				Modalities: []inference.Modality{
					inference.ModalityAudio,
					inference.ModalityText,
				},
				OutputAudioFormat: &media.AudioFormat{
					Encoding:     media.AudioEncodingPCM16,
					SampleRateHz: 24000,
					Channels:     1,
				},
				Voice: &media.VoiceSpec{ID: "zh_female_qingxin"},
			}
		},
		Input: func() inference.RealtimeInput {
			return inference.RealtimeTextInput{Text: "hello"}
		},
		SessionOpens:  opens.Load,
		InputCompiles: inputCompiles.Load,
		InputSends:    sends.Load,
		Cancellations: cancels.Load,
		NextCalls:     nexts.Load,
		SessionCloses: closes.Load,
	})
}
