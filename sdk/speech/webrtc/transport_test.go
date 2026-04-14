package webrtc_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/speech/audio"
	"github.com/GizClaw/flowcraft/sdk/speech/tts"
	rtc "github.com/GizClaw/flowcraft/sdk/speech/webrtc"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// ---------------------------------------------------------------------------
// Test codecs & fakes
// ---------------------------------------------------------------------------

type compactEncoder struct {
	mu       sync.Mutex
	lastSize int
}

func (e *compactEncoder) Encode(pcm []byte) ([]byte, error) {
	e.mu.Lock()
	e.lastSize = len(pcm)
	e.mu.Unlock()
	if len(pcm) > 160 {
		return pcm[:160], nil
	}
	return pcm, nil
}
func (e *compactEncoder) Reset() {}
func (e *compactEncoder) LastInputSize() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastSize
}

type passthroughDecoder struct{}

func (passthroughDecoder) Decode(data []byte) ([]byte, error) { return data, nil }
func (passthroughDecoder) Reset()                             {}

var (
	_ rtc.AudioEncoder = &compactEncoder{}
	_ rtc.AudioDecoder = passthroughDecoder{}
)

type failEncoder struct{ calls atomic.Int64 }

func (e *failEncoder) Encode([]byte) ([]byte, error) {
	e.calls.Add(1)
	return nil, errors.New("encode boom")
}
func (e *failEncoder) Reset() {}

type emptyEncoder struct{}

func (emptyEncoder) Encode([]byte) ([]byte, error) { return nil, nil }
func (emptyEncoder) Reset()                        {}

type countEncoder struct {
	mu    sync.Mutex
	calls int
}

func (e *countEncoder) Encode(pcm []byte) ([]byte, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	if len(pcm) > 160 {
		return pcm[:160], nil
	}
	return pcm, nil
}
func (e *countEncoder) Reset() {}
func (e *countEncoder) Count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

type fakeSinkTrack struct {
	mu      sync.Mutex
	count   int
	samples []media.Sample
}

func (f *fakeSinkTrack) WriteSample(s media.Sample) error {
	f.mu.Lock()
	f.count++
	f.samples = append(f.samples, s)
	f.mu.Unlock()
	return nil
}

type errorTrack struct{ calls atomic.Int64 }

func (t *errorTrack) WriteSample(media.Sample) error {
	t.calls.Add(1)
	return errors.New("track write boom")
}

type inMemorySignaler struct {
	mu        sync.Mutex
	answer    rtc.SessionDescription
	answerErr error
	closed    bool
}

func (s *inMemorySignaler) Exchange(_ context.Context, _ rtc.SessionDescription) (rtc.SessionDescription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answer, s.answerErr
}
func (s *inMemorySignaler) OnICECandidate(func(rtc.ICECandidate))                       {}
func (s *inMemorySignaler) AddICECandidate(_ context.Context, _ rtc.ICECandidate) error { return nil }
func (s *inMemorySignaler) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

type blockingSignaler struct {
	ch chan struct{}
}

func (s *blockingSignaler) Exchange(ctx context.Context, _ rtc.SessionDescription) (rtc.SessionDescription, error) {
	select {
	case <-ctx.Done():
		return rtc.SessionDescription{}, ctx.Err()
	case <-s.ch:
		return rtc.SessionDescription{}, errors.New("unexpected unblock")
	}
}
func (s *blockingSignaler) OnICECandidate(func(rtc.ICECandidate))                       {}
func (s *blockingSignaler) AddICECandidate(_ context.Context, _ rtc.ICECandidate) error { return nil }
func (s *blockingSignaler) Close() error                                                { return nil }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makePCM16Tone(samples int) []byte {
	buf := make([]byte, samples*2)
	for i := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(8192))
	}
	return buf
}

func readWithTimeout(s audio.Stream[audio.Frame], ctx context.Context) (audio.Frame, error) {
	type result struct {
		f   audio.Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := s.Read()
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-ctx.Done():
		return audio.Frame{}, ctx.Err()
	}
}

func sdpExchange(t *testing.T, ctx context.Context, server *rtc.Transport, clientPC *webrtc.PeerConnection) {
	t.Helper()

	clientOffer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("client CreateOffer: %v", err)
	}
	gatherDone := webrtc.GatheringCompletePromise(clientPC)
	if err := clientPC.SetLocalDescription(clientOffer); err != nil {
		t.Fatalf("client SetLocalDescription: %v", err)
	}
	<-gatherDone

	serverAnswer, err := server.Accept(ctx, rtc.SessionDescription{
		Type: "offer",
		SDP:  clientPC.LocalDescription().SDP,
	})
	if err != nil {
		t.Fatalf("server Accept: %v", err)
	}

	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  serverAnswer.SDP,
	}); err != nil {
		t.Fatalf("client SetRemoteDescription: %v", err)
	}
}

type connWaiter struct {
	ready chan struct{}
}

func newConnWaiter(pc *webrtc.PeerConnection) *connWaiter {
	w := &connWaiter{ready: make(chan struct{})}
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case <-w.ready:
			default:
				close(w.ready)
			}
		}
	})
	return w
}

func (w *connWaiter) wait(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-w.ready:
	case <-ctx.Done():
		t.Fatal("timeout waiting for connection")
	}
}

func newTransport(t *testing.T, opts ...func(*rtc.TransportConfig)) *rtc.Transport {
	t.Helper()
	cfg := rtc.TransportConfig{Encoder: &compactEncoder{}, Decoder: passthroughDecoder{}}
	for _, o := range opts {
		o(&cfg)
	}
	tr, err := rtc.NewTransport(cfg)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func newClientPC(t *testing.T) (*webrtc.PeerConnection, *webrtc.TrackLocalStaticSample) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	track, _ := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "mic", "client")
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc, track
}

func connectPair(t *testing.T, ctx context.Context, server *rtc.Transport) (*webrtc.PeerConnection, *webrtc.TrackLocalStaticSample) {
	t.Helper()
	pc, track := newClientPC(t)
	cw := newConnWaiter(pc)
	sdpExchange(t, ctx, server, pc)
	cw.wait(t, ctx)
	return pc, track
}

// ---------------------------------------------------------------------------
// Tests — Transport construction & validation
// ---------------------------------------------------------------------------

func TestTransport_NewTransport_RequiresEncoderDecoder(t *testing.T) {
	_, err := rtc.NewTransport(rtc.TransportConfig{})
	if err == nil {
		t.Fatal("expected error when Encoder is nil")
	}

	_, err = rtc.NewTransport(rtc.TransportConfig{Encoder: &compactEncoder{}})
	if err == nil {
		t.Fatal("expected error when Decoder is nil")
	}

	tr, err := rtc.NewTransport(rtc.TransportConfig{
		Encoder: &compactEncoder{},
		Decoder: passthroughDecoder{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = tr.Close()
}

// ---------------------------------------------------------------------------
// Tests — Accept path (server / answerer)
// ---------------------------------------------------------------------------

func TestTransport_AcceptConnect_Loopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var receivedFrames [][]byte
	var framesMu sync.Mutex
	framesReady := make(chan struct{}, 1)

	server := newTransport(t)

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client NewPeerConnection: %v", err)
	}
	defer func() { _ = clientPC.Close() }()

	clientTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"mic", "client",
	)
	if err != nil {
		t.Fatalf("client NewTrackLocalStaticSample: %v", err)
	}
	if _, err := clientPC.AddTrack(clientTrack); err != nil {
		t.Fatalf("client AddTrack: %v", err)
	}

	clientPC.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		buf := make([]byte, 65535)
		for {
			n, _, err := track.Read(buf)
			if err != nil {
				return
			}
			framesMu.Lock()
			receivedFrames = append(receivedFrames, append([]byte(nil), buf[:n]...))
			framesMu.Unlock()
			select {
			case framesReady <- struct{}{}:
			default:
			}
		}
	})

	cw := newConnWaiter(clientPC)
	sdpExchange(t, ctx, server, clientPC)
	cw.wait(t, ctx)

	pcmData := makePCM16Tone(480)
	if err := clientTrack.WriteSample(media.Sample{Data: pcmData, Duration: 10 * time.Millisecond}); err != nil {
		t.Fatalf("client WriteSample: %v", err)
	}

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	frame, readErr := readWithTimeout(server.Source().Stream(), readCtx)
	if readErr != nil {
		t.Fatalf("server source read: %v", readErr)
	}
	if len(frame.Data) == 0 {
		t.Fatal("server source returned empty frame")
	}

	uttPipe := audio.NewPipe[tts.Utterance](8)
	for range 5 {
		ttsData := makePCM16Tone(960)
		uttPipe.Send(tts.Utterance{
			Frame: audio.Frame{
				Data:   ttsData,
				Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 48000, Channels: 1, BitDepth: 16},
			},
			Text: "hello",
		})
		time.Sleep(20 * time.Millisecond)
	}
	uttPipe.Close()

	playDone := server.Sink().Play(uttPipe)
	select {
	case <-playDone:
	case <-ctx.Done():
		t.Fatal("timeout waiting for Sink.Play to finish")
	}

	select {
	case <-framesReady:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for client to receive audio")
	}

	framesMu.Lock()
	count := len(receivedFrames)
	framesMu.Unlock()
	if count == 0 {
		t.Fatal("client received no audio frames from server")
	}

	_ = server.Close()
	_, err = server.Source().Stream().Read()
	if err == nil {
		t.Fatal("expected error after Close, got nil")
	}
}

func TestTransport_Accept_InvalidSDP(t *testing.T) {
	server := newTransport(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := server.Accept(ctx, rtc.SessionDescription{Type: "offer", SDP: "garbage"})
	if err == nil {
		t.Fatal("expected error with invalid SDP")
	}
}

func TestTransport_Accept_Idempotent_InitPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := newTransport(t)

	pc1, _ := newClientPC(t)
	sdpExchange(t, ctx, server, pc1)

	pc2, _ := newClientPC(t)
	offer, _ := pc2.CreateOffer(nil)
	gatherDone := webrtc.GatheringCompletePromise(pc2)
	if err := pc2.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-gatherDone

	_, err := server.Accept(ctx, rtc.SessionDescription{
		Type: "offer",
		SDP:  pc2.LocalDescription().SDP,
	})
	if err == nil {
		t.Log("second Accept succeeded (expected: initPC is idempotent)")
	}
}

// ---------------------------------------------------------------------------
// Tests — Connect path (client / offerer)
// ---------------------------------------------------------------------------

func TestTransport_Connect_RequiresSignaler(t *testing.T) {
	tr := newTransport(t)
	err := tr.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error when Signaler is nil")
	}
}

func TestTransport_Connect_ExchangeError(t *testing.T) {
	sig := &inMemorySignaler{answerErr: errors.New("signaling failed")}
	tr := newTransport(t, func(cfg *rtc.TransportConfig) { cfg.Signaler = sig })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := tr.Connect(ctx)
	if err == nil {
		t.Fatal("expected error from signaler")
	}
	if !strings.Contains(err.Error(), "signaling failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTransport_Connect_ContextCancelled(t *testing.T) {
	sig := &blockingSignaler{ch: make(chan struct{})}
	tr := newTransport(t, func(cfg *rtc.TransportConfig) { cfg.Signaler = sig })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := tr.Connect(ctx)
	if err == nil {
		t.Fatal("expected context deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests — DataChannel
// ---------------------------------------------------------------------------

func TestTransport_DataChannel_ControlMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	controlReceived := make(chan rtc.ClientMessage, 1)

	server := newTransport(t, func(cfg *rtc.TransportConfig) {
		cfg.OnControlMessage = func(msg rtc.ClientMessage) {
			select {
			case controlReceived <- msg:
			default:
			}
		}
	})

	clientPC, _ := newClientPC(t)
	clientDC, err := clientPC.CreateDataChannel("control", nil)
	if err != nil {
		t.Fatalf("client CreateDataChannel: %v", err)
	}
	dcReady := make(chan struct{})
	clientDC.OnOpen(func() { close(dcReady) })

	sdpExchange(t, ctx, server, clientPC)

	select {
	case <-dcReady:
	case <-ctx.Done():
		t.Fatal("timeout waiting for DataChannel")
	}

	msg := rtc.ClientMessage{Type: rtc.MessageInterrupt}
	data, _ := json.Marshal(msg)
	if err := clientDC.Send(data); err != nil {
		t.Fatalf("DataChannel Send: %v", err)
	}

	select {
	case got := <-controlReceived:
		if got.Type != rtc.MessageInterrupt {
			t.Fatalf("expected interrupt message, got %q", got.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for control message")
	}
}

func TestTransport_DataChannel_InvalidJSON_Ignored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var calls atomic.Int64
	server := newTransport(t, func(cfg *rtc.TransportConfig) {
		cfg.OnControlMessage = func(rtc.ClientMessage) { calls.Add(1) }
	})

	clientPC, _ := newClientPC(t)
	clientDC, _ := clientPC.CreateDataChannel("control", nil)
	dcReady := make(chan struct{})
	clientDC.OnOpen(func() { close(dcReady) })

	sdpExchange(t, ctx, server, clientPC)

	select {
	case <-dcReady:
	case <-ctx.Done():
		t.Fatal("timeout")
	}

	_ = clientDC.Send([]byte("not json"))
	time.Sleep(200 * time.Millisecond)

	if c := calls.Load(); c != 0 {
		t.Fatalf("expected 0 callback invocations for invalid JSON, got %d", c)
	}
}

func TestTransport_DataChannel_NoCallback_Safe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)

	clientPC, _ := newClientPC(t)
	clientDC, _ := clientPC.CreateDataChannel("control", nil)
	dcReady := make(chan struct{})
	clientDC.OnOpen(func() { close(dcReady) })

	sdpExchange(t, ctx, server, clientPC)

	select {
	case <-dcReady:
	case <-ctx.Done():
		t.Fatal("timeout")
	}

	msg := rtc.ClientMessage{Type: rtc.MessageInterrupt}
	data, _ := json.Marshal(msg)
	_ = clientDC.Send(data)
	time.Sleep(200 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// Tests — SendEvent
// ---------------------------------------------------------------------------

func TestTransport_SendEvent_BeforeConnect_ReturnsFalse(t *testing.T) {
	tr := newTransport(t)
	ok := tr.SendEvent(rtc.ServerMessage{Type: rtc.MessageEvent})
	if ok {
		t.Fatal("expected SendEvent to return false before connection")
	}
}

func TestTransport_SendEvent_AfterClose_ReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	connectPair(t, ctx, server)

	_ = server.Close()

	ok := server.SendEvent(rtc.ServerMessage{Type: rtc.MessageEvent})
	if ok {
		t.Fatal("expected SendEvent to return false after Close")
	}
}

func TestTransport_SendEvent_Delivers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	clientPC, _ := newClientPC(t)

	received := make(chan []byte, 1)
	// Client creates the DC (offerer) — server receives it via OnDataChannel.
	clientDC, err := clientPC.CreateDataChannel("control", nil)
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	clientDC.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case received <- append([]byte(nil), msg.Data...):
		default:
		}
	})

	cw := newConnWaiter(clientPC)
	sdpExchange(t, ctx, server, clientPC)
	cw.wait(t, ctx)

	// Wait for server-side OnDataChannel callback to fire and DC to open.
	time.Sleep(500 * time.Millisecond)

	ok := server.SendEvent(rtc.ServerMessage{Type: rtc.MessageEvent})
	if !ok {
		t.Fatal("SendEvent should succeed when DC is open")
	}

	select {
	case data := <-received:
		var msg rtc.ServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Type != rtc.MessageEvent {
			t.Fatalf("expected event message, got %q", msg.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for server event on client")
	}
}

// ---------------------------------------------------------------------------
// Tests — Close semantics
// ---------------------------------------------------------------------------

func TestTransport_Close_Idempotent(t *testing.T) {
	tr := newTransport(t)
	for i := range 5 {
		if err := tr.Close(); err != nil {
			t.Fatalf("Close call %d: %v", i, err)
		}
	}
}

func TestTransport_Close_InterruptsActiveSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	_, track := connectPair(t, ctx, server)

	_ = track.WriteSample(media.Sample{Data: makePCM16Tone(480), Duration: 10 * time.Millisecond})
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	_, err := readWithTimeout(server.Source().Stream(), readCtx)
	if err != nil {
		t.Fatalf("source read: %v", err)
	}

	_ = server.Close()

	_, err = server.Source().Stream().Read()
	if err == nil {
		t.Fatal("expected error after Close")
	}
}

func TestTransport_Close_InterruptsSinkPlay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	connectPair(t, ctx, server)

	pipe := audio.NewPipe[tts.Utterance](64)
	for range 10 {
		pipe.Send(tts.Utterance{
			Frame: audio.Frame{
				Data:   makePCM16Tone(960),
				Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
			},
		})
	}

	done := server.Sink().Play(pipe)

	time.Sleep(10 * time.Millisecond)
	pipe.Interrupt()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Sink.Play did not stop after Interrupt")
	}
}

func TestTransport_Sink_NilBeforeAccept(t *testing.T) {
	tr := newTransport(t)
	if tr.Sink() != nil {
		t.Fatal("Sink should be nil before Accept/Connect")
	}
}

// ---------------------------------------------------------------------------
// Tests — Source
// ---------------------------------------------------------------------------

func TestSource_ReadRTP_ExtractsPayload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	_, track := connectPair(t, ctx, server)

	_ = track.WriteSample(media.Sample{Data: makePCM16Tone(480), Duration: 10 * time.Millisecond})

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	frame, err := readWithTimeout(server.Source().Stream(), readCtx)
	if err != nil {
		t.Fatalf("source read: %v", err)
	}
	if len(frame.Data) == 0 {
		t.Fatal("expected non-empty frame data")
	}
	const rtpHeaderMinSize = 12
	maxExpected := 960 + rtpHeaderMinSize
	if len(frame.Data) > maxExpected {
		t.Fatalf("frame data too large (%d bytes), likely contains RTP header", len(frame.Data))
	}
}

func TestSource_Format_ReflectsConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t, func(cfg *rtc.TransportConfig) {
		cfg.Source = rtc.SourceConfig{SampleRate: 16000, Channels: 2}
	})
	_, track := connectPair(t, ctx, server)

	_ = track.WriteSample(media.Sample{Data: makePCM16Tone(480), Duration: 10 * time.Millisecond})

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	frame, err := readWithTimeout(server.Source().Stream(), readCtx)
	if err != nil {
		t.Fatalf("source read: %v", err)
	}
	if frame.Format.SampleRate != 16000 {
		t.Fatalf("expected SampleRate 16000, got %d", frame.Format.SampleRate)
	}
	if frame.Format.Channels != 2 {
		t.Fatalf("expected Channels 2, got %d", frame.Format.Channels)
	}
}

func TestSource_Format_DefaultsTo48kMono(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	_, track := connectPair(t, ctx, server)

	_ = track.WriteSample(media.Sample{Data: makePCM16Tone(480), Duration: 10 * time.Millisecond})

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	frame, err := readWithTimeout(server.Source().Stream(), readCtx)
	if err != nil {
		t.Fatalf("source read: %v", err)
	}
	if frame.Format.SampleRate != 48000 {
		t.Fatalf("expected default SampleRate 48000, got %d", frame.Format.SampleRate)
	}
	if frame.Format.Channels != 1 {
		t.Fatalf("expected default Channels 1, got %d", frame.Format.Channels)
	}
}

func TestSource_ReadLoop_OnceOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	_, track := connectPair(t, ctx, server)

	_ = track.WriteSample(media.Sample{Data: makePCM16Tone(480), Duration: 10 * time.Millisecond})

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	_, err := readWithTimeout(server.Source().Stream(), readCtx)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}

	_ = server.Close()
	_, err = server.Source().Stream().Read()
	if err == nil {
		t.Fatal("expected error after Close")
	}

	_, err = server.Source().Stream().Read()
	if err == nil {
		t.Fatal("expected error on second read after Close")
	}
}

func TestSource_Sequence_Increments(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	_, track := connectPair(t, ctx, server)

	const numPackets = 5
	for range numPackets {
		_ = track.WriteSample(media.Sample{Data: makePCM16Tone(480), Duration: 10 * time.Millisecond})
		time.Sleep(5 * time.Millisecond)
	}

	var seqs []int64
	for range numPackets {
		readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
		frame, err := readWithTimeout(server.Source().Stream(), readCtx)
		readCancel()
		if err != nil {
			break
		}
		seqs = append(seqs, frame.Sequence)
	}

	if len(seqs) < 2 {
		t.Fatalf("expected at least 2 frames, got %d", len(seqs))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("sequence not monotonically increasing: %v", seqs)
		}
	}
}

func TestSource_CaptureTime_IsRecent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	_, track := connectPair(t, ctx, server)

	before := time.Now()
	_ = track.WriteSample(media.Sample{Data: makePCM16Tone(480), Duration: 10 * time.Millisecond})

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	frame, err := readWithTimeout(server.Source().Stream(), readCtx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if frame.CaptureTime.Before(before) {
		t.Fatal("CaptureTime is before the send time")
	}
	if time.Since(frame.CaptureTime) > 5*time.Second {
		t.Fatal("CaptureTime is too old")
	}
}

// ---------------------------------------------------------------------------
// Tests — Sink (unit tests via NewSinkForTest)
// ---------------------------------------------------------------------------

func TestSink_ZeroPadsTailFrame(t *testing.T) {
	enc := &compactEncoder{}
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, enc)

	data := makePCM16Tone(1250) // 2500 bytes → 1 full (1920) + 1 tail (580 → padded to 1920)
	uttPipe := audio.NewPipe[tts.Utterance](1)
	uttPipe.Send(tts.Utterance{
		Frame: audio.Frame{
			Data:   data,
			Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 48000, Channels: 1, BitDepth: 16},
		},
	})
	uttPipe.Close()

	<-sink.Play(uttPipe)

	track.mu.Lock()
	sampleCount := track.count
	track.mu.Unlock()

	if sampleCount != 2 {
		t.Fatalf("expected 2 WriteSample calls (full + padded tail), got %d", sampleCount)
	}

	lastSize := enc.LastInputSize()
	if lastSize != 1920 {
		t.Fatalf("expected encoder to receive 1920-byte padded frame, got %d", lastSize)
	}
}

func TestSink_EmptyData_NoWrite(t *testing.T) {
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, &compactEncoder{})

	pipe := audio.NewPipe[tts.Utterance](1)
	pipe.Send(tts.Utterance{
		Frame: audio.Frame{
			Data:   nil,
			Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
		},
	})
	pipe.Close()

	<-sink.Play(pipe)

	track.mu.Lock()
	c := track.count
	track.mu.Unlock()
	if c != 0 {
		t.Fatalf("expected 0 WriteSample for empty data, got %d", c)
	}
}

func TestSink_EncoderFailure_DropsFrame(t *testing.T) {
	enc := &failEncoder{}
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, enc)

	pipe := audio.NewPipe[tts.Utterance](1)
	pipe.Send(tts.Utterance{
		Frame: audio.Frame{
			Data:   makePCM16Tone(960),
			Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
		},
	})
	pipe.Close()

	<-sink.Play(pipe)

	if enc.calls.Load() == 0 {
		t.Fatal("encoder should have been called")
	}
	track.mu.Lock()
	c := track.count
	track.mu.Unlock()
	if c != 0 {
		t.Fatalf("expected 0 WriteSample when encoder fails, got %d", c)
	}
}

func TestSink_EncoderReturnsEmpty_DropsFrame(t *testing.T) {
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, emptyEncoder{})

	pipe := audio.NewPipe[tts.Utterance](1)
	pipe.Send(tts.Utterance{
		Frame: audio.Frame{
			Data:   makePCM16Tone(960),
			Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
		},
	})
	pipe.Close()

	<-sink.Play(pipe)

	track.mu.Lock()
	c := track.count
	track.mu.Unlock()
	if c != 0 {
		t.Fatalf("expected 0 WriteSample when encoder returns empty, got %d", c)
	}
}

func TestSink_TrackWriteFailure_Continues(t *testing.T) {
	enc := &countEncoder{}
	track := &errorTrack{}
	sink := rtc.NewSinkForTest(track, enc)

	pipe := audio.NewPipe[tts.Utterance](4)
	for range 3 {
		pipe.Send(tts.Utterance{
			Frame: audio.Frame{
				Data:   makePCM16Tone(960),
				Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
			},
		})
	}
	pipe.Close()

	<-sink.Play(pipe)

	if enc.Count() < 3 {
		t.Fatalf("encoder should have been called at least 3 times, got %d", enc.Count())
	}
	if track.calls.Load() < 3 {
		t.Fatalf("track.WriteSample should have been called at least 3 times, got %d", track.calls.Load())
	}
}

func TestSink_MultipleUtterances(t *testing.T) {
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, &compactEncoder{})

	pipe := audio.NewPipe[tts.Utterance](8)
	for i := range 5 {
		pipe.Send(tts.Utterance{
			Frame: audio.Frame{
				Data:   makePCM16Tone(960),
				Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
			},
			Text: fmt.Sprintf("utt_%d", i),
		})
	}
	pipe.Close()

	<-sink.Play(pipe)

	track.mu.Lock()
	c := track.count
	track.mu.Unlock()
	if c != 5 {
		t.Fatalf("expected 5 WriteSample calls for 5 exact-size utterances, got %d", c)
	}
}

func TestSink_NonStandardSampleRate(t *testing.T) {
	enc := &compactEncoder{}
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, enc)

	// 16kHz mono: 20ms = 320 samples = 640 bytes per frame
	pipe := audio.NewPipe[tts.Utterance](1)
	pipe.Send(tts.Utterance{
		Frame: audio.Frame{
			Data:   makePCM16Tone(320),
			Format: audio.Format{SampleRate: 16000, Channels: 1, BitDepth: 16},
		},
	})
	pipe.Close()

	<-sink.Play(pipe)

	track.mu.Lock()
	c := track.count
	track.mu.Unlock()
	if c != 1 {
		t.Fatalf("expected 1 WriteSample for 16kHz 20ms frame, got %d", c)
	}

	lastSize := enc.LastInputSize()
	if lastSize != 640 {
		t.Fatalf("expected 640-byte frame for 16kHz, got %d", lastSize)
	}
}

func TestSink_StereoFormat(t *testing.T) {
	enc := &compactEncoder{}
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, enc)

	// 48kHz stereo: 20ms = 960 samples * 2 ch * 2 bytes = 3840 bytes per frame
	pipe := audio.NewPipe[tts.Utterance](1)
	pipe.Send(tts.Utterance{
		Frame: audio.Frame{
			Data:   makePCM16Tone(1920), // 3840 bytes = exactly 1 frame
			Format: audio.Format{SampleRate: 48000, Channels: 2, BitDepth: 16},
		},
	})
	pipe.Close()

	<-sink.Play(pipe)

	track.mu.Lock()
	c := track.count
	track.mu.Unlock()
	if c != 1 {
		t.Fatalf("expected 1 WriteSample for stereo frame, got %d", c)
	}

	if enc.LastInputSize() != 3840 {
		t.Fatalf("expected 3840-byte frame for stereo 48kHz, got %d", enc.LastInputSize())
	}
}

func TestSink_ZeroSampleRate_DefaultsTo48k(t *testing.T) {
	enc := &compactEncoder{}
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, enc)

	// 48kHz mono: 20ms = 1920 bytes. Send exactly 1920 bytes with zero SampleRate.
	pipe := audio.NewPipe[tts.Utterance](1)
	pipe.Send(tts.Utterance{
		Frame: audio.Frame{
			Data:   makePCM16Tone(960),
			Format: audio.Format{SampleRate: 0, Channels: 0, BitDepth: 16},
		},
	})
	pipe.Close()

	<-sink.Play(pipe)

	if enc.LastInputSize() != 1920 {
		t.Fatalf("expected 1920 (48kHz default), got %d", enc.LastInputSize())
	}
}

func TestSink_Interrupt_ResetsEncoder(t *testing.T) {
	var resetCalled atomic.Bool
	enc := &resettableEncoder{onReset: func() { resetCalled.Store(true) }}
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, enc)

	pipe := audio.NewPipe[tts.Utterance](8)
	for range 3 {
		pipe.Send(tts.Utterance{
			Frame: audio.Frame{
				Data:   makePCM16Tone(960),
				Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
			},
		})
	}
	pipe.Interrupt()

	<-sink.Play(pipe)

	if !resetCalled.Load() {
		t.Fatal("encoder.Reset should be called on non-EOF stream error")
	}
}

func TestSink_EOF_DoesNotResetEncoder(t *testing.T) {
	var resetCalled atomic.Bool
	enc := &resettableEncoder{onReset: func() { resetCalled.Store(true) }}
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, enc)

	pipe := audio.NewPipe[tts.Utterance](1)
	pipe.Send(tts.Utterance{
		Frame: audio.Frame{
			Data:   makePCM16Tone(960),
			Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
		},
	})
	pipe.Close()

	<-sink.Play(pipe)

	if resetCalled.Load() {
		t.Fatal("encoder.Reset should NOT be called on normal EOF")
	}
}

func TestSink_WriteSample_Duration(t *testing.T) {
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, &compactEncoder{})

	pipe := audio.NewPipe[tts.Utterance](1)
	pipe.Send(tts.Utterance{
		Frame: audio.Frame{
			Data:   makePCM16Tone(960),
			Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
		},
	})
	pipe.Close()

	<-sink.Play(pipe)

	track.mu.Lock()
	defer track.mu.Unlock()
	if len(track.samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(track.samples))
	}
	if track.samples[0].Duration != 20*time.Millisecond {
		t.Fatalf("expected 20ms duration, got %v", track.samples[0].Duration)
	}
}

func TestSink_LargeBuffer_ChunkedCorrectly(t *testing.T) {
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, &compactEncoder{})

	// 48kHz mono: frame = 1920 bytes. Send 10 frames worth = 19200 bytes.
	pipe := audio.NewPipe[tts.Utterance](1)
	pipe.Send(tts.Utterance{
		Frame: audio.Frame{
			Data:   makePCM16Tone(9600), // 19200 bytes
			Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
		},
	})
	pipe.Close()

	<-sink.Play(pipe)

	track.mu.Lock()
	c := track.count
	track.mu.Unlock()
	if c != 10 {
		t.Fatalf("expected 10 chunks for 10-frame buffer, got %d", c)
	}
}

// ---------------------------------------------------------------------------
// Tests — Concurrency & race safety
// ---------------------------------------------------------------------------

func TestTransport_TrackReady_ConcurrentSafe(t *testing.T) {
	tr := newTransport(t)

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			tr.NotifyTrackReady()
		}()
	}
	wg.Wait()

	select {
	case <-tr.TrackReady():
	default:
		t.Fatal("trackReady channel should be closed after NotifyTrackReady")
	}
}

func TestTransport_ConcurrentSendEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	connectPair(t, ctx, server)
	time.Sleep(200 * time.Millisecond)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			server.SendEvent(rtc.ServerMessage{Type: rtc.MessageEvent})
		}(i)
	}
	wg.Wait()
}

func TestTransport_ConcurrentClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	connectPair(t, ctx, server)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_ = server.Close()
		}()
	}
	wg.Wait()
}

func TestTransport_Connect_RaceWithClose(t *testing.T) {
	for range 5 {
		sig := &blockingSignaler{ch: make(chan struct{})}
		tr := newTransport(t, func(cfg *rtc.TransportConfig) { cfg.Signaler = sig })

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = tr.Close()
			cancel()
		}()

		_ = tr.Connect(ctx)
		cancel()
	}
}

func TestTransport_Accept_RaceWithClose(t *testing.T) {
	for range 5 {
		server := newTransport(t)
		clientPC, _ := newClientPC(t)

		offer, _ := clientPC.CreateOffer(nil)
		gatherDone := webrtc.GatheringCompletePromise(clientPC)
		if err := clientPC.SetLocalDescription(offer); err != nil {
			t.Fatalf("SetLocalDescription: %v", err)
		}
		<-gatherDone

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		sdp := clientPC.LocalDescription().SDP

		go func() {
			time.Sleep(10 * time.Millisecond)
			_ = server.Close()
		}()

		_, _ = server.Accept(ctx, rtc.SessionDescription{Type: "offer", SDP: sdp})
		cancel()
	}
}

func TestTransport_ConcurrentSinkAndClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	connectPair(t, ctx, server)

	pipe := audio.NewPipe[tts.Utterance](64)
	go func() {
		for i := range 100 {
			if !pipe.Send(tts.Utterance{
				Frame: audio.Frame{
					Data:   makePCM16Tone(960),
					Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
				},
				Text: fmt.Sprintf("chunk_%d", i),
			}) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		pipe.Close()
	}()

	done := server.Sink().Play(pipe)

	time.Sleep(20 * time.Millisecond)
	_ = server.Close()
	pipe.Interrupt()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Play did not exit after Close+Interrupt")
	}
}

// ---------------------------------------------------------------------------
// Tests — Connection state callbacks
// ---------------------------------------------------------------------------

func TestTransport_ConnectionStateChange_Callback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	states := make(chan rtc.ConnectionState, 10)
	server := newTransport(t, func(cfg *rtc.TransportConfig) {
		cfg.OnConnectionStateChange = func(s rtc.ConnectionState) {
			states <- s
		}
	})

	_, _ = connectPair(t, ctx, server)

	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	var got []rtc.ConnectionState
	for {
		select {
		case s := <-states:
			got = append(got, s)
			if s == rtc.ConnectionStateConnected {
				goto done
			}
		case <-timer.C:
			goto done
		}
	}
done:
	hasConnected := false
	for _, s := range got {
		if s == rtc.ConnectionStateConnected {
			hasConnected = true
		}
	}
	if !hasConnected {
		t.Fatalf("expected ConnectionStateConnected in states, got %v", got)
	}
}

func TestTransport_ConnectionStateFailed_InterruptsSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server := newTransport(t)
	pc, track := connectPair(t, ctx, server)

	_ = track.WriteSample(media.Sample{Data: makePCM16Tone(480), Duration: 10 * time.Millisecond})
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	_, err := readWithTimeout(server.Source().Stream(), readCtx)
	if err != nil {
		t.Fatalf("initial read: %v", err)
	}

	// Force-close the client PC to trigger Failed/Disconnected on server
	_ = pc.Close()

	readCtx2, readCancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel2()
	_, err = readWithTimeout(server.Source().Stream(), readCtx2)
	if err == nil {
		t.Fatal("expected error after peer disconnection")
	}
}

// ---------------------------------------------------------------------------
// Tests — Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkSink_Encode_48kHz(b *testing.B) {
	enc := &compactEncoder{}
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, enc)
	data := makePCM16Tone(960) // one 20ms frame

	b.ResetTimer()
	for b.Loop() {
		pipe := audio.NewPipe[tts.Utterance](1)
		pipe.Send(tts.Utterance{
			Frame: audio.Frame{
				Data:   data,
				Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
			},
		})
		pipe.Close()
		<-sink.Play(pipe)
	}
}

func BenchmarkSink_Encode_LargeBatch(b *testing.B) {
	enc := &compactEncoder{}
	track := &fakeSinkTrack{}
	sink := rtc.NewSinkForTest(track, enc)
	data := makePCM16Tone(48000) // 2 seconds of 48kHz mono

	b.ResetTimer()
	for b.Loop() {
		pipe := audio.NewPipe[tts.Utterance](1)
		pipe.Send(tts.Utterance{
			Frame: audio.Frame{
				Data:   data,
				Format: audio.Format{SampleRate: 48000, Channels: 1, BitDepth: 16},
			},
		})
		pipe.Close()
		<-sink.Play(pipe)
	}
}

func BenchmarkSendEvent(b *testing.B) {
	tr, _ := rtc.NewTransport(rtc.TransportConfig{
		Encoder: &compactEncoder{},
		Decoder: passthroughDecoder{},
	})
	defer func() { _ = tr.Close() }()
	msg := rtc.ServerMessage{Type: rtc.MessageEvent}

	b.ResetTimer()
	for b.Loop() {
		tr.SendEvent(msg)
	}
}

// ---------------------------------------------------------------------------
// Additional test fakes
// ---------------------------------------------------------------------------

type resettableEncoder struct {
	onReset func()
}

func (e *resettableEncoder) Encode(pcm []byte) ([]byte, error) {
	if len(pcm) > 160 {
		return pcm[:160], nil
	}
	return pcm, nil
}

func (e *resettableEncoder) Reset() {
	if e.onReset != nil {
		e.onReset()
	}
}

// Verify interface compliance at compile time.
var (
	_ rtc.Signaler = (*inMemorySignaler)(nil)
	_ rtc.Signaler = (*blockingSignaler)(nil)
	_ io.Reader    = (*strings.Reader)(nil) // keep strings import
)
