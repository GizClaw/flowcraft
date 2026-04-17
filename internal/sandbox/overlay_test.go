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

func TestHasOverlayMounts(t *testing.T) {
	if hasOverlayMounts(nil) {
		t.Fatal("nil mounts should return false")
	}
	if hasOverlayMounts([]MountConfig{{Source: "/a", Target: "/b"}}) {
		t.Fatal("non-overlay mount should return false")
	}
	if !hasOverlayMounts([]MountConfig{{Source: "/a", Target: "/b", Overlay: true}}) {
		t.Fatal("overlay mount should return true")
	}
	if !hasOverlayMounts([]MountConfig{
		{Source: "/a", Target: "/b"},
		{Source: "/c", Target: "/d", Overlay: true},
	}) {
		t.Fatal("mixed mounts with one overlay should return true")
	}
}

func TestResolveMounts_NoOverlayManager(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	cfg.Mounts = []MountConfig{
		{Source: "/host/skills", Target: "/workspace/skills", ReadOnly: true, Overlay: true},
		{Source: "/host/data", Target: "/workspace/data"},
	}
	m, err := NewManager(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	// On non-Linux (or when overlay manager fails), overlay mounts
	// should fall back to direct read-write bind mount
	resolved := m.resolveMounts(t.Context(), "test-session")
	if len(resolved) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(resolved))
	}

	skillsMount := resolved[0]
	if OverlaySupported() {
		// On Linux the overlay manager is initialized and Prepare is called,
		// but mount -t overlay will fail in non-root test environment,
		// so it falls back to direct mount
		if skillsMount.ReadOnly {
			t.Fatal("fallback should not be read-only")
		}
	} else {
		// On non-Linux, overlay manager is nil, mount is passed through unchanged
		if skillsMount.Source != "/host/skills" {
			t.Fatalf("expected source /host/skills, got %s", skillsMount.Source)
		}
	}

	dataMount := resolved[1]
	if dataMount.Source != "/host/data" {
		t.Fatalf("non-overlay mount should pass through, got source %s", dataMount.Source)
	}
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
