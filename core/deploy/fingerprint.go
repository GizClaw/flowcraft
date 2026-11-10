package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// deploymentFingerprint is the identity a checkpoint of this deployment records.
//
// The runtime configuration is only part of what makes a session what it is:
// the same model and tools reached through a different peer, or exposed through a
// different protocol, is a different deployment entity. Resuming across either
// would continue a transcript under conditions it was not produced under.
//
// The card is deliberately absent: it is an advertisement, and fixing a typo in
// a description must not invalidate sessions.
func deploymentFingerprint(runtimeDigest runtime.Digest, engineKind, engineSettings, protocolKind string) agent.Fingerprint {
	// The separator cannot appear in a component or a kind, so the fields cannot
	// collide into the same bytes as a different combination.
	payload := strings.Join([]string{
		string(runtimeDigest), engineKind, engineSettings, protocolKind,
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return agent.Fingerprint("sha256:" + hex.EncodeToString(sum[:]))
}
