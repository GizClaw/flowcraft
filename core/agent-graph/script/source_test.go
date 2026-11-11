package script

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestFileSourceReadsRelativeToBaseDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "tally.star", "def run(state):\n    return {}\n")

	source := &FileSource{BaseDir: dir}
	script, err := source.Load(context.Background(), "tally.star")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if script.Name != filepath.Join(dir, "tally.star") {
		t.Fatalf("name = %q, want the resolved path", script.Name)
	}
	if !strings.HasPrefix(script.Version, "sha256:") {
		t.Fatalf("version = %q, want a content hash", script.Version)
	}
	if !strings.Contains(script.Text, "def run(state):") {
		t.Fatalf("text = %q", script.Text)
	}
}

func TestFileSourceVersionFollowsContent(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "rule.star", "one")

	source := &FileSource{BaseDir: dir}
	first, err := source.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	writeFile(t, dir, "rule.star", "two")
	second, err := source.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if first.Version == second.Version {
		t.Fatal("an edited file kept its version, so a cached program would run stale logic")
	}
	if second.Text != "two" {
		t.Fatalf("text = %q", second.Text)
	}
}

func TestFileSourceTTLBoundsTheStatInterval(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "rule.star", "one")

	cached := &FileSource{BaseDir: dir, TTL: time.Hour}
	if _, err := cached.Load(context.Background(), path); err != nil {
		t.Fatalf("load: %v", err)
	}
	writeFile(t, dir, "rule.star", "much longer body")
	got, err := cached.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Text != "one" {
		t.Fatalf("text = %q, want the cached body inside the TTL", got.Text)
	}

	fresh := &FileSource{BaseDir: dir}
	got, err = fresh.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Text != "much longer body" {
		t.Fatalf("text = %q, want the new body without a TTL", got.Text)
	}
}

func TestFileSourceReportsWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	source := &FileSource{BaseDir: dir}
	if _, err := source.Load(context.Background(), "absent.star"); !errdefs.IsNotFound(err) {
		t.Fatalf("error = %v, want not found", err)
	}
	if _, err := source.Load(context.Background(), ""); !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want a validation error for an empty reference", err)
	}
	if _, err := source.Load(context.Background(), dir); !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want a validation error for a directory", err)
	}
}

func TestFileSourceHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := &FileSource{BaseDir: t.TempDir()}
	if _, err := source.Load(ctx, "anything"); err == nil {
		t.Fatal("expected the cancelled context to stop the load")
	}
}
