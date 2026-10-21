package protocol

import (
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestVersionMatchesConstants(t *testing.T) {
	if got, want := Version, fmt.Sprintf("%d.%d", Major, Minor); got != want {
		t.Fatalf("Version = %q, want %q", got, want)
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in           string
		major, minor uint32
	}{
		{in: "1.0", major: 1},
		{in: "1.9", major: 1, minor: 9},
		{in: " 1.3 ", major: 1, minor: 3},
		{in: "10.20", major: 10, minor: 20},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			major, minor, err := ParseVersion(tc.in)
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", tc.in, err)
			}
			if major != tc.major || minor != tc.minor {
				t.Fatalf("ParseVersion(%q) = %d.%d, want %d.%d",
					tc.in, major, minor, tc.major, tc.minor)
			}
		})
	}
}

func TestParseVersionRefusesMalformed(t *testing.T) {
	// Each of these names something this build cannot even parse, so the
	// failure has to be terminal instead of a version check that never
	// runs.
	for _, in := range []string{"", "1", "1.0.0", "x.0", "1.x", "-1.0", "1.-2", "1."} {
		t.Run(in, func(t *testing.T) {
			_, _, err := ParseVersion(in)
			if err == nil {
				t.Fatalf("ParseVersion(%q) accepted a malformed version", in)
			}
			if !errdefs.IsValidation(err) {
				t.Fatalf("ParseVersion(%q) = %v, want a validation error", in, err)
			}
		})
	}
}

func TestCheckCompatible(t *testing.T) {
	// A higher minor is accepted because one major may only grow
	// additively; a different major is the one boundary that is refused.
	cases := []struct {
		in      string
		wantErr bool
	}{
		{in: Version},
		{in: "1.9"},
		{in: "0.9", wantErr: true},
		{in: "2.0", wantErr: true},
		{in: "", wantErr: true},
		{in: "1.x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := CheckCompatible(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("CheckCompatible(%q) accepted a foreign version", tc.in)
				}
				if !errdefs.IsValidation(err) {
					t.Fatalf("CheckCompatible(%q) = %v, want a validation error",
						tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckCompatible(%q): %v", tc.in, err)
			}
		})
	}
}

func TestNegotiatedVersion(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{in: Version, want: Version},
		{in: "1.7", want: Version},
	} {
		got, err := NegotiatedVersion(tc.in)
		if err != nil {
			t.Fatalf("NegotiatedVersion(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NegotiatedVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := NegotiatedVersion("2.0"); !errdefs.IsValidation(err) {
		t.Fatalf("NegotiatedVersion(\"2.0\") = %v, want a validation error", err)
	}
}
