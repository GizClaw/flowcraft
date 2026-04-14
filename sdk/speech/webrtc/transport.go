package webrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"
)

// TransportConfig configures a WebRTC transport.
type TransportConfig struct {
	// ICEServers for NAT traversal. Empty = rely on host candidates only.
	ICEServers []ICEServer

	// Encoder encodes PCM to Opus for the outbound track (TTS → client).
	// Required.
	Encoder AudioEncoder

	// Decoder decodes Opus to PCM from the inbound track (client → STT).
	// Required.
	Decoder AudioDecoder

	// Source configures the PCM format metadata emitted by the Source.
	// Defaults to 48kHz mono 16-bit if zero-valued.
	Source SourceConfig

	// Signaler handles SDP exchange. Required only for Connect() (client mode).
	// For Accept() (server mode), the SDP exchange is driven directly by the
	// HTTP handler and Signaler is not used.
	Signaler Signaler

	// OnConnectionStateChange is called when the peer connection state changes.
	OnConnectionStateChange func(state ConnectionState)

	// OnControlMessage is called when a control message is received from the
	// remote peer via the DataChannel. The callback receives the raw JSON.
	OnControlMessage func(msg ClientMessage)
}

// Transport manages one WebRTC PeerConnection for bidirectional audio
// and a DataChannel for control messages.
type Transport struct {
	mu     sync.Mutex
	pc     *webrtc.PeerConnection
	config TransportConfig
	source *Source
	sink   *Sink
	dc     *webrtc.DataChannel
	closed chan struct{}

	trackReady     chan struct{} // closed when remote audio track is received
	trackReadyOnce sync.Once
}

// NewTransport creates a Transport but does not start the connection.
// Call Accept() or Connect() to begin the signaling handshake.
func NewTransport(config TransportConfig) (*Transport, error) {
	if config.Encoder == nil {
		return nil, fmt.Errorf("webrtc: Encoder is required")
	}
	if config.Decoder == nil {
		return nil, fmt.Errorf("webrtc: Decoder is required")
	}
	return &Transport{
		config:     config,
		source:     newSource(config.Decoder, config.Source),
		closed:     make(chan struct{}),
		trackReady: make(chan struct{}),
	}, nil
}

// Accept handles the server-side flow: receives a remote SDP offer (typically
// from an HTTP handler), creates an answer, and returns it. The caller is
// responsible for delivering the answer back to the client.
func (t *Transport) Accept(ctx context.Context, offer SessionDescription) (SessionDescription, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.initPC(false); err != nil {
		return SessionDescription{}, err
	}

	if err := t.pc.SetRemoteDescription(toWebRTCSD(offer)); err != nil {
		return SessionDescription{}, fmt.Errorf("webrtc: set remote description: %w", err)
	}

	answer, err := t.pc.CreateAnswer(nil)
	if err != nil {
		return SessionDescription{}, fmt.Errorf("webrtc: create answer: %w", err)
	}

	gatherDone := webrtc.GatheringCompletePromise(t.pc)
	if err := t.pc.SetLocalDescription(answer); err != nil {
		return SessionDescription{}, fmt.Errorf("webrtc: set local description: %w", err)
	}

	select {
	case <-gatherDone:
	case <-ctx.Done():
		return SessionDescription{}, ctx.Err()
	}

	localDesc := t.pc.LocalDescription()
	return fromWebRTCSD(*localDesc), nil
}

// Connect handles the client-side flow: creates an SDP offer, exchanges it
// via Signaler, and waits for the ICE connection to be established.
func (t *Transport) Connect(ctx context.Context) error {
	t.mu.Lock()
	if t.config.Signaler == nil {
		t.mu.Unlock()
		return fmt.Errorf("webrtc: Signaler is required for Connect()")
	}
	if err := t.initPC(true); err != nil {
		t.mu.Unlock()
		return err
	}
	t.mu.Unlock()

	offer, err := t.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("webrtc: create offer: %w", err)
	}

	gatherDone := webrtc.GatheringCompletePromise(t.pc)
	if err := t.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("webrtc: set local description: %w", err)
	}

	select {
	case <-gatherDone:
	case <-ctx.Done():
		return ctx.Err()
	}

	localDesc := t.pc.LocalDescription()
	answer, err := t.config.Signaler.Exchange(ctx, fromWebRTCSD(*localDesc))
	if err != nil {
		return fmt.Errorf("webrtc: signaler exchange: %w", err)
	}

	if err := t.pc.SetRemoteDescription(toWebRTCSD(answer)); err != nil {
		return fmt.Errorf("webrtc: set remote description: %w", err)
	}

	return nil
}

// Source returns the AudioSource that reads from the remote audio track.
func (t *Transport) Source() *Source {
	return t.source
}

// Sink returns the AudioSink that writes to the local audio track.
// Returns nil until Accept or Connect completes successfully.
func (t *Transport) Sink() *Sink {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sink
}

// SendEvent sends a server message to the remote peer via the DataChannel.
// Returns false if the DataChannel is not ready.
func (t *Transport) SendEvent(msg ServerMessage) bool {
	t.mu.Lock()
	dc := t.dc
	t.mu.Unlock()

	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	return dc.Send(data) == nil
}

// Close tears down the PeerConnection and releases all resources.
// Interrupts Source.pipe so any active Session.Run exits promptly.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	select {
	case <-t.closed:
		return nil
	default:
		close(t.closed)
	}

	t.source.pipe.Interrupt()

	if t.pc != nil {
		return t.pc.Close()
	}
	return nil
}

// initPC creates the PeerConnection, local audio track, and registers
// callbacks. Must be called with t.mu held.
// asOfferer controls DataChannel setup: offerers create it, answerers listen.
func (t *Transport) initPC(asOfferer bool) error {
	if t.pc != nil {
		return nil
	}

	iceServers := make([]webrtc.ICEServer, len(t.config.ICEServers))
	for i, s := range t.config.ICEServers {
		iceServers[i] = webrtc.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		}
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		return fmt.Errorf("webrtc: new peer connection: %w", err)
	}

	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "flowcraft",
	)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("webrtc: new audio track: %w", err)
	}

	if _, err := pc.AddTrack(audioTrack); err != nil {
		_ = pc.Close()
		return fmt.Errorf("webrtc: add track: %w", err)
	}

	t.sink = newSink(audioTrack, t.config.Encoder)

	if asOfferer {
		dc, err := pc.CreateDataChannel("control", &webrtc.DataChannelInit{
			Ordered: boolPtr(true),
		})
		if err != nil {
			_ = pc.Close()
			return fmt.Errorf("webrtc: create data channel: %w", err)
		}
		// Caller (Connect) already holds t.mu, so assign directly
		// and only register the OnMessage callback.
		t.dc = dc
		t.bindDataChannelMessage(dc)
	} else {
		pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			t.setupDataChannel(dc)
		})
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		go t.source.readLoop(track)
		t.trackReadyOnce.Do(func() { close(t.trackReady) })
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		cs := mapConnectionState(state)
		if t.config.OnConnectionStateChange != nil {
			t.config.OnConnectionStateChange(cs)
		}
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateDisconnected ||
			state == webrtc.PeerConnectionStateClosed {
			t.source.pipe.Interrupt()
		}
	})

	t.pc = pc
	return nil
}

// setupDataChannel stores the DataChannel reference (with locking) and
// registers the OnMessage callback. Called from OnDataChannel callbacks
// where t.mu is NOT held.
func (t *Transport) setupDataChannel(dc *webrtc.DataChannel) {
	t.mu.Lock()
	t.dc = dc
	t.mu.Unlock()

	t.bindDataChannelMessage(dc)
}

// bindDataChannelMessage registers the OnMessage callback on dc.
// Must NOT acquire t.mu — safe to call with or without the lock held.
func (t *Transport) bindDataChannelMessage(dc *webrtc.DataChannel) {
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if t.config.OnControlMessage == nil {
			return
		}
		var clientMsg ClientMessage
		if json.Unmarshal(msg.Data, &clientMsg) == nil {
			t.config.OnControlMessage(clientMsg)
		}
	})
}

func toWebRTCSD(sd SessionDescription) webrtc.SessionDescription {
	sdType := webrtc.SDPTypeOffer
	if sd.Type == "answer" {
		sdType = webrtc.SDPTypeAnswer
	}
	return webrtc.SessionDescription{Type: sdType, SDP: sd.SDP}
}

func fromWebRTCSD(sd webrtc.SessionDescription) SessionDescription {
	t := "offer"
	if sd.Type == webrtc.SDPTypeAnswer {
		t = "answer"
	}
	return SessionDescription{Type: t, SDP: sd.SDP}
}

func mapConnectionState(state webrtc.PeerConnectionState) ConnectionState {
	switch state {
	case webrtc.PeerConnectionStateNew:
		return ConnectionStateNew
	case webrtc.PeerConnectionStateConnecting:
		return ConnectionStateConnecting
	case webrtc.PeerConnectionStateConnected:
		return ConnectionStateConnected
	case webrtc.PeerConnectionStateDisconnected:
		return ConnectionStateDisconnected
	case webrtc.PeerConnectionStateFailed:
		return ConnectionStateFailed
	case webrtc.PeerConnectionStateClosed:
		return ConnectionStateClosed
	default:
		return ConnectionStateNew
	}
}

func boolPtr(v bool) *bool { return &v }
