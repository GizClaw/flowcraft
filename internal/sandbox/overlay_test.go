package sandbox

import (
	"testing"
)

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/workspace/skills", "workspace-skills"},
		{"/workspace/skills/", "workspace-skills"},
		{"workspace", "workspace"},
		{"/", "root"},
		{"", "root"},
		{"/a/b/c", "a-b-c"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeName(tt.input)
			if got != tt.want {
				t.Fatalf("sanitizeName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestOverlaySupported(t *testing.T) {
	// Just verify it doesn't panic; actual value depends on platform
	_ = OverlaySupported()
}

func TestNewOverlayManager(t *testing.T) {
	dir := t.TempDir()
	om, err := NewOverlayManager(dir)
	if err != nil {
		t.Fatalf("NewOverlayManager should succeed: %v", err)
	}
	if om == nil {
		t.Fatal("expected non-nil manager")
	}
	if om.baseDir != dir {
		t.Fatalf("baseDir = %q, want %q", om.baseDir, dir)
	}
}

func TestOverlayManager_CleanupNonexistent(t *testing.T) {
	dir := t.TempDir()
	om, err := NewOverlayManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := om.Cleanup("nonexistent-session"); err != nil {
		t.Fatalf("cleanup of nonexistent session should not error: %v", err)
	}
}
