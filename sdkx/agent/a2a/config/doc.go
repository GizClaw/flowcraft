// Package config assembles A2A remote-proxy engines for deployment
// documents: it implements the sdk/config.Factory protocol for the "a2a"
// engine kind and is registered with sdkx/deploy via
// [Builder.RegisterEngine].
//
// # Settings
//
// The engine settings subtree accepts:
//
//	engine:
//	  kind: a2a
//	  settings:
//	    url: https://peer.example/a2a        # explicit JSON-RPC endpoint
//	    # card_url: https://peer.example     # discover via /.well-known/agent-card.json
//	    # card: { ...inline AgentCard... }   # or inline the whole card
//	    protocol: auto                        # auto|0.3|1.0 (url path only)
//	    auth: {scheme: bearer, token: "..."}  # bearer|basic|custom
//	    headers: {X-Tenant: acme}             # extra static headers
//	    stream: auto                          # auto|on|off
//	    poll_interval: 1s
//	    history_length: 0
//	    accepted_output_modes: [text/plain]
//	    preferred_transports: [jsonrpc, grpc]
//
// Unknown keys are rejected by strict decoding; durations use Go duration
// strings.
package config
