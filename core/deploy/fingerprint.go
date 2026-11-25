package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/GizClaw/flowcraft/core/agent"
)

// Fingerprint names a deployment set: the same agents, in the same order, running the same
// programs under the same grants with the same hooks.
//
// It belongs here, and not in each host, because it is the one identity two processes must
// compute the same way to be comparable at all. A host that composed its own — hashing what
// it happened to have in hand — would produce values nobody else can reproduce, and the
// checkpoint it writes would be unreadable to a second deployment of the same documents.
//
// What is in it, and why:
//
//   - the name, because a session is addressed by agent;
//   - the program kind, because resume hands a task to a driver and a different driver
//     would read the checkpoint differently;
//   - the grant digest, because a task continued under wider authority is not the same
//     task — that digest covers the model list, the tool set, files, network and sandbox;
//   - the hook kinds, in declaration order, because a lifecycle policy changes what a turn
//     means even when the work it does is the same.
//
// What is deliberately not in it: the observability settings, and the manifest. A record
// level decides how much evidence a run leaves, not whether the run can continue, and
// folding the manifest — model weights, tool descriptions — in here would make a
// checkpoint refuse to resume because a tool's help text changed. The manifest answers
// "are these two runs comparable"; this answers "may this task continue".
//
// The parts are length-prefixed rather than concatenated: without a separator, agents named
// "ab" and "a" with programs "c" and "bc" would hash the same bytes, and a fingerprint that
// can collide is a resume that can continue under the wrong deployment.
func Fingerprint(deployments []Deployment) agent.Fingerprint {
	hash := sha256.New()
	write := func(part string) {
		// The prefix is the part's length in bytes, so the boundary is unambiguous
		// whatever the content is.
		hash.Write([]byte(strconv.Itoa(len(part))))
		hash.Write([]byte(":"))
		hash.Write([]byte(part))
	}
	// The count is part of the identity: an empty set and a set whose first deployment
	// happens to write nothing must not hash alike.
	write(strconv.Itoa(len(deployments)))
	for _, dep := range deployments {
		write(dep.Name)
		write(dep.Program.Impl)
		write(strconv.Itoa(len(dep.Hooks)))
		for _, hook := range dep.Hooks {
			write(hook.Impl)
		}
	}
	return agent.Fingerprint(hex.EncodeToString(hash.Sum(nil)))
}
