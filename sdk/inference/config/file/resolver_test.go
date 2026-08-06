package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolverReadsOwnedSecretBytesExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte("sk-live\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	secret, err := New().Resolve(t.Context(), path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Bytes are returned exactly, including the trailing newline: trimming
	// is the provider factory's decision, never the resolver's.
	if got := string(secret.Bytes()); got != "sk-live\n" {
		t.Fatalf("secret = %q", got)
	}
	if got := secret.String(); got != "<redacted>" {
		t.Fatalf("formatted secret = %q", got)
	}
}

func TestResolverRejectsMissingEmptyAndInvalidInputs(t *testing.T) {
	resolver := New()
	if _, err := resolver.Resolve(t.Context(), ""); err == nil {
		t.Fatal("empty key accepted")
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := resolver.Resolve(t.Context(), missing); err == nil {
		t.Fatal("missing file accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := resolver.Resolve(t.Context(), empty); err == nil {
		t.Fatal("empty secret file accepted")
	}

	noReader := Resolver{}
	if _, err := noReader.Resolve(t.Context(), "anything"); err == nil {
		t.Fatal("resolver without a read function accepted a key")
	}
}

func TestResolverInDirConfinesKeysAfterSymlinkResolution(t *testing.T) {
	root := t.TempDir()
	resolver, err := NewInDir(root)
	if err != nil {
		t.Fatalf("NewInDir: %v", err)
	}

	t.Run("relative and absolute keys inside the root", func(t *testing.T) {
		if err := os.WriteFile(
			filepath.Join(root, "openai"),
			[]byte("sk-inside"),
			0o600,
		); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		for _, key := range []string{
			"openai",
			"./openai",
			filepath.Join(root, "openai"),
		} {
			secret, err := resolver.Resolve(t.Context(), key)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", key, err)
			}
			if string(secret.Bytes()) != "sk-inside" {
				t.Fatalf("Resolve(%q) = %q", key, secret.Bytes())
			}
		}
	})

	t.Run("path traversal is rejected", func(t *testing.T) {
		// A sibling of root: one ".." is enough to escape confinement.
		outside := filepath.Join(filepath.Dir(root), "outside-secret")
		if err := os.WriteFile(outside, []byte("sk-outside"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		for _, key := range []string{
			"../outside-secret",
			"sub/../../outside-secret",
			outside,
		} {
			if _, err := resolver.Resolve(t.Context(), key); err == nil ||
				!strings.Contains(err.Error(), "escapes") {
				t.Fatalf("Resolve(%q) error = %v, want confinement rejection", key, err)
			}
		}
	})

	t.Run("symlink escape is rejected", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("sk-target"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		link := filepath.Join(root, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		if _, err := resolver.Resolve(t.Context(), "link"); err == nil ||
			!strings.Contains(err.Error(), "escapes") {
			t.Fatalf("Resolve(link) error = %v, want confinement rejection", err)
		}
	})

	t.Run("missing file inside the root reports the read error", func(t *testing.T) {
		_, err := resolver.Resolve(t.Context(), "absent")
		if err == nil || strings.Contains(err.Error(), "escapes") {
			t.Fatalf("Resolve(absent) error = %v, want read failure", err)
		}
	})
}

func TestResolverNewInDirValidatesRoot(t *testing.T) {
	if _, err := NewInDir(""); err == nil {
		t.Fatal("empty root accepted")
	}
}

func TestResolverHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := New().Resolve(ctx, "any"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve error = %v, want context.Canceled", err)
	}
}

func TestResolverSupportsInjectedReaders(t *testing.T) {
	resolver := NewWithReader(func(path string) ([]byte, error) {
		if path != "/virtual/key" {
			return nil, errors.New("unknown path")
		}
		return []byte("from-reader"), nil
	})
	secret, err := resolver.Resolve(t.Context(), "/virtual/key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(secret.Bytes()) != "from-reader" {
		t.Fatalf("secret = %q", secret.Bytes())
	}
}
