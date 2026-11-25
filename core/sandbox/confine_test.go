package sandbox_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// fullCaps claims every dimension the contract knows, so a refusal below can
// never be blamed on the caps: if the gate refuses with fullCaps, it refuses
// because the request itself is wrong.
func fullCaps() sandbox.Enforcement {
	return sandbox.Enforcement{
		EnvAllowList:     true,
		NetModes:         []policy.NetMode{policy.NetDenyAll, policy.NetAllowList, policy.NetProxy},
		Socks5:           true,
		MITM:             true,
		UnixSocketPolicy: true,
		MemoryCap:        true,
		CPUCap:           true,
		DiskCap:          true,
		FilesystemBounds: true,
	}
}

// TestValidateConfineAcceptsWhatTheBackendClaims is the "no false refusal" half:
// a backend that advertises a dimension must be allowed to serve it, or the
// declaration means nothing.
func TestValidateConfineAcceptsWhatTheBackendClaims(t *testing.T) {
	caps := fullCaps()
	for name, confine := range map[string]sandbox.Confine{
		"empty":            {},
		"net deny all":     {Net: policy.NetPolicy{Mode: policy.NetDenyAll}},
		"net allow list":   {Net: policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"api.example.com"}}},
		"net proxy":        {Net: policy.NetPolicy{Mode: policy.NetProxy, Proxy: "http://127.0.0.1:8080"}},
		"mitm":             {Net: policy.NetPolicy{Mode: policy.NetAllowList, MITM: &policy.MITMPolicy{Enabled: true}}},
		"unix sockets":     {Net: policy.NetPolicy{UnixSockets: []string{"/run/x.sock"}}},
		"env allow-list":   {Env: policy.EnvPolicy{Allow: []string{"PATH"}}},
		"memory cap":       {Resources: policy.Resources{MemoryBytes: 64 << 20}},
		"cpu cap":          {Resources: policy.Resources{CPUMillicores: 500}},
		"disk cap":         {Resources: policy.Resources{DiskBytes: 1 << 30}},
		"result cap alone": {Resources: policy.Resources{MaxOutputBytes: 1 << 10}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := sandbox.ValidateConfine(confine, caps); err != nil {
				t.Fatalf("gate refused a dimension the backend claims: %v", err)
			}
		})
	}
}

// TestValidateConfineRefusesWhatItDoesNotClaim is the other half: every dimension
// is gated on its own capability, so one cannot be traded for another.
func TestValidateConfineRefusesWhatItDoesNotClaim(t *testing.T) {
	for name, tc := range map[string]struct {
		confine sandbox.Confine
		caps    sandbox.Enforcement
	}{
		"net mode":       {sandbox.Confine{Net: policy.NetPolicy{Mode: policy.NetDenyAll}}, sandbox.Enforcement{}},
		"mitm":           {sandbox.Confine{Net: policy.NetPolicy{MITM: &policy.MITMPolicy{Enabled: true}}}, sandbox.Enforcement{}},
		"unix sockets":   {sandbox.Confine{Net: policy.NetPolicy{UnixSockets: []string{"/run/x.sock"}}}, sandbox.Enforcement{}},
		"env allow-list": {sandbox.Confine{Env: policy.EnvPolicy{Allow: []string{"PATH"}}}, sandbox.Enforcement{}},
		"memory cap":     {sandbox.Confine{Resources: policy.Resources{MemoryBytes: 1}}, sandbox.Enforcement{}},
		"cpu cap":        {sandbox.Confine{Resources: policy.Resources{CPUMillicores: 1}}, sandbox.Enforcement{}},
		"disk cap":       {sandbox.Confine{Resources: policy.Resources{DiskBytes: 1}}, sandbox.Enforcement{}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := sandbox.ValidateConfine(tc.confine, tc.caps); !errdefs.IsNotAvailable(err) {
				t.Fatalf("err = %v, want NotAvailable", err)
			}
		})
	}
}

// TestValidateConfinePassesTheDefaultPostureOnAnyBackend keeps "no policy" out of
// the gate: NetDefault is the absence of a posture, not one a backend has to
// support.
func TestValidateConfinePassesTheDefaultPostureOnAnyBackend(t *testing.T) {
	confine := sandbox.Confine{Net: policy.NetPolicy{Mode: policy.NetDefault}}
	if err := sandbox.ValidateConfine(confine, sandbox.Enforcement{}); err != nil {
		t.Fatalf("NetDefault was gated: %v", err)
	}
}

// TestNarrowInheritsTheInstanceCeiling is the reason Narrow returns options
// instead of only validating them: a call that names no budget still runs under
// the instance's caps.
func TestNarrowInheritsTheInstanceCeiling(t *testing.T) {
	confine := sandbox.Confine{Resources: policy.Resources{
		CPUMillicores:  500,
		MemoryBytes:    64 << 20,
		MaxOutputBytes: 1 << 20,
	}}
	got, err := confine.Narrow(sandbox.ExecOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Narrow: %v", err)
	}
	if got.Resources != confine.Resources {
		t.Fatalf("Resources = %+v, want the instance ceiling %+v", got.Resources, confine.Resources)
	}
}

func TestNarrowKeepsWhatFitsAndRefusesWhatDoesNot(t *testing.T) {
	confine := sandbox.Confine{Resources: policy.Resources{MemoryBytes: 64 << 20, MaxOutputBytes: 1 << 20}}

	fits := sandbox.ExecOptions{Resources: policy.Resources{MemoryBytes: 32 << 20, MaxOutputBytes: 1 << 20}}
	got, err := confine.Narrow(fits)
	if err != nil {
		t.Fatalf("Narrow: %v", err)
	}
	if got.Resources != fits.Resources {
		t.Fatalf("Resources = %+v, want the request %+v", got.Resources, fits.Resources)
	}

	for name, over := range map[string]policy.Resources{
		"memory":     {MemoryBytes: 128 << 20},
		"max output": {MaxOutputBytes: 2 << 20},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := confine.Narrow(sandbox.ExecOptions{Resources: over})
			if !errdefs.IsNotAvailable(err) {
				t.Fatalf("err = %v, want NotAvailable", err)
			}
		})
	}
}

// TestNarrowAllowsACapOnAnUnboundedInstance keeps the direction straight: adding a
// cap where the instance has none is narrowing, not widening.
func TestNarrowAllowsACapOnAnUnboundedInstance(t *testing.T) {
	got, err := (sandbox.Confine{}).Narrow(sandbox.ExecOptions{
		Resources: policy.Resources{MemoryBytes: 32 << 20},
	})
	if err != nil {
		t.Fatalf("Narrow: %v", err)
	}
	if got.Resources.MemoryBytes != 32<<20 {
		t.Fatalf("MemoryBytes = %d, want the requested cap", got.Resources.MemoryBytes)
	}
}

// TestNarrowRequiresADeadlineForACpuBudget pins the one implementation constraint
// the gate owns: a cpu budget is a rate, and only a deadline turns it into cpu
// seconds.
func TestNarrowRequiresADeadlineForACpuBudget(t *testing.T) {
	confine := sandbox.Confine{}
	opts := sandbox.ExecOptions{Resources: policy.Resources{CPUMillicores: 100}}

	err := func() error { _, err := confine.Narrow(opts); return err }()
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}

	opts.Timeout = time.Second
	if _, err := confine.Narrow(opts); err != nil {
		t.Fatalf("a cpu budget with a deadline was refused: %v", err)
	}
}

// FuzzNarrowIsMonotoneAndIdempotent pins the two properties the rest of the
// platform assumes: a call can only shrink what the instance granted, and
// resolving an already-resolved request changes nothing (so a retried or
// re-entered path cannot drift the budget).
func FuzzNarrowIsMonotoneAndIdempotent(f *testing.F) {
	f.Add(int64(0), int64(0), int64(1<<20), int64(1<<20), false)
	f.Add(int64(1<<20), int64(2<<20), int64(0), int64(0), false)
	f.Add(int64(1<<30), int64(1<<30), int64(1), int64(1), true)

	f.Fuzz(func(t *testing.T, ceilingMem, askedMem, ceilingOut, askedOut int64, cpu bool) {
		confine := sandbox.Confine{Resources: policy.Resources{
			MemoryBytes:    abs(ceilingMem),
			MaxOutputBytes: abs(ceilingOut),
		}}
		opts := sandbox.ExecOptions{
			Timeout: time.Second, // a cpu budget always has one here
			Resources: policy.Resources{
				MemoryBytes:    abs(askedMem),
				MaxOutputBytes: abs(askedOut),
			},
		}
		if cpu {
			confine.Resources.CPUMillicores = 100
			opts.Resources.CPUMillicores = 100
		}

		got, err := confine.Narrow(opts)
		if err != nil {
			return
		}

		// Monotone: every bounded dimension of the result fits the ceiling.
		if limit := confine.Resources.MemoryBytes; limit > 0 && got.Resources.MemoryBytes > limit {
			t.Fatalf("MemoryBytes %d exceeds the ceiling %d", got.Resources.MemoryBytes, limit)
		}
		if limit := confine.Resources.MaxOutputBytes; limit > 0 && got.Resources.MaxOutputBytes > limit {
			t.Fatalf("MaxOutputBytes %d exceeds the ceiling %d", got.Resources.MaxOutputBytes, limit)
		}
		if limit := confine.Resources.CPUMillicores; limit > 0 && got.Resources.CPUMillicores > limit {
			t.Fatalf("CPUMillicores %d exceeds the ceiling %d", got.Resources.CPUMillicores, limit)
		}

		// Idempotent: resolving the resolved result changes nothing.
		again, err := confine.Narrow(got)
		if err != nil {
			t.Fatalf("the resolved request was refused on the second pass: %v", err)
		}
		if again.Resources != got.Resources {
			t.Fatalf("not idempotent: %+v then %+v", got.Resources, again.Resources)
		}
	})
}

// abs keeps fuzz inputs out of the negative range without discarding them.
func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// TestValidateConfineRefusesWritesItCannotBound is the gate that makes "a boundary exists" true
// on every backend rather than only on the ones that project one. A read-only or narrowed write
// set on a backend that cannot confine writes would be enforced for tools (fsx) and silently not
// for commands, which is the worst of both: the deployment believes it is confined.
func TestValidateConfineRefusesWritesItCannotBound(t *testing.T) {
	unbounding := sandbox.Enforcement{FilesystemBounds: false}
	for name, files := range map[string]policy.Files{
		"read only":      {ReadOnly: true},
		"narrowed write": {Read: []string{pathtest.Abs("srv")}, Write: []string{pathtest.Abs("srv/work")}},
	} {
		confine := sandbox.Confine{Files: files}
		if err := sandbox.ValidateConfine(confine, unbounding); err == nil {
			t.Fatalf("%s: a backend that cannot confine writes accepted it", name)
		} else if !errdefs.IsNotAvailable(err) {
			t.Fatalf("%s: error = %v, want NotAvailable", name, err)
		}
	}
}

// TestValidateConfineAcceptsAnUnrestrictedWriteSet is the other half: "no requirement" needs no
// promise, so a backend without an OS boundary stays usable for grants that do not narrow writes.
func TestValidateConfineAcceptsAnUnrestrictedWriteSet(t *testing.T) {
	if err := sandbox.ValidateConfine(sandbox.Confine{}, sandbox.Enforcement{FilesystemBounds: false}); err != nil {
		t.Fatalf("an empty confine was refused: %v", err)
	}
	if err := sandbox.ValidateConfine(sandbox.Confine{Files: policy.Files{Read: []string{pathtest.Abs("srv")}, Write: []string{pathtest.Abs("srv")}}},
		sandbox.Enforcement{FilesystemBounds: true}); err != nil {
		t.Fatalf("a bounding backend refused a write set: %v", err)
	}
}

// TestDeniedfIsTheRefusalAHostActsOn pins the two classifications one refusal has to carry: the
// boundary refused before anything ran (ErrConfineDenied, which a host may answer by widening one
// command), and a rule refused rather than an execution failing (the policy classification the
// rest of the platform counts).
func TestDeniedfIsTheRefusalAHostActsOn(t *testing.T) {
	err := sandbox.Deniedf("write %q is outside the write set", "/srv/w/out/x")
	if !errors.Is(err, sandbox.ErrConfineDenied) {
		t.Fatalf("err = %v, want it to be a confine refusal", err)
	}
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("err = %v, want the policy classification", err)
	}
	if !strings.Contains(err.Error(), "/srv/w/out/x") {
		t.Fatalf("err = %q, want it to name what it refused", err)
	}
	// A plain error is not a confine refusal, which is what keeps the host from asking an
	// operator about every command that failed.
	if errors.Is(errors.New("exit status 1"), sandbox.ErrConfineDenied) {
		t.Fatal("an ordinary failure was classified as a confine refusal")
	}
}
