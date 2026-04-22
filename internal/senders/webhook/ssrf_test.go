package webhook

import (
	"errors"
	"testing"
)

func TestSSRFGuard_BlocksPrivateAndMetadata(t *testing.T) {
	g := NewSSRFGuard()
	cases := []string{
		"http://127.0.0.1/foo",
		"https://10.0.0.1:8080/bar",
		"http://192.168.1.5/",
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1",
		"http://host.local",
		"http://host.internal",
		"ftp://example.com",
		"http://0.0.0.0",
		"http:///bad",
	}
	for _, raw := range cases {
		if err := g.Check(raw); !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("expected SSRF block for %s, got %v", raw, err)
		}
	}
}

func TestSSRFGuard_AllowsPublicLiteralIP(t *testing.T) {
	g := NewSSRFGuard()
	if err := g.Check("https://8.8.8.8/foo"); err != nil {
		t.Fatalf("public literal IP should pass, got %v", err)
	}
	if err := g.Check("https://1.1.1.1/"); err != nil {
		t.Fatalf("public literal IP should pass, got %v", err)
	}
}
