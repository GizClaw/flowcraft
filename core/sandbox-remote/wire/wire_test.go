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

// The read frame carries raw bytes, so a chunk that is not valid UTF-8 (a TTY, a
// binary command) needs no escaping — and decoding points into the body instead of
// copying it.
func TestReadFrameRoundTrip(t *testing.T) {
	want := sandbox.SessionOutput{
		NextSeq: 42,
		Chunks: []sandbox.OutputChunk{
			{Seq: 40, Stream: sandbox.SessionStreamStdout, Data: []byte{0xff, 0xfe, 0x00}},
			{Seq: 41, Stream: sandbox.SessionStreamStderr, Data: []byte("plain")},
		},
		EOF: true,
	}

	body := wire.AppendReadResponse(nil, want, false)
	if size := wire.ReadResponseSize(want, false); size != len(body) {
		t.Fatalf("ReadResponseSize = %d, encoded %d", size, len(body))
	}

	got, idle, err := wire.DecodeReadResponse(body)
	if err != nil {
		t.Fatalf("DecodeReadResponse: %v", err)
	}
	if idle {
		t.Fatal("a real batch decoded as idle")
	}
	if got.NextSeq != want.NextSeq || got.EOF != want.EOF || len(got.Chunks) != len(want.Chunks) {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
	for i := range want.Chunks {
		if got.Chunks[i].Seq != want.Chunks[i].Seq ||
			got.Chunks[i].Stream != want.Chunks[i].Stream ||
			!reflect.DeepEqual(got.Chunks[i].Data, want.Chunks[i].Data) {
			t.Fatalf("chunk %d = %+v, want %+v", i, got.Chunks[i], want.Chunks[i])
		}
	}
}

// The server writes the header and then each chunk straight to the connection, so
// the piecewise form has to produce exactly what the one-shot form does.
func TestReadFramePiecewiseMatchesOneShot(t *testing.T) {
	out := sandbox.SessionOutput{
		NextSeq: 9,
		Chunks: []sandbox.OutputChunk{
			{Seq: 0, Stream: sandbox.SessionStreamStdout, Data: []byte("ab")},
			{Seq: 2, Stream: sandbox.SessionStreamTTY, Data: []byte("cde")},
		},
	}
	oneShot := wire.AppendReadResponse(nil, out, false)

	piecewise := wire.AppendReadHeader(nil, out, false)
	for _, chunk := range out.Chunks {
		piecewise = wire.AppendChunkHeader(piecewise, chunk)
		piecewise = append(piecewise, chunk.Data...)
	}
	if !reflect.DeepEqual(piecewise, oneShot) {
		t.Fatalf("piecewise %v != one-shot %v", piecewise, oneShot)
	}
	if size := wire.ReadResponseSize(out, false); size != len(piecewise) {
		t.Fatalf("ReadResponseSize = %d, want %d (what Content-Length promises)", size, len(piecewise))
	}
}

func TestReadFrameIdle(t *testing.T) {
	body := wire.AppendReadResponse(nil, sandbox.SessionOutput{NextSeq: 7}, true)
	if size := wire.ReadResponseSize(sandbox.SessionOutput{NextSeq: 7}, true); size != len(body) {
		t.Fatalf("idle size = %d, encoded %d", size, len(body))
	}
	got, idle, err := wire.DecodeReadResponse(body)
	if err != nil {
		t.Fatalf("DecodeReadResponse: %v", err)
	}
	if !idle || got.NextSeq != 7 || len(got.Chunks) != 0 || got.EOF {
		t.Fatalf("idle frame decoded as %+v (idle=%v)", got, idle)
	}
}

// A response that is not ours has to be refused, not parsed into nonsense chunk
// lengths: a proxy's HTML error page and a truncated body both land here.
func TestReadFrameRejectsMalformed(t *testing.T) {
	good := wire.AppendReadResponse(nil, sandbox.SessionOutput{
		NextSeq: 3,
		Chunks:  []sandbox.OutputChunk{{Seq: 0, Stream: sandbox.SessionStreamStdout, Data: []byte("abc")}},
	}, false)

	cases := map[string][]byte{
		"empty":            nil,
		"short header":     good[:5],
		"wrong magic":      append([]byte("HTML"), good[4:]...),
		"truncated chunk":  good[:len(good)-1],
		"truncated header": good[:len(good)-4],
		"trailing bytes":   append(append([]byte(nil), good...), 0x00),
		"impossible count": func() []byte {
			bad := append([]byte(nil), good...)
			bad[5], bad[6], bad[7], bad[8] = 0xff, 0xff, 0x00, 0x00
			return bad
		}(),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := wire.DecodeReadResponse(body); err == nil {
				t.Fatal("malformed frame decoded without an error")
			} else if !errdefs.IsInternal(err) {
				t.Fatalf("err = %v, want an internal classification", err)
			}
		})
	}
}

// FuzzDecodeReadResponse: arbitrary bytes either decode or are refused; nothing
// panics, and a decoded frame never reports chunk data outside the body.
func FuzzDecodeReadResponse(f *testing.F) {
	f.Add(wire.AppendReadResponse(nil, sandbox.SessionOutput{
		NextSeq: 3,
		Chunks:  []sandbox.OutputChunk{{Seq: 0, Stream: sandbox.SessionStreamStdout, Data: []byte("abc")}},
	}, false))
	f.Add(wire.AppendReadResponse(nil, sandbox.SessionOutput{NextSeq: 1}, true))
	f.Add([]byte("FSR1"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, body []byte) {
		out, idle, err := wire.DecodeReadResponse(body)
		if err != nil {
			return
		}
		if idle && len(out.Chunks) != 0 {
			t.Fatalf("idle frame carried %d chunks", len(out.Chunks))
		}
		total := 0
		for _, c := range out.Chunks {
			total += len(c.Data)
		}
		if total > len(body) {
			t.Fatalf("decoded %d bytes of data from a %d-byte body", total, len(body))
		}
	})
}
