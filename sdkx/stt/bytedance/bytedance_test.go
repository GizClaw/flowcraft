package bytedance

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/speech/audio"
	"github.com/GizClaw/flowcraft/sdk/speech/stt"
	"github.com/coder/websocket"
)

func TestNew_RequiresToken(t *testing.T) {
	_, err := New(WithAppID("app"))
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestNew_RequiresAppID(t *testing.T) {
	_, err := New(WithToken("tok"))
	if err == nil {
		t.Fatal("expected error for missing app id")
	}
}

func TestNew_Defaults(t *testing.T) {
	s, err := New(WithAppID("app"), WithToken("tok"))
	if err != nil {
		t.Fatal(err)
	}
	if s.host != defaultHost {
		t.Errorf("host = %q, want %q", s.host, defaultHost)
	}
	if s.model != defaultModel {
		t.Errorf("model = %q, want %q", s.model, defaultModel)
	}
	if s.endWindow != 800 {
		t.Errorf("endWindow = %d, want 800", s.endWindow)
	}
}

func TestOptions(t *testing.T) {
	s, _ := New(
		WithAppID("a"),
		WithToken("t"),
		WithHost("custom.host"),
		WithModel("custom-model"),
		WithUID("user1"),
		EnableNonstream(),
		WithEndWindow(500),
	)
	if s.host != "custom.host" {
		t.Errorf("host = %q", s.host)
	}
	if s.model != "custom-model" {
		t.Errorf("model = %q", s.model)
	}
	if s.uid != "user1" {
		t.Errorf("uid = %q", s.uid)
	}
	if !s.nonstream {
		t.Error("expected nonstream=true")
	}
	if s.endWindow != 500 {
		t.Errorf("endWindow = %d", s.endWindow)
	}
}

func TestSTTOptionApplyProvider(t *testing.T) {
	opt := WithModel("test-model")
	s, _ := New(WithAppID("a"), WithToken("t"))
	opt.ApplySTTProvider(s)
	if s.model != "test-model" {
		t.Errorf("model = %q after ApplySTTProvider", s.model)
	}
	opt.ApplySTTProvider("not an STT")
}

func TestRegistration(t *testing.T) {
	providers := stt.ListSTTProviders()
	found := false
	for _, p := range providers {
		if p == "bytedance" {
			found = true
			break
		}
	}
	if !found {
		t.Error("bytedance not registered in stt.DefaultSTTRegistry")
	}
}

func TestBuildASRRequest(t *testing.T) {
	s, _ := New(WithAppID("app"), WithToken("tok"), WithUID("u1"), EnableNonstream(), WithEndWindow(600))
	o := stt.ApplySTTOptions(stt.WithLanguage("zh"))
	fmt := audio.Format{SampleRate: 16000, Channels: 1, BitDepth: 16}

	req := s.buildASRRequest(o, fmt)

	if req.User.UID != "u1" {
		t.Errorf("uid = %q", req.User.UID)
	}
	if req.Audio.Rate != 16000 {
		t.Errorf("rate = %d", req.Audio.Rate)
	}
	if req.Audio.Format != "pcm" {
		t.Errorf("format = %q, want pcm", req.Audio.Format)
	}
	if req.Audio.Language != "zh" {
		t.Errorf("language = %q", req.Audio.Language)
	}
	if !req.Request.EnableNonstream {
		t.Error("expected EnableNonstream=true")
	}
	if req.Request.EndWindowSize != 600 {
		t.Errorf("end_window_size = %d", req.Request.EndWindowSize)
	}
	if req.Request.ResultType != "full" {
		t.Errorf("result_type = %q", req.Request.ResultType)
	}
}

func TestBuildASRRequest_FormatMapping(t *testing.T) {
	s, _ := New(WithAppID("a"), WithToken("t"))

	o := stt.ApplySTTOptions()
	req := s.buildASRRequest(o, audio.Format{Codec: audio.CodecPCM})
	if req.Audio.Format != "pcm" {
		t.Errorf("default CodecPCM should map to pcm, got %q", req.Audio.Format)
	}

	req2 := s.buildASRRequest(o, audio.Format{Codec: audio.CodecOpus})
	if req2.Audio.Format != "opus" {
		t.Errorf("format = %q, want opus", req2.Audio.Format)
	}
}

func TestGenerateReqID(t *testing.T) {
	id := generateReqID()
	if len(id) != 16 {
		t.Errorf("reqID length = %d, want 16", len(id))
	}
	id2 := generateReqID()
	if id == id2 {
		t.Error("two consecutive reqIDs should differ")
	}
}

// --- Protocol unit tests ---

func TestPackMessage_Roundtrip(t *testing.T) {
	data := []byte(`{"test":"value"}`)
	framed, err := packMessage(msgTypeClientFull, 1, data, false)
	if err != nil {
		t.Fatal(err)
	}

	// packMessage produces client frames: [toc(4) + seq(4) + payloadSize(4) + payload]
	// unpackFrame is designed for server responses (ServerFull, Error), but we can
	// verify the binary layout directly.
	if len(framed) != 4+4+4+len(data) {
		t.Fatalf("frame len = %d, want %d", len(framed), 4+4+4+len(data))
	}

	toc := binary.BigEndian.Uint32(framed[0:4])
	if toc&msgTypeMask != msgTypeClientFull {
		t.Errorf("msgType = %x, want %x", toc&msgTypeMask, msgTypeClientFull)
	}
	if toc&msgTypeFlagMask != flagPositiveSeq {
		t.Errorf("flag = %x, want positive", toc&msgTypeFlagMask)
	}
	seq := binary.BigEndian.Uint32(framed[4:8])
	if seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}
	psize := binary.BigEndian.Uint32(framed[8:12])
	if psize != uint32(len(data)) {
		t.Errorf("payloadSize = %d, want %d", psize, len(data))
	}
	if string(framed[12:]) != string(data) {
		t.Errorf("payload = %q, want %q", string(framed[12:]), string(data))
	}
}

func TestPackMessage_NegativeSeq(t *testing.T) {
	framed, err := packMessage(msgTypeClientAudioOnly, -5, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	hdr, _, err := unpackFrame(framed)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.msgTypeFlag() != flagNegativeSeq {
		t.Errorf("flag = %x, want negative", hdr.msgTypeFlag())
	}
}

func TestPackMessage_WithGzip(t *testing.T) {
	data := []byte(`{"big":"payload with some content to compress"}`)
	framed, err := packMessage(msgTypeClientFull, 1, data, true)
	if err != nil {
		t.Fatal(err)
	}

	// Verify gzip flag in header
	toc := binary.BigEndian.Uint32(framed[0:4])
	if toc&compressionMask != compressionGZip {
		t.Error("expected gzip flag set in header")
	}

	// Extract compressed payload (skip header(4) + seq(4) + payloadSize(4))
	payloadSize := binary.BigEndian.Uint32(framed[8:12])
	compressedPayload := framed[12:]
	if uint32(len(compressedPayload)) != payloadSize {
		t.Fatalf("compressed payload len = %d, header says %d", len(compressedPayload), payloadSize)
	}

	decompressed, err := gzipDecompress(compressedPayload)
	if err != nil {
		t.Fatal(err)
	}
	if string(decompressed) != string(data) {
		t.Errorf("decompressed = %q, want %q", string(decompressed), string(data))
	}
}

func TestDecodeASRResponse_ServerFull(t *testing.T) {
	payload := asrResponsePayload{}
	payload.Result.Text = "你好世界"
	payload.Result.Utterances = []asrUtterance{
		{Text: "你好", Definite: false},
		{Text: "你好世界", Definite: true},
	}
	data, _ := json.Marshal(payload)

	hdr := frameHeader{
		toc: msgTypeServerFull | flagPositiveSeq,
		seq: 1,
	}

	resp, err := decodeASRResponse(hdr, data)
	if err != nil {
		t.Fatal(err)
	}
	if resp.isLast {
		t.Error("should not be last")
	}
	if resp.payload == nil {
		t.Fatal("payload is nil")
	}
	if len(resp.payload.Result.Utterances) != 2 {
		t.Fatalf("utterances = %d, want 2", len(resp.payload.Result.Utterances))
	}
	if !resp.payload.Result.Utterances[1].Definite {
		t.Error("second utterance should be definite")
	}
}

func TestDecodeASRResponse_Error(t *testing.T) {
	hdr := frameHeader{
		toc:  msgTypeError | flagNoSeq,
		code: 1004,
	}
	data := []byte(`{"error":"auth failed"}`)

	resp, err := decodeASRResponse(hdr, data)
	if err != nil {
		t.Fatal(err)
	}
	if resp.code != 1004 {
		t.Errorf("code = %d", resp.code)
	}
	if resp.payload == nil || resp.payload.Error != "auth failed" {
		t.Errorf("error = %v", resp.payload)
	}
}

func TestDecodeASRResponse_LastPackage(t *testing.T) {
	hdr := frameHeader{toc: msgTypeServerFull | flagNegativeSeq}
	resp, err := decodeASRResponse(hdr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.isLast {
		t.Error("should be last (negative seq)")
	}

	hdr2 := frameHeader{toc: msgTypeServerFull | flagNoSeqEOF}
	resp2, err := decodeASRResponse(hdr2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resp2.isLast {
		t.Error("should be last (EOF)")
	}
}

func TestEmitResults_Utterances(t *testing.T) {
	s, _ := New(WithAppID("a"), WithToken("t"))
	resp := &asrResponse{
		payload: &asrResponsePayload{},
	}
	resp.payload.Result.Utterances = []asrUtterance{
		{Text: "partial", Definite: false},
		{Text: "final", Definite: true},
	}

	out := audio.NewPipe[stt.STTResult](10)
	s.emitResults(context.Background(), resp, "zh", out)
	out.Close()

	var results []stt.STTResult
	for {
		r, err := out.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, r)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].IsFinal {
		t.Error("first should be partial")
	}
	if !results[1].IsFinal {
		t.Error("second should be final")
	}
	if results[0].Lang != "zh" {
		t.Errorf("lang = %q", results[0].Lang)
	}
}

func TestEmitResults_TextOnly(t *testing.T) {
	s, _ := New(WithAppID("a"), WithToken("t"))
	resp := &asrResponse{
		isLast:  true,
		payload: &asrResponsePayload{},
	}
	resp.payload.Result.Text = "hello"

	out := audio.NewPipe[stt.STTResult](10)
	s.emitResults(context.Background(), resp, "", out)
	out.Close()

	var results []stt.STTResult
	for {
		r, err := out.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, r)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !results[0].IsFinal {
		t.Error("should be final since isLast")
	}
}

// --- WebSocket integration test with mock server ---

func TestRecognize_MockServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Access-Key") != "test-token" {
			t.Errorf("token = %q", r.Header.Get("X-Api-Access-Key"))
		}
		if r.Header.Get("X-Api-App-Key") != "test-app" {
			t.Errorf("appID = %q", r.Header.Get("X-Api-App-Key"))
		}

		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Logf("accept error: %v", err)
			return
		}
		defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()

		// Read full request
		_, _, err = c.Read(r.Context())
		if err != nil {
			return
		}

		// Read audio chunks until we get a negative-seq frame
		for {
			_, raw, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if len(raw) < 8 {
				continue
			}
			toc := binary.BigEndian.Uint32(raw[0:4])
			if toc&msgTypeFlagMask == flagNegativeSeq {
				break
			}
		}

		// Send response
		respPayload := asrResponsePayload{}
		respPayload.Result.Text = "识别结果"
		data, _ := json.Marshal(respPayload)

		respFrame := buildServerFullFrame(data, true)
		_ = c.Write(r.Context(), websocket.MessageBinary, respFrame)
	}))
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1)

	s, err := New(WithAppID("test-app"), WithToken("test-token"), WithHost(wsURL))
	if err != nil {
		t.Fatal(err)
	}

	audioData := make([]byte, 6400) // 200ms of silence
	input := audio.Frame{
		Data:   audioData,
		Format: audio.Format{SampleRate: 16000, Channels: 1, BitDepth: 16},
	}
	result, err := s.Recognize(context.Background(), input)
	if err != nil {
		t.Fatalf("Recognize error: %v", err)
	}
	if result.Text != "识别结果" {
		t.Errorf("text = %q, want %q", result.Text, "识别结果")
	}
	if !result.IsFinal {
		t.Error("expected IsFinal=true")
	}
}

// buildServerFullFrame constructs a server-full response frame for testing.
func buildServerFullFrame(data []byte, isLast bool) []byte {
	toc := uint32(protoVersion | protoHdrSize | msgTypeServerFull | serializationJSON)
	if isLast {
		toc |= flagNegativeSeq
	} else {
		toc |= flagPositiveSeq
	}

	buf := make([]byte, 4+4+4+len(data))
	binary.BigEndian.PutUint32(buf[0:], toc)
	binary.BigEndian.PutUint32(buf[4:], 1) // seq
	binary.BigEndian.PutUint32(buf[8:], uint32(len(data)))
	copy(buf[12:], data)
	return buf
}

func TestUnpackFrame_ServerFull(t *testing.T) {
	payload := []byte(`{"result":{"text":"hello"}}`)
	frame := buildServerFullFrame(payload, false)

	hdr, data, err := unpackFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.msgType() != msgTypeServerFull {
		t.Errorf("msgType = %x, want %x", hdr.msgType(), msgTypeServerFull)
	}
	if hdr.seq != 1 {
		t.Errorf("seq = %d, want 1", hdr.seq)
	}
	if hdr.payloadSize != uint32(len(payload)) {
		t.Errorf("payloadSize = %d, want %d", hdr.payloadSize, len(payload))
	}
	if string(data) != string(payload) {
		t.Errorf("payload = %q, want %q", string(data), string(payload))
	}
}

func TestUnpackFrame_ServerFullLast(t *testing.T) {
	payload := []byte(`{"result":{"text":"final"}}`)
	frame := buildServerFullFrame(payload, true)

	hdr, _, err := unpackFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.msgTypeFlag() != flagNegativeSeq {
		t.Errorf("flag = %x, want negative seq", hdr.msgTypeFlag())
	}
}

func TestRecognizeStream_MockServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()

		// Read full request
		_, _, err = c.Read(r.Context())
		if err != nil {
			return
		}

		// Read audio until finish
		for {
			_, raw, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if len(raw) < 8 {
				continue
			}
			toc := binary.BigEndian.Uint32(raw[0:4])
			if toc&msgTypeFlagMask == flagNegativeSeq {
				break
			}
		}

		// Send partial result
		partial := asrResponsePayload{}
		partial.Result.Utterances = []asrUtterance{{Text: "你", Definite: false}}
		data, _ := json.Marshal(partial)
		_ = c.Write(r.Context(), websocket.MessageBinary, buildServerFullFrame(data, false))

		// Send final result
		final := asrResponsePayload{}
		final.Result.Utterances = []asrUtterance{{Text: "你好", Definite: true}}
		data, _ = json.Marshal(final)
		_ = c.Write(r.Context(), websocket.MessageBinary, buildServerFullFrame(data, true))
	}))
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1)
	s, _ := New(WithAppID("app"), WithToken("tok"), WithHost(wsURL))

	input := audio.NewPipe[audio.Frame](2)
	input.Send(audio.Frame{
		Data:   make([]byte, 3200),
		Format: audio.Format{SampleRate: 16000, Channels: 1, BitDepth: 16},
	})
	input.Close()

	out, err := s.RecognizeStream(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	var results []stt.STTResult
	for {
		r, err := out.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, r)
	}
	if len(results) < 1 {
		t.Fatal("expected at least 1 result")
	}

	lastResult := results[len(results)-1]
	if lastResult.Text != "你好" {
		t.Errorf("last text = %q, want %q", lastResult.Text, "你好")
	}
	if !lastResult.IsFinal {
		t.Error("last result should be final")
	}
}

func TestGzipRoundtrip(t *testing.T) {
	original := []byte("hello world 你好世界")
	compressed, err := gzipCompress(original)
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := gzipDecompress(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if string(decompressed) != string(original) {
		t.Errorf("roundtrip failed: %q != %q", string(decompressed), string(original))
	}
}
