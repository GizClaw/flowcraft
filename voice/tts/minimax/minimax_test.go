package minimax

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	speechaudio "github.com/GizClaw/flowcraft/voice/audio"
	speechtts "github.com/GizClaw/flowcraft/voice/tts"
)

func TestNew_RequiresAPIKey(t *testing.T) {
	_, err := New()
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestNew_Defaults(t *testing.T) {
	p, err := New(WithAPIKey("test-key"))
	if err != nil {
		t.Fatal(err)
	}
	if p.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", p.baseURL, defaultBaseURL)
	}
	if p.model != defaultModel {
		t.Errorf("model = %q, want %q", p.model, defaultModel)
	}
	if p.voiceID != defaultVoiceID {
		t.Errorf("voiceID = %q, want %q", p.voiceID, defaultVoiceID)
	}
}

func TestOptions(t *testing.T) {
	p, _ := New(
		WithAPIKey("k"),
		WithBaseURL("https://custom.api"),
		WithModel("speech-2.8-turbo"),
		WithVoiceID("female-shaonv"),
		WithEmotion("happy"),
	)
	if p.baseURL != "https://custom.api" {
		t.Errorf("baseURL = %q", p.baseURL)
	}
	if p.model != "speech-2.8-turbo" {
		t.Errorf("model = %q", p.model)
	}
	if p.voiceID != "female-shaonv" {
		t.Errorf("voiceID = %q", p.voiceID)
	}
	if p.emotion != "happy" {
		t.Errorf("emotion = %q", p.emotion)
	}
}

func TestSynthesize(t *testing.T) {
	audioData := []byte("fake-audio-data")
	hexAudio := hex.EncodeToString(audioData)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/t2a_v2" {
			t.Errorf("path = %s, want /v1/t2a_v2", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth = %q, want %q", got, "Bearer test-key")
		}

		var req t2aRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if req.Stream {
			t.Error("expected stream=false for Synthesize")
		}
		if req.Text != "你好世界" {
			t.Errorf("text = %q", req.Text)
		}
		if req.VoiceSetting.VoiceID != "female-shaonv" {
			t.Errorf("voice_id = %q", req.VoiceSetting.VoiceID)
		}

		resp := t2aResponse{
			Data:     &t2aData{Audio: hexAudio, Status: 2},
			BaseResp: &baseResp{StatusCode: 0, StatusMsg: "success"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	p, _ := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	rc, err := p.Synthesize(context.Background(), "你好世界", speechtts.WithVoice("female-shaonv"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	got, _ := io.ReadAll(rc)
	if string(got) != string(audioData) {
		t.Errorf("audio = %q, want %q", string(got), string(audioData))
	}
}

func TestSynthesize_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := t2aResponse{
			BaseResp: &baseResp{StatusCode: 1004, StatusMsg: "auth failed"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	p, _ := New(WithAPIKey("bad-key"), WithBaseURL(srv.URL))
	_, err := p.Synthesize(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error for API error response")
	}
}

func TestVoices_FromAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/get_voice" {
			t.Errorf("path = %s, want /v1/get_voice", r.URL.Path)
		}

		var req getVoiceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.VoiceType != "all" {
			t.Errorf("voice_type = %q, want all", req.VoiceType)
		}

		resp := getVoiceResponse{
			SystemVoice: []systemVoiceInfo{
				{VoiceID: "sys-voice-1", VoiceName: "系统音色1"},
				{VoiceID: "sys-voice-2", VoiceName: "", Description: []string{"描述音色2"}},
			},
			VoiceCloning: []voiceCloningInfo{
				{VoiceID: "clone-voice-1", Description: []string{"克隆音色"}},
			},
			VoiceGeneration: []voiceGenerationInfo{
				{VoiceID: "gen-voice-1"},
			},
			BaseResp: &baseResp{StatusCode: 0, StatusMsg: "success"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	p, _ := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	voices, err := p.Voices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 4 {
		t.Fatalf("got %d voices, want 4", len(voices))
	}
	if voices[0].ID != "sys-voice-1" || voices[0].Name != "系统音色1" {
		t.Errorf("voices[0] = %+v", voices[0])
	}
	if voices[1].ID != "sys-voice-2" || voices[1].Name != "描述音色2" {
		t.Errorf("voices[1] = %+v", voices[1])
	}
	if voices[2].ID != "clone-voice-1" || voices[2].Name != "克隆音色" {
		t.Errorf("voices[2] = %+v", voices[2])
	}
	if voices[3].ID != "gen-voice-1" || voices[3].Name != "gen-voice-1" {
		t.Errorf("voices[3] = %+v", voices[3])
	}
}

func TestVoices_Fallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p, _ := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	voices, err := p.Voices(context.Background())
	if err == nil {
		t.Fatal("expected error indicating fallback")
	}
	if !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("expected fallback error, got: %v", err)
	}
	if len(voices) == 0 {
		t.Fatal("expected fallback voices")
	}
	found := false
	for _, v := range voices {
		if v.ID == defaultVoiceID {
			found = true
		}
	}
	if !found {
		t.Errorf("default voice %q not in fallback list", defaultVoiceID)
	}
}

func TestSynthesizeStream(t *testing.T) {
	chunk1 := hex.EncodeToString([]byte("chunk-1"))
	chunk2 := hex.EncodeToString([]byte("chunk-2"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req t2aRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if !req.Stream {
			t.Error("expected stream=true for SynthesizeStream")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		events := []t2aResponse{
			{Data: &t2aData{Audio: chunk1, Status: 1}, BaseResp: &baseResp{StatusCode: 0}},
			{Data: &t2aData{Audio: chunk2, Status: 1}, BaseResp: &baseResp{StatusCode: 0}},
			{Data: &t2aData{Audio: "", Status: 2}, BaseResp: &baseResp{StatusCode: 0, StatusMsg: "success"}},
		}
		for _, ev := range events {
			data, _ := json.Marshal(ev)
			if _, err := w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
				t.Fatalf("write sse: %v", err)
			}
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p, _ := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))

	textPipe := speechaudio.NewPipe[string](1)
	textPipe.Send("测试流式")
	textPipe.Close()

	audioStream, err := p.SynthesizeStream(context.Background(), textPipe)
	if err != nil {
		t.Fatal(err)
	}

	var chunks []speechtts.Utterance
	for {
		c, err := audioStream.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, c)
	}

	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want at least 2", len(chunks))
	}
	if string(chunks[0].Data) != "chunk-1" {
		t.Errorf("chunk[0] = %q, want %q", string(chunks[0].Data), "chunk-1")
	}
	if string(chunks[1].Data) != "chunk-2" {
		t.Errorf("chunk[1] = %q, want %q", string(chunks[1].Data), "chunk-2")
	}
	if chunks[0].Sequence != 0 || chunks[1].Sequence != 1 {
		t.Errorf("sequence = [%d, %d], want [0, 1]", chunks[0].Sequence, chunks[1].Sequence)
	}
	if chunks[0].Text != "测试流式" {
		t.Errorf("chunk[0].Text = %q, want %q", chunks[0].Text, "测试流式")
	}
	if chunks[0].Format.Codec != speechaudio.CodecMP3 {
		t.Errorf("chunk[0].Format.Codec = %v, want CodecMP3", chunks[0].Format.Codec)
	}
}

func TestSynthesizeStream_FormatCodecMatchesOutput(t *testing.T) {
	// MiniMax may return bytes in a codec other than the one requested
	// (opus -> mp3, wav -> flac, pcm -> mp3). The Format.Codec label on emitted
	// frames must describe the bytes the API actually returned, and the format
	// string sent to the API must correspond to that same codec.
	cases := []struct {
		name       string
		reqCodec   speechaudio.Codec
		wantFormat string
		wantCodec  speechaudio.Codec
	}{
		{"pcm->mp3", speechaudio.CodecPCM, "mp3", speechaudio.CodecMP3},
		{"opus->mp3", speechaudio.CodecOpus, "mp3", speechaudio.CodecMP3},
		{"wav->flac", speechaudio.CodecWAV, "flac", speechaudio.CodecFLAC},
		{"mp3->mp3", speechaudio.CodecMP3, "mp3", speechaudio.CodecMP3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunk := hex.EncodeToString([]byte("audio"))
			var gotFormat string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req t2aRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				gotFormat = req.AudioSetting.Format

				flusher, _ := w.(http.Flusher)
				events := []t2aResponse{
					{Data: &t2aData{Audio: chunk, Status: 1}, BaseResp: &baseResp{StatusCode: 0}},
					{Data: &t2aData{Audio: "", Status: 2}, BaseResp: &baseResp{StatusCode: 0}},
				}
				for _, ev := range events {
					data, _ := json.Marshal(ev)
					if _, err := w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
						t.Fatalf("write sse: %v", err)
					}
					flusher.Flush()
				}
			}))
			defer srv.Close()

			p, _ := New(WithAPIKey("k"), WithBaseURL(srv.URL))
			textPipe := speechaudio.NewPipe[string](1)
			textPipe.Send("测试")
			textPipe.Close()

			stream, err := p.SynthesizeStream(context.Background(), textPipe, speechtts.WithCodec(tc.reqCodec))
			if err != nil {
				t.Fatal(err)
			}
			frame, err := stream.Read()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			// Drain the stream.
			for {
				if _, err := stream.Read(); err == io.EOF {
					break
				} else if err != nil {
					t.Fatalf("drain: %v", err)
				}
			}

			if gotFormat != tc.wantFormat {
				t.Errorf("api format = %q, want %q", gotFormat, tc.wantFormat)
			}
			if frame.Format.Codec != tc.wantCodec {
				t.Errorf("frame codec = %v, want %v", frame.Format.Codec, tc.wantCodec)
			}
		})
	}
}

func TestSynthesizeStream_LargeFinalStatus2Line(t *testing.T) {
	// Issue #4: production reads the SSE stream line-by-line. The original
	// bufio.Scanner imposes a max-token cap (even when bumped to 1MB via
	// scanner.Buffer), so any single `data:` line longer than that cap makes
	// Scan() fail with bufio.ErrTooLong — aborting the read loop and silently
	// dropping every remaining chunk on the stream. The fix switched to
	// bufio.Reader, whose ReadString has no line-length cap.
	//
	// To catch a regression back to Scanner, the oversized line MUST sit on an
	// INCREMENTAL audio event (status != 2 — the branch that decodes + Sends),
	// followed by a further small incremental chunk. Under Scanner the >1MB
	// line trips ErrTooLong: the large chunk is never decoded/sent AND the
	// trailing small chunk is lost. Under Reader both are delivered. Putting
	// the big payload on the skipped status=2 final line (as an earlier version
	// did) is indistinguishable between the two implementations, so this test
	// deliberately does not do that.
	small := []byte("small-trailing-chunk")
	smallHex := hex.EncodeToString(small)

	// A > 1 MiB raw payload => > 2 MiB hex on the wire, well past both the
	// default 64KB Scanner cap and a 1MB scanner.Buffer cap. Fill with a
	// recognizable byte so the decoded bytes can be asserted exactly.
	bigAudio := make([]byte, 1<<20+512)
	for i := range bigAudio {
		bigAudio[i] = 0xAB
	}
	bigHex := hex.EncodeToString(bigAudio)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		write := func(ev t2aResponse) {
			data, _ := json.Marshal(ev)
			if _, err := w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
				t.Errorf("write sse: %v", err)
			}
			flusher.Flush()
		}
		// Oversized INCREMENTAL chunk first, then a small incremental chunk,
		// then the redundant status=2 final (skipped by production).
		write(t2aResponse{Data: &t2aData{Audio: bigHex, Status: 1}, BaseResp: &baseResp{StatusCode: 0}})
		write(t2aResponse{Data: &t2aData{Audio: smallHex, Status: 1}, BaseResp: &baseResp{StatusCode: 0}})
		write(t2aResponse{Data: &t2aData{Audio: "", Status: 2}, BaseResp: &baseResp{StatusCode: 0, StatusMsg: "success"}})
	}))
	defer srv.Close()

	p, _ := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	textPipe := speechaudio.NewPipe[string](1)
	textPipe.Send("测试大行")
	textPipe.Close()

	stream, err := p.SynthesizeStream(context.Background(), textPipe)
	if err != nil {
		t.Fatal(err)
	}

	var got [][]byte
	var termErr error
	for {
		u, err := stream.Read()
		if err != nil {
			termErr = err
			break
		}
		got = append(got, append([]byte(nil), u.Data...))
	}

	if termErr != io.EOF {
		t.Fatalf("terminal error = %v, want io.EOF (large incremental line must not fail synthesis)", termErr)
	}
	// Both incremental chunks must be delivered: the >1MB one AND the small
	// one after it. Under the old bufio.Scanner the oversized line trips
	// ErrTooLong and BOTH are lost, so this count assertion fails.
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2 (>1MB incremental + trailing small incremental)", len(got))
	}
	if !bytes.Equal(got[0], bigAudio) {
		t.Errorf("large chunk: got %d bytes, want %d bytes with exact payload match", len(got[0]), len(bigAudio))
	}
	if string(got[1]) != string(small) {
		t.Errorf("trailing chunk = %q, want %q", string(got[1]), string(small))
	}
}

func TestBuildRequest_FormatMapping(t *testing.T) {
	p, _ := New(WithAPIKey("k"))

	o := speechtts.ApplyTTSOptions()
	req := p.buildRequest("test", false, o)
	if req.AudioSetting.Format != "mp3" {
		t.Errorf("CodecPCM default should map to mp3, got %q", req.AudioSetting.Format)
	}

	o2 := speechtts.ApplyTTSOptions(speechtts.WithCodec(speechaudio.CodecWAV))
	req2 := p.buildRequest("test", false, o2)
	if req2.AudioSetting.Format != "flac" {
		t.Errorf("format = %q, want flac", req2.AudioSetting.Format)
	}
}

func TestBuildRequest_VoiceOverride(t *testing.T) {
	p, _ := New(WithAPIKey("k"), WithVoiceID("default-voice"))

	o := speechtts.ApplyTTSOptions(speechtts.WithVoice("custom-voice"))
	req := p.buildRequest("test", false, o)
	if req.VoiceSetting.VoiceID != "custom-voice" {
		t.Errorf("voice_id = %q, want custom-voice", req.VoiceSetting.VoiceID)
	}
}

func TestBuildRequest_SpeedMapping(t *testing.T) {
	p, _ := New(WithAPIKey("k"))

	o := speechtts.ApplyTTSOptions(speechtts.WithSpeed(1.5))
	req := p.buildRequest("test", false, o)
	if req.VoiceSetting.Speed != 1.5 {
		t.Errorf("speed = %f, want 1.5", req.VoiceSetting.Speed)
	}
}

func TestWarmup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, _ := New(WithAPIKey("k"), WithBaseURL(srv.URL))
	err := p.Warmup(context.Background())
	if err != nil {
		t.Fatalf("Warmup error: %v", err)
	}
}

func TestWarmup_StatusHandling(t *testing.T) {
	// A completed handshake is warmup's goal, so a client-side (4xx) status —
	// which posting "{}" typically produces — must count as success, while a
	// server-side (5xx) status is reported as a failure.
	cases := []struct {
		status  int
		wantErr bool
	}{
		{http.StatusOK, false},
		{http.StatusBadRequest, false},
		{http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"base_resp":{"status_code":1004}}`))
		}))
		p, _ := New(WithAPIKey("k"), WithBaseURL(srv.URL))
		err := p.Warmup(context.Background())
		if tc.wantErr && err == nil {
			t.Errorf("status %d: expected error, got nil", tc.status)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("status %d: unexpected error: %v", tc.status, err)
		}
		srv.Close()
	}
}

func TestRegistration(t *testing.T) {
	providers := speechtts.ListTTSProviders()
	found := false
	for _, p := range providers {
		if p == "minimax" {
			found = true
			break
		}
	}
	if !found {
		t.Error("minimax not registered in voice.DefaultTTSRegistry")
	}
}

func TestVoiceClone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/voice_clone" {
			t.Errorf("path = %s, want /v1/voice_clone", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth = %q, want %q", got, "Bearer test-key")
		}

		var req voiceCloneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if req.FileID != 123456 {
			t.Errorf("file_id = %d, want 123456", req.FileID)
		}
		if req.VoiceID != "TestVoice001" {
			t.Errorf("voice_id = %q, want TestVoice001", req.VoiceID)
		}

		resp := voiceCloneResponse{
			BaseResp: &baseResp{StatusCode: 0, StatusMsg: "success"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	p, _ := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	result, err := p.VoiceClone(context.Background(), 123456, "TestVoice001")
	if err != nil {
		t.Fatal(err)
	}
	if result.VoiceID != "TestVoice001" {
		t.Errorf("VoiceID = %q, want TestVoice001", result.VoiceID)
	}
}

func TestVoiceClone_WithOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req voiceCloneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if req.ClonePrompt == nil {
			t.Fatal("expected clone_prompt to be set")
		}
		if req.ClonePrompt.PromptAudio != 789 {
			t.Errorf("prompt_audio = %d, want 789", req.ClonePrompt.PromptAudio)
		}
		if req.ClonePrompt.PromptText != "示例文本" {
			t.Errorf("prompt_text = %q, want 示例文本", req.ClonePrompt.PromptText)
		}
		if req.Text != "试听文本" {
			t.Errorf("text = %q, want 试听文本", req.Text)
		}
		if req.Model != "speech-2.8-hd" {
			t.Errorf("model = %q, want speech-2.8-hd", req.Model)
		}
		if req.LanguageBoost != "Chinese" {
			t.Errorf("language_boost = %q, want Chinese", req.LanguageBoost)
		}
		if req.NeedNoiseReduction == nil || !*req.NeedNoiseReduction {
			t.Error("expected need_noise_reduction = true")
		}

		resp := voiceCloneResponse{
			DemoAudio: "https://example.com/demo.mp3",
			BaseResp:  &baseResp{StatusCode: 0, StatusMsg: "success"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	p, _ := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	result, err := p.VoiceClone(context.Background(), 123456, "TestVoice002",
		WithClonePrompt(789, "示例文本"),
		WithPreviewText("试听文本", "speech-2.8-hd"),
		WithLanguageBoost("Chinese"),
		WithNoiseReduction(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.DemoAudio != "https://example.com/demo.mp3" {
		t.Errorf("DemoAudio = %q, want https://example.com/demo.mp3", result.DemoAudio)
	}
}

func TestVoiceClone_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := voiceCloneResponse{
			BaseResp: &baseResp{StatusCode: 2038, StatusMsg: "no clone permission"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	p, _ := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	_, err := p.VoiceClone(context.Background(), 123456, "TestVoice003")
	if err == nil {
		t.Fatal("expected error for API error response")
	}
}

func TestVoiceClone_ValidationErrors(t *testing.T) {
	p, _ := New(WithAPIKey("k"))

	_, err := p.VoiceClone(context.Background(), 0, "TestVoice")
	if err == nil {
		t.Error("expected error for missing file_id")
	}

	_, err = p.VoiceClone(context.Background(), 123, "")
	if err == nil {
		t.Error("expected error for missing voice_id")
	}

	_, err = p.VoiceClone(context.Background(), 123, "TestVoice",
		WithPreviewText("hello", ""),
	)
	if err == nil {
		t.Error("expected error when text provided without model")
	}
}

func TestTTSOptionApplyProvider(t *testing.T) {
	opt := WithModel("speech-2.8-turbo")
	p, _ := New(WithAPIKey("k"))

	opt.ApplyTTSProvider(p)
	if p.model != "speech-2.8-turbo" {
		t.Errorf("model = %q after ApplyTTSProvider", p.model)
	}

	opt.ApplyTTSProvider("not a TTS")
}
