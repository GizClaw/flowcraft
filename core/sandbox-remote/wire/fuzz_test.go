package wire_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"
)

// FuzzSpecRoundTrip: arbitrary bytes must either be refused or produce a spec
// that survives a second trip unchanged where the contract says it must.
//
// The property worth pinning is the tri-state one: a decoded spec's Allow is
// either absent or present, and re-encoding must not turn one into the other —
// that would quietly change "inherit nothing" into "inherit the host
// environment" on a policy that crosses a process boundary.
func FuzzSpecRoundTrip(f *testing.F) {
	f.Add(`{"argv":["sh"],"opts":{"env":{"allow":null}}}`)
	f.Add(`{"argv":["sh"],"opts":{"env":{"allow":[]}}}`)
	f.Add(`{"argv":["sh"],"opts":{"env":{"allow":["PATH"]},"timeout_ms":1500}}`)
	f.Add(`{"argv":[],"opts":{"timeout_ms":-1,"stdin":"//8=","permission":{}}}`)
	f.Add(`{"id":"s1","tty":true,"rows":40,"cols":120,"stdin":true}`)

	f.Fuzz(func(t *testing.T, raw string) {
		var decoded wire.Spec
		if err := wire.Decode([]byte(raw), &decoded); err != nil {
			return // refused: a valid answer for arbitrary input
		}
		spec := wire.ToSpec(decoded)

		encoded, err := wire.Encode(wire.FromSpec(spec))
		if err != nil {
			t.Fatalf("Encode(FromSpec(ToSpec(x))): %v", err)
		}
		var again wire.Spec
		if err := wire.Decode(encoded, &again); err != nil {
			t.Fatalf("Decode(Encode(...)) = %v for %s", err, encoded)
		}

		if (decoded.Opts.Env.Allow == nil) != (again.Opts.Env.Allow == nil) {
			t.Fatalf("Allow presence changed: %v then %v", decoded.Opts.Env.Allow, again.Opts.Env.Allow)
		}
		if again.Opts.TimeoutMS < 0 {
			t.Fatalf("a negative timeout survived as %d", again.Opts.TimeoutMS)
		}
		if (decoded.Opts.Permission == nil) != (again.Opts.Permission == nil) {
			t.Fatalf("Permission presence changed")
		}
	})
}

// FuzzErrorRoundTrip: an envelope decoded from arbitrary bytes must either be
// refused or rebuild an error without panicking. The rebuilt error is what a
// caller branches on, so "no panic, always non-nil" is the floor.
func FuzzErrorRoundTrip(f *testing.F) {
	f.Add(`{"kind":"policy_denied","message":"no"}`)
	f.Add(`{"code":"session_closed","message":"closed"}`)
	f.Add(`{"kind":"invented","message":"?"}`)
	f.Add(`{"message":""}`)
	f.Add(`{}`)

	f.Fuzz(func(t *testing.T, raw string) {
		var envelope wire.Error
		if err := wire.Decode([]byte(raw), &envelope); err != nil {
			return
		}
		if err := envelope.Err(); err == nil {
			t.Fatalf("Err() = nil for %s", raw)
		}
	})
}
