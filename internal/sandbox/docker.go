package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Driver abstracts sandbox creation for different backends.
type Driver interface {
	Create(ctx context.Context, id string, opts CreateOptions) (Sandbox, error)
}

// CreateOptions configures sandbox container creation.
type CreateOptions struct {
	Image       string        `json:"image,omitempty"`
	NetworkMode string        `json:"network_mode,omitempty"`
	CPUQuota    int64         `json:"cpu_quota,omitempty"`
	MemoryLimit int64         `json:"memory_limit,omitempty"`
	Mounts      []MountConfig `json:"mounts,omitempty"`
}

// MountConfig describes a container mount (bind or volume).
type MountConfig struct {
	Source   string `json:"source" yaml:"source"`
	Target   string `json:"target" yaml:"target"`
	ReadOnly bool   `json:"readonly,omitempty" yaml:"readonly,omitempty"`
	Type     string `json:"type,omitempty" yaml:"type,omitempty"` // "bind" (default) or "volume"
	Overlay  bool   `json:"overlay,omitempty" yaml:"overlay,omitempty"`
}

// DockerDriver creates Docker-based sandboxes.
type DockerDriver struct {
	defaultImage string
	mounts       []MountConfig
}

// NewDockerDriver creates a driver with default settings.
func NewDockerDriver(image string, mounts []MountConfig) *DockerDriver {
	if image == "" {
		image = "flowcraft/sandbox:latest"
	}
	return &DockerDriver{defaultImage: image, mounts: mounts}
}

func (d *DockerDriver) Create(ctx context.Context, id string, opts CreateOptions) (Sandbox, error) {
	image := opts.Image
	if image == "" {
		image = d.defaultImage
	}
	networkMode := opts.NetworkMode
	if networkMode == "" {
		networkMode = "none"
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("sandbox: docker client: %w", err)
	}

	allMounts := make([]MountConfig, 0, len(d.mounts)+len(opts.Mounts))
	allMounts = append(allMounts, d.mounts...)
	allMounts = append(allMounts, opts.Mounts...)
	var dockerMounts []mount.Mount
	for _, m := range allMounts {
		mt := mount.TypeBind
		if m.Type == "volume" {
			mt = mount.TypeVolume
		}
		dockerMounts = append(dockerMounts, mount.Mount{
			Type: mt, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly,
		})
	}

	hostCfg := &container.HostConfig{
		NetworkMode: container.NetworkMode(networkMode),
		Mounts:      dockerMounts,
		Resources: container.Resources{
			CPUQuota: opts.CPUQuota,
			Memory:   opts.MemoryLimit,
		},
	}

	containerCfg := &container.Config{
		Image:      image,
		Entrypoint: []string{"sleep", "infinity"},
		WorkingDir: "/workspace",
		Labels: map[string]string{
			"flowcraft.sandbox.id": id,
			"managed-by":           "flowcraft",
		},
	}

	containerName := "flowcraft-sandbox-" + id

	// Reuse a running container left over from a previous process,
	// but only if its mount configuration still matches.
	if info, inspectErr := cli.ContainerInspect(ctx, containerName); inspectErr == nil {
		if info.State.Running && mountsMatch(info.Mounts, dockerMounts) {
			return &DockerSandbox{id: id, containerID: info.ID, cli: cli}, nil
		}
		_ = cli.ContainerStop(ctx, containerName, container.StopOptions{})
		_ = cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true})
	}

	resp, err := cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, containerName)
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("sandbox: create container: %w", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		_ = cli.Close()
		return nil, fmt.Errorf("sandbox: start container: %w", err)
	}

	return &DockerSandbox{id: id, containerID: resp.ID, cli: cli}, nil
}

// DockerSandbox runs commands inside a Docker container.
type DockerSandbox struct {
	id          string
	containerID string
	cli         *client.Client
	closed      atomic.Bool
}

func (s *DockerSandbox) ID() string { return s.id }

func (s *DockerSandbox) Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.exec",
		trace.WithAttributes(
			attribute.String("sandbox.runtime_id", s.id),
			attribute.String("sandbox.command", cmd),
		),
	)
	defer span.End()

	if s.closed.Load() {
		span.SetStatus(codes.Error, "sandbox closed")
		return nil, ErrClosed
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	start := time.Now()
	fullCmd := append([]string{cmd}, args...)
	workDir := "/workspace"
	if opts.WorkDir != "" {
		if filepath.IsAbs(opts.WorkDir) {
			workDir = opts.WorkDir
		} else {
			workDir = filepath.Join("/workspace", opts.WorkDir)
		}
	}

	var env []string
	for k, v := range opts.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	execCfg := container.ExecOptions{
		Cmd: fullCmd, WorkingDir: workDir, Env: env,
		AttachStdout: true, AttachStderr: true, AttachStdin: opts.Stdin != nil,
	}

	execResp, err := s.cli.ContainerExecCreate(ctx, s.containerID, execCfg)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("sandbox: exec create: %w", err)
	}

	attach, err := s.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("sandbox: exec attach: %w", err)
	}
	defer attach.Close()

	// Force-close the hijacked connection when context is cancelled,
	// otherwise stdcopy.StdCopy blocks forever since the underlying
	// net.Conn does not respect context cancellation.
	doneCh := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			attach.Close()
		case <-doneCh:
		}
	}()
	defer close(doneCh)

	if opts.Stdin != nil {
		_, _ = attach.Conn.Write(opts.Stdin)
		_ = attach.CloseWrite()
	}

	var stdout, stderr bytes.Buffer
	_, copyErr := stdcopy.StdCopy(&stdout, &stderr, attach.Reader)

	if ctx.Err() != nil {
		dur := time.Since(start)
		sbExecDuration.Record(ctx, dur.Seconds())
		sbExecCount.Add(ctx, 1, metric.WithAttributes(
			attribute.String("runtime_id", s.id),
			attribute.String("status", "timeout")))
		span.SetStatus(codes.Error, "exec timeout")
		return &ExecResult{
			ExitCode: -1,
			Stdout:   stdout.String(),
			Stderr:   fmt.Sprintf("command timed out after %v\n%s", dur, stderr.String()),
		}, fmt.Errorf("sandbox: exec timeout after %v", dur)
	}

	if copyErr != nil {
		span.RecordError(copyErr)
		return nil, fmt.Errorf("sandbox: exec read output: %w", copyErr)
	}

	inspect, err := s.cli.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("sandbox: exec inspect: %w", err)
	}

	dur := time.Since(start)
	sbExecDuration.Record(ctx, dur.Seconds())
	span.SetAttributes(attribute.Int("sandbox.exit_code", inspect.ExitCode))

	status := "success"
	if inspect.ExitCode != 0 {
		status = "nonzero_exit"
	}
	sbExecCount.Add(ctx, 1, metric.WithAttributes(
		attribute.String("runtime_id", s.id),
		attribute.String("status", status)))

	return &ExecResult{ExitCode: inspect.ExitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

func (s *DockerSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	containerPath := resolveContainerPath(path)
	reader, _, err := s.cli.CopyFromContainer(ctx, s.containerID, containerPath)
	if err != nil {
		if strings.Contains(err.Error(), "No such") || strings.Contains(err.Error(), "not found") {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("sandbox: read file: %w", err)
	}
	defer func() { _ = reader.Close() }()

	tr := tar.NewReader(reader)
	if _, err := tr.Next(); err != nil {
		return nil, fmt.Errorf("sandbox: read tar header: %w", err)
	}
	return io.ReadAll(tr)
}

func (s *DockerSandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	if s.closed.Load() {
		return ErrClosed
	}
	containerPath := resolveContainerPath(path)
	dir := filepath.Dir(containerPath)

	mkdirResult, err := s.Exec(ctx, "mkdir", []string{"-p", dir}, ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("sandbox: mkdir for write: %w", err)
	}
	if mkdirResult.ExitCode != 0 {
		return fmt.Errorf("sandbox: mkdir failed: %s", mkdirResult.Stderr)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: filepath.Base(containerPath), Mode: 0o644, Size: int64(len(data))}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("sandbox: tar header: %w", err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("sandbox: tar write: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("sandbox: tar close: %w", err)
	}

	return s.cli.CopyToContainer(ctx, s.containerID, dir, &buf, container.CopyToContainerOptions{})
}

func (s *DockerSandbox) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	timeout := 5
	_ = s.cli.ContainerStop(ctx, s.containerID, container.StopOptions{Timeout: &timeout})
	_ = s.cli.ContainerRemove(ctx, s.containerID, container.RemoveOptions{Force: true})
	return s.cli.Close()
}

func resolveContainerPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join("/workspace", path)
}

// mountsMatch checks whether the existing container mounts are consistent
// with the desired mount configuration. Order-independent comparison.
func mountsMatch(existing []container.MountPoint, desired []mount.Mount) bool {
	if len(desired) == 0 {
		return true
	}
	if len(existing) < len(desired) {
		return false
	}
	type key struct {
		typ    mount.Type
		target string
	}
	want := make(map[key]mount.Mount, len(desired))
	for _, m := range desired {
		want[key{m.Type, m.Target}] = m
	}
	for k, w := range want {
		found := false
		for _, e := range existing {
			if e.Type == k.typ && e.Destination == k.target {
				if w.Type == mount.TypeBind && e.Source != w.Source {
					return false
				}
				if w.Type == mount.TypeVolume && e.Name != w.Source {
					return false
				}
				if w.ReadOnly && e.RW {
					return false
				}
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
