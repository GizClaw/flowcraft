package sandbox

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// --- SB-17: MountConfig.Type="volume" 生成 mount.TypeVolume ---

func TestMountConfigToDockerMount_Bind(t *testing.T) {
	m := MountConfig{Source: "/host/skills", Target: "/workspace/skills", ReadOnly: true}
	dm := toDockerMount(m)
	if dm.Type != mount.TypeBind {
		t.Fatalf("expected TypeBind, got %v", dm.Type)
	}
	if dm.Source != "/host/skills" {
		t.Fatalf("unexpected source: %s", dm.Source)
	}
	if dm.Target != "/workspace/skills" {
		t.Fatalf("unexpected target: %s", dm.Target)
	}
	if !dm.ReadOnly {
		t.Fatal("expected ReadOnly=true")
	}
}

func TestMountConfigToDockerMount_Volume(t *testing.T) {
	m := MountConfig{Type: "volume", Source: "deploy_flowcraft-workspace", Target: "/workspace"}
	dm := toDockerMount(m)
	if dm.Type != mount.TypeVolume {
		t.Fatalf("expected TypeVolume, got %v", dm.Type)
	}
	if dm.Source != "deploy_flowcraft-workspace" {
		t.Fatalf("unexpected source: %s", dm.Source)
	}
	if dm.Target != "/workspace" {
		t.Fatalf("unexpected target: %s", dm.Target)
	}
}

func TestMountConfigToDockerMount_DefaultBind(t *testing.T) {
	m := MountConfig{Source: "/a", Target: "/b"}
	dm := toDockerMount(m)
	if dm.Type != mount.TypeBind {
		t.Fatalf("empty Type should default to TypeBind, got %v", dm.Type)
	}
}

// --- SB-18: 多次 Create 调用无 slice 共享污染 ---

func TestDockerDriver_SliceIsolation(t *testing.T) {
	baseMounts := []MountConfig{
		{Source: "/host/base", Target: "/workspace/base"},
	}
	d := NewDockerDriver("test:latest", baseMounts)

	extra1 := []MountConfig{{Source: "/host/a", Target: "/workspace/a"}}
	extra2 := []MountConfig{{Source: "/host/b", Target: "/workspace/b"}}

	all1 := buildAllMounts(d.mounts, extra1)
	all2 := buildAllMounts(d.mounts, extra2)

	if len(all1) != 2 || len(all2) != 2 {
		t.Fatalf("expected 2 mounts each, got %d and %d", len(all1), len(all2))
	}
	if all1[1].Source == all2[1].Source {
		t.Fatal("slice sharing detected: second elements should differ")
	}
	if d.mounts[0].Source != "/host/base" {
		t.Fatal("original mounts were mutated")
	}
	if len(d.mounts) != 1 {
		t.Fatalf("original mounts length changed: %d", len(d.mounts))
	}
}

// --- SB-10: mountsMatch ---

func TestMountsMatch_Empty(t *testing.T) {
	if !mountsMatch(nil, nil) {
		t.Fatal("empty desired should match")
	}
}

func TestMountsMatch_BindMatch(t *testing.T) {
	existing := []container.MountPoint{
		{Type: mount.TypeBind, Source: "/host/skills", Destination: "/workspace/skills", RW: false},
	}
	desired := []mount.Mount{
		{Type: mount.TypeBind, Source: "/host/skills", Target: "/workspace/skills", ReadOnly: true},
	}
	if !mountsMatch(existing, desired) {
		t.Fatal("should match")
	}
}

func TestMountsMatch_BindSourceMismatch(t *testing.T) {
	existing := []container.MountPoint{
		{Type: mount.TypeBind, Source: "/old/path", Destination: "/workspace/skills", RW: false},
	}
	desired := []mount.Mount{
		{Type: mount.TypeBind, Source: "/new/path", Target: "/workspace/skills", ReadOnly: true},
	}
	if mountsMatch(existing, desired) {
		t.Fatal("should not match: source differs")
	}
}

func TestMountsMatch_VolumeMatch(t *testing.T) {
	existing := []container.MountPoint{
		{Type: mount.TypeVolume, Name: "my-vol", Destination: "/workspace", RW: true},
	}
	desired := []mount.Mount{
		{Type: mount.TypeVolume, Source: "my-vol", Target: "/workspace"},
	}
	if !mountsMatch(existing, desired) {
		t.Fatal("should match volume by name")
	}
}

func TestMountsMatch_VolumeNameMismatch(t *testing.T) {
	existing := []container.MountPoint{
		{Type: mount.TypeVolume, Name: "old-vol", Destination: "/workspace", RW: true},
	}
	desired := []mount.Mount{
		{Type: mount.TypeVolume, Source: "new-vol", Target: "/workspace"},
	}
	if mountsMatch(existing, desired) {
		t.Fatal("should not match: volume name differs")
	}
}

func TestMountsMatch_MissingMount(t *testing.T) {
	existing := []container.MountPoint{}
	desired := []mount.Mount{
		{Type: mount.TypeBind, Source: "/x", Target: "/y"},
	}
	if mountsMatch(existing, desired) {
		t.Fatal("should not match: missing mount")
	}
}

func TestMountsMatch_ReadOnlyMismatch(t *testing.T) {
	existing := []container.MountPoint{
		{Type: mount.TypeBind, Source: "/host/skills", Destination: "/workspace/skills", RW: true},
	}
	desired := []mount.Mount{
		{Type: mount.TypeBind, Source: "/host/skills", Target: "/workspace/skills", ReadOnly: true},
	}
	if mountsMatch(existing, desired) {
		t.Fatal("should not match: desired readonly but existing is RW")
	}
}

// --- helpers ---

func toDockerMount(m MountConfig) mount.Mount {
	mt := mount.TypeBind
	if m.Type == "volume" {
		mt = mount.TypeVolume
	}
	return mount.Mount{Type: mt, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly}
}

func buildAllMounts(base, extra []MountConfig) []MountConfig {
	all := make([]MountConfig, 0, len(base)+len(extra))
	all = append(all, base...)
	all = append(all, extra...)
	return all
}
