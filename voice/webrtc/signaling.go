package webrtc

import "context"

// Signaler abstracts the SDP exchange mechanism used by Transport.Connect
// (client mode). Server mode (Transport.Accept) does not use Signaler — the
// HTTP handler drives the SDP exchange directly.
//
// Implementations may use WHIP/WHEP, Volcengine WTN HTTP API, or any
// custom signaling channel.
type Signaler interface {
	// Exchange sends a local SDP offer and returns the remote SDP answer.
	Exchange(ctx context.Context, local SessionDescription) (SessionDescription, error)

	// OnICECandidate registers a callback for trickle ICE candidates
	// from the remote peer. Implementations that use full ICE gathering
	// (candidates embedded in SDP) may ignore this.
	OnICECandidate(func(candidate ICECandidate))

	// AddICECandidate delivers a local ICE candidate to the remote peer.
	// Implementations that use full ICE gathering may ignore this.
	AddICECandidate(ctx context.Context, candidate ICECandidate) error

	// Close releases signaling resources (HTTP connections, etc.).
	Close() error
}

// SessionDescription wraps an SDP with its type.
// Decoupled from pion/webrtc types at the interface boundary.
type SessionDescription struct {
	Type string `json:"type"` // "offer" or "answer"
	SDP  string `json:"sdp"`
}

// ICECandidate represents a trickle ICE candidate.
type ICECandidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        string  `json:"sdp_mid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdp_mline_index,omitempty"`
}

// ICEServer describes a STUN/TURN server for NAT traversal.
type ICEServer struct {
	URLs       []string
	Username   string
	Credential string
}

// ConnectionState represents the state of a WebRTC peer connection.
type ConnectionState int

const (
	ConnectionStateNew ConnectionState = iota
	ConnectionStateConnecting
	ConnectionStateConnected
	ConnectionStateDisconnected
	ConnectionStateFailed
	ConnectionStateClosed
)
