package wire_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"
)

// roundTrip sends spec through the encoder and decoder, then back to the
// contract type.
func roundTrip(t *testing.T, spec sandbox.SessionSpec) sandbox.SessionSpec {
	t.Helper()
	encoded, err := wire.Encode(wire.FromSpec(spec))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded wire.Spec
	if err := wire.Decode(encoded, &decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return wire.ToSpec(decoded)
}

// The reason the request side is mirrored at all: nil Allow is "inherit the host
// environment" and an empty non-nil Allow is "inherit nothing". A plain slice
// field would collapse them after a JSON round trip, silently turning the
// stricter policy into the looser one.
func TestSpecRoundTripKeepsEnvTriState(t *testing.T) {
	cases := map[string]struct {
		allow []string
		want  []string // nil means "must stay nil"
	}{
		"inherit everything": {allow: nil, want: nil},
		"inherit nothing":    {allow: []string{}, want: []string{}},
		"allow-list":         {allow: []string{"PATH", "HOME"}, want: []string{"PATH", "HOME"}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			spec := sandbox.SessionSpec{Opts: sandbox.ExecOptions{
				Env: sandbox.EnvPolicy{Allow: tc.allow, Inject: map[string]string{"A": "b"}},
			}}
			got := roundTrip(t, spec)
			if got.Opts.Env.Allow == nil {
				if tc.want != nil {
					t.Fatalf("Allow = nil, want %v", tc.want)
				}
				return
			}
			if tc.want == nil {
				t.Fatalf("Allow = %v, want nil (inherit everything)", got.Opts.Env.Allow)
			}
			if !reflect.DeepEqual(got.Opts.Env.Allow, tc.want) {
				t.Fatalf("Allow = %v, want %v", got.Opts.Env.Allow, tc.want)
			}
		})
	}
}

func TestSpecRoundTrip(t *testing.T) {
	policy := &permission.Policy{
		Writes: permission.Writes{Paths: []string{"/work/out"}},
		Net: permission.NetPolicy{
			Mode:       permission.NetAllowList,
			AllowHosts: []string{"api.example.com:443"},
		},
	}
	spec := sandbox.SessionSpec{
		ID:    "s1",
		Argv:  []string{"sh", "-c", "echo hi"},
		TTY:   true,
		Rows:  40,
		Cols:  120,
		Stdin: true,
		Opts: sandbox.ExecOptions{
			WorkDir: "/work",
			// Deliberately not valid UTF-8: the field carries bytes, not text.
			Stdin:      []byte{0xff, 0x00, 0xfe},
			Timeout:    1500 * time.Millisecond,
			Env:        sandbox.EnvPolicy{Allow: []string{"PATH"}, Inject: map[string]string{"K": "v"}},
			Net:        policy.Net,
			Permission: policy,
			Resources:  sandbox.ResourceLimits{MemoryBytes: 1 << 20, MaxOutputBytes: 4096},
		},
	}

	got := roundTrip(t, spec)
	if got.ID != spec.ID || got.TTY != spec.TTY || got.Rows != spec.Rows || got.Cols != spec.Cols || got.Stdin != spec.Stdin {
		t.Fatalf("session fields did not survive: %+v", got)
	}
	if !reflect.DeepEqual(got.Argv, spec.Argv) {
		t.Fatalf("Argv = %v, want %v", got.Argv, spec.Argv)
	}
	if got.Opts.WorkDir != spec.Opts.WorkDir {
		t.Fatalf("WorkDir = %q", got.Opts.WorkDir)
	}
	if !reflect.DeepEqual(got.Opts.Stdin, spec.Opts.Stdin) {
		t.Fatalf("Stdin bytes = %v, want %v", got.Opts.Stdin, spec.Opts.Stdin)
	}
	if got.Opts.Timeout != spec.Opts.Timeout {
		t.Fatalf("Timeout = %v, want %v", got.Opts.Timeout, spec.Opts.Timeout)
	}
	if !reflect.DeepEqual(got.Opts.Env, spec.Opts.Env) {
		t.Fatalf("Env = %+v, want %+v", got.Opts.Env, spec.Opts.Env)
	}
	if !reflect.DeepEqual(got.Opts.Net, spec.Opts.Net) {
		t.Fatalf("Net = %+v, want %+v", got.Opts.Net, spec.Opts.Net)
	}
	if !reflect.DeepEqual(got.Opts.Permission, spec.Opts.Permission) {
		t.Fatalf("Permission = %+v, want %+v", got.Opts.Permission, spec.Opts.Permission)
	}
	if !reflect.DeepEqual(got.Opts.Resources, spec.Opts.Resources) {
		t.Fatalf("Resources = %+v", got.Opts.Resources)
	}
}

// A nil permission means "keep the runner's boundary"; a non-nil one replaces it.
// That distinction is a pointer on both sides and must survive.
func TestSpecRoundTripKeepsPermissionPresence(t *testing.T) {
	if got := roundTrip(t, sandbox.SessionSpec{}); got.Opts.Permission != nil {
		t.Fatalf("Permission = %+v, want nil", got.Opts.Permission)
	}
	empty := &permission.Policy{}
	got := roundTrip(t, sandbox.SessionSpec{Opts: sandbox.ExecOptions{Permission: empty}})
	if got.Opts.Permission == nil {
		t.Fatal("Permission = nil, want a present (read-only) policy")
	}
}

// A negative deadline is not a deadline. It arrives as zero rather than as a
// value every backend would have to reinterpret.
func TestToSpecNegativeTimeoutIsUnset(t *testing.T) {
	got := wire.ToSpec(wire.Spec{Opts: wire.Options{TimeoutMS: -5}})
	if got.Opts.Timeout != 0 {
		t.Fatalf("Timeout = %v, want 0", got.Opts.Timeout)
	}
}

func TestHelloCarriesTheServerDeclaration(t *testing.T) {
	hello := wire.Hello{
		Version: wire.Version,
		Server:  "sandboxd/1",
		Capabilities: sandbox.Capabilities{
			Policy: sandbox.Enforcement{
				EnvAllowList: true,
				NetModes:     permission.IsolatingNetModes(),
				MemoryCap:    true,
			},
			Features: sandbox.SessionFeatures{TTY: true, Signal: true, Events: true},
			Journal:  sandbox.JournalCapabilities{Enabled: true, WatchBudget: 4096},
		},
	}
	encoded, err := wire.Encode(hello)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var got wire.Hello
	if err := wire.Decode(encoded, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Version != wire.Version || got.Server != hello.Server {
		t.Fatalf("hello = %+v", got)
	}
	if !reflect.DeepEqual(got.Capabilities, hello.Capabilities) {
		t.Fatalf("capabilities = %+v, want %+v", got.Capabilities, hello.Capabilities)
	}
}

// Additions must be compatible: an old client drops a field it does not know
// instead of refusing the message.
func TestDecodeIgnoresUnknownFields(t *testing.T) {
	var spec wire.Spec
	if err := wire.Decode([]byte(`{"argv":["sh"],"future_field":{"x":1}}`), &spec); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(spec.Argv, []string{"sh"}) {
		t.Fatalf("Argv = %v", spec.Argv)
	}
}

func TestErrorRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// checks run against the rebuilt error.
		checks []func(error) bool
	}{
		{
			name:   "classified",
			err:    errdefs.PolicyDeniedf("command %q is not allowed", "rm"),
			checks: []func(error) bool{errdefs.IsPolicyDenied},
		},
		{
			name:   "not available",
			err:    errdefs.NotAvailablef("net mode %d", 2),
			checks: []func(error) bool{errdefs.IsNotAvailable},
		},
		{
			name:   "plain",
			err:    errors.New("something broke"),
			checks: []func(error) bool{func(err error) bool { return errdefs.KindOf(err) == errdefs.KindNone }},
		},
		{
			name:   "session closed sentinel",
			err:    sandbox.ErrSessionClosed,
			checks: []func(error) bool{func(err error) bool { return errors.Is(err, sandbox.ErrSessionClosed) }},
		},
		{
			name:   "sequence gap sentinel",
			err:    sandbox.ErrSequenceGap,
			checks: []func(error) bool{func(err error) bool { return errors.Is(err, sandbox.ErrSequenceGap) }},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := wire.Encode(wire.FromError(tc.err))
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			var decoded wire.Error
			if err := wire.Decode(encoded, &decoded); err != nil {
				t.Fatalf("Decode: %v", err)
			}
			rebuilt := decoded.Err()
			if rebuilt == nil {
				t.Fatal("Err() = nil")
			}
			// The sender's text survives verbatim, so a log on the client side
			// still says what the server said.
			if rebuilt.Error() != tc.err.Error() {
				t.Errorf("text = %q, want %q", rebuilt.Error(), tc.err.Error())
			}
			for i, check := range tc.checks {
				if !check(rebuilt) {
					t.Errorf("check %d failed for %v", i, rebuilt)
				}
			}
		})
	}
}

func TestErrorRoundTripNil(t *testing.T) {
	if wire.FromError(nil) != nil {
		t.Fatal("FromError(nil) != nil")
	}
	var e *wire.Error
	if err := e.Err(); err != nil {
		t.Fatalf("(*Error)(nil).Err() = %v, want nil", err)
	}
}

// A kind this build does not know is a protocol mismatch, and saying so beats
// reporting the failure as unclassified.
func TestErrorUnknownKind(t *testing.T) {
	err := (&wire.Error{Kind: "invented", Message: "boom"}).Err()
	if err == nil {
		t.Fatal("Err() = nil")
	}
	if !strings.Contains(err.Error(), "invented") {
		t.Fatalf("Err() = %q, want it to name the unknown kind", err.Error())
	}
}

// Output crosses the wire as bytes: a chunk's Data is base64 in JSON on both
// sides, so a stream that is not valid UTF-8 (a TTY, a binary command) survives.
func TestReadResponseRoundTrip(t *testing.T) {
	resp := wire.ReadResponse{Output: sandbox.SessionOutput{
		NextSeq: 42,
		Chunks: []sandbox.OutputChunk{
			{Seq: 40, Stream: sandbox.SessionStreamStdout, Data: []byte{0xff, 0xfe, 0x00}},
			{Seq: 41, Stream: sandbox.SessionStreamStderr, Data: []byte("plain")},
		},
		EOF: true,
	}}
	encoded, err := wire.Encode(resp)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var got wire.ReadResponse
	if err := wire.Decode(encoded, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, resp) {
		t.Fatalf("round trip = %+v, want %+v", got, resp)
	}
}
