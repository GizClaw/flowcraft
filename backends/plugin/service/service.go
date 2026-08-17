package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

const (
	// ProtocolVersion1 is the v1 JSON-RPC wire protocol implemented by
	// this package.
	ProtocolVersion1 = 1

	defaultCallTimeout   = 30 * time.Second
	defaultMaxPayload    = 8 << 20 // 8 MiB
	maxStartAttempts     = 3
	startBackoff         = 200 * time.Millisecond
	terminateGracePeriod = 5 * time.Second
	defaultHostName      = "flowcraft"
)

// supportedProtocolVersions is the host's protocol support set, sent
// in the handshake. It is extended additively when new protocol
// versions are implemented.
var supportedProtocolVersions = []int{ProtocolVersion1}

// SupportedProtocolVersions returns a copy of the host's supported RPC
// protocol versions.
func SupportedProtocolVersions() []int {
	return append([]int(nil), supportedProtocolVersions...)
}

// Transport selects how the host reaches the plugin process.
type Transport string

const (
	// TransportStdio runs the plugin command and exchanges newline
	// delimited JSON on stdin/stdout; stderr goes to the host log.
	TransportStdio Transport = "stdio"
	// TransportHTTP posts JSON-RPC 2.0 requests to a fixed URL.
	TransportHTTP Transport = "http"
)

// Spec describes how to reach and drive one plugin service.
type Spec struct {
	Transport Transport

	// stdio fields.
	Command string
	Args    []string
	// Env is the plugin's complete environment: a minimal host base
	// (PATH, TMPDIR) plus these overrides. Host secrets are not
	// inherited; credentials must be declared explicitly here.
	Env    map[string]string
	Dir    string    // optional working directory for the command
	Stderr io.Writer // nil means os.Stderr

	// http fields.
	URL     string
	Headers map[string]string

	// HostName identifies the host in the handshake. Empty means
	// "flowcraft".
	HostName string
	// HostCoreVersion is the host core version sent in the handshake.
	HostCoreVersion string
	// CallTimeout caps every RPC call. Zero means 30s.
	CallTimeout time.Duration
	// MaxPayload caps request and response JSON sizes. Zero means
	// 8 MiB.
	MaxPayload int64
}

// Validate checks the static invariants of the spec.
func (s Spec) Validate() error {
	switch s.Transport {
	case TransportStdio:
		if s.Command == "" {
			return errdefs.Validationf("service: stdio transport requires command")
		}
	case TransportHTTP:
		if s.URL == "" {
			return errdefs.Validationf("service: http transport requires url")
		}
	default:
		return errdefs.Validationf("service: unknown transport %q", s.Transport)
	}
	return nil
}

func (s Spec) callTimeout() time.Duration {
	if s.CallTimeout > 0 {
		return s.CallTimeout
	}
	return defaultCallTimeout
}

func (s Spec) maxPayload() int64 {
	if s.MaxPayload > 0 {
		return s.MaxPayload
	}
	return defaultMaxPayload
}

func (s Spec) hostName() string {
	if s.HostName != "" {
		return s.HostName
	}
	return defaultHostName
}

// Capability is one implementation the plugin offers, as declared by
// the handshake result.
type Capability struct {
	Kind string        `json:"kind"`
	Impl string        `json:"impl"`
	Spec resource.Spec `json:"spec"`
	// Streaming declares support for incremental generate_stream via
	// the http /stream endpoint. Unary-only plugins leave it false.
	Streaming bool `json:"streaming,omitempty"`
}

// Handshake is the negotiated handshake result.
type Handshake struct {
	ProtocolVersion int          `json:"protocol_version"`
	Name            string       `json:"name"`
	Capabilities    []Capability `json:"capabilities"`
}

// Service drives one plugin process (or HTTP endpoint). Calls are
// serialized; the process is spawned lazily on first use.
type Service struct {
	spec Spec

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	client    *http.Client
	stream    *http.Client
	handshake *Handshake
	started   bool
	failed    error
	closed    bool
	nextID    int
}

// New validates spec and returns a Service without starting it.
func New(spec Spec) (*Service, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &Service{spec: spec}, nil
}

// Start validates spec, constructs a Service, and performs the
// handshake immediately.
func Start(ctx context.Context, spec Spec) (*Service, error) {
	s, err := New(spec)
	if err != nil {
		return nil, err
	}
	if err := s.Start(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Start spawns the process (stdio) and runs the handshake. It is
// idempotent: a service that already started returns nil.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked(ctx)
}

func (s *Service) startLocked(ctx context.Context) error {
	if s.closed {
		return errdefs.NotAvailablef("service: plugin service is closed")
	}
	if s.started && s.failed == nil && !s.closed {
		return nil
	}
	var lastErr error
	for attempt := range maxStartAttempts {
		if ctx.Err() != nil {
			s.failLocked(mapCtxError(ctx.Err()))
			return s.failed
		}
		if err := s.spawnAndHandshake(ctx); err == nil {
			return nil
		} else {
			lastErr = err
			s.teardownProcess()
			if attempt+1 == maxStartAttempts {
				break
			}
			select {
			case <-time.After(startBackoff):
			case <-ctx.Done():
				s.failLocked(mapCtxError(ctx.Err()))
				return s.failed
			}
		}
	}
	s.failLocked(errdefs.NotAvailablef(
		"service: start failed after %d attempts: %v", maxStartAttempts, lastErr))
	return s.failed
}

func (s *Service) failLocked(err error) {
	s.failed = err
	s.started = false
}

func (s *Service) ensureStartedLocked(ctx context.Context) error {
	switch {
	case s.closed:
		return errdefs.NotAvailablef("service: plugin service is closed")
	case s.failed != nil:
		return s.failed
	case s.started:
		return nil
	}
	return s.startLocked(ctx)
}

func (s *Service) spawnAndHandshake(ctx context.Context) error {
	switch s.spec.Transport {
	case TransportStdio:
		return s.spawnAndHandshakeStdio(ctx)
	case TransportHTTP:
		handshake, err := s.runHandshake(ctx)
		if err != nil {
			return err
		}
		s.handshake = handshake
		s.started = true
		return nil
	default:
		return errdefs.Validationf("service: unknown transport %q", s.spec.Transport)
	}
}

func (s *Service) spawnAndHandshakeStdio(ctx context.Context) error {
	cmd := exec.Command(s.spec.Command, s.spec.Args...)
	cmd.Dir = s.spec.Dir
	cmd.Env = mergeEnv(minimalBaseEnv(), s.spec.Env)
	cmd.Stderr = stderrWriter(s.spec.Stderr)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("service: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("service: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("service: start %s: %w", s.spec.Command, err)
	}
	s.cmd = cmd
	s.stdin = stdin
	s.stdout = bufio.NewReader(stdout)

	handshake, err := s.runHandshake(ctx)
	if err != nil {
		return err
	}
	s.handshake = handshake
	s.started = true
	return nil
}

func (s *Service) runHandshake(ctx context.Context) (*Handshake, error) {
	result, err := s.exchangeLocked(ctx, "plugin.handshake", handshakeParams{
		ProtocolVersions: SupportedProtocolVersions(),
		HostName:         s.spec.hostName(),
		HostCoreVersion:  s.spec.HostCoreVersion,
	})
	if err != nil {
		return nil, errdefs.NotAvailablef("service: handshake: %v", err)
	}
	handshake, err := parseHandshake(result)
	if err != nil {
		return nil, errdefs.NotAvailablef("service: handshake: %v", err)
	}
	return handshake, nil
}

func parseHandshake(raw json.RawMessage) (*Handshake, error) {
	var handshake Handshake
	if err := json.Unmarshal(raw, &handshake); err != nil {
		return nil, fmt.Errorf("decode handshake result: %w", err)
	}
	if !containsInt(supportedProtocolVersions, handshake.ProtocolVersion) {
		return nil, fmt.Errorf(
			"plugin protocol version %d is not supported by the host",
			handshake.ProtocolVersion)
	}
	if handshake.Name == "" {
		return nil, errors.New("handshake result is missing name")
	}
	for i, capability := range handshake.Capabilities {
		if capability.Kind == "" || capability.Impl == "" {
			return nil, fmt.Errorf(
				"handshake capability %d: kind and impl are required", i)
		}
		if err := capability.Spec.Validate(); err != nil {
			return nil, fmt.Errorf("handshake capability %d: %v", i, err)
		}
	}
	return &handshake, nil
}

// New constructs one plugin resource: resource.new with the capability
// and settings, returning the opaque handle.
func (s *Service) New(ctx context.Context, capability string, settings []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureStartedLocked(ctx); err != nil {
		return "", err
	}
	result, err := s.rpcExchangeLocked(ctx, "resource.new", newParams{
		Capability: capability,
		Settings:   json.RawMessage(settings),
	})
	if err != nil {
		return "", fmt.Errorf("service: resource.new %s: %w", capability, err)
	}
	var out struct {
		Handle string `json:"handle"`
	}
	if err := json.Unmarshal(result, &out); err != nil {
		return "", fmt.Errorf("service: resource.new: %w", err)
	}
	if out.Handle == "" {
		return "", errdefs.Validationf("service: resource.new returned an empty handle")
	}
	return out.Handle, nil
}

// Call invokes one method on a previously constructed handle:
// resource.call, returning the raw JSON result.
func (s *Service) Call(ctx context.Context, handle, method string, args []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureStartedLocked(ctx); err != nil {
		return nil, err
	}
	result, err := s.rpcExchangeLocked(ctx, "resource.call", callParams{
		Handle: handle,
		Method: method,
		Args:   json.RawMessage(args),
	})
	if err != nil {
		return nil, fmt.Errorf("service: resource.call %s/%s: %w", handle, method, err)
	}
	return result, nil
}

// CloseHandle releases one plugin resource: resource.close with the
// opaque handle. The plugin process stays running and the service
// remains healthy; this is the per-resource counterpart of [Service.Close].
func (s *Service) CloseHandle(ctx context.Context, handle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureStartedLocked(ctx); err != nil {
		return err
	}
	if _, err := s.rpcExchangeLocked(
		ctx, "resource.close", closeParams{Handle: handle},
	); err != nil {
		return fmt.Errorf("service: resource.close %s: %w", handle, err)
	}
	return nil
}

// Close stops the plugin process: SIGTERM, a grace period, then
// SIGKILL. It is idempotent.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.started = false
	defer func() {
		if s.client != nil {
			s.client.CloseIdleConnections()
		}
		if s.stream != nil {
			s.stream.CloseIdleConnections()
		}
	}()
	if s.cmd == nil {
		return nil
	}
	defer func() {
		s.cmd = nil
		s.stdin = nil
		s.stdout = nil
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- s.cmd.Wait() }()
	terminated := false
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-waitCh:
			terminated = true
		case <-time.After(terminateGracePeriod):
		case <-ctx.Done():
		}
	}
	if !terminated {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-waitCh
	}
	return nil
}

// Healthy reports whether the service is started, has not failed, and
// has not been closed.
func (s *Service) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started && s.failed == nil && !s.closed
}

// Handshake returns the negotiated handshake.
func (s *Service) Handshake() (Handshake, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handshake == nil {
		return Handshake{}, false
	}
	return *s.handshake, true
}

// Capabilities returns the handshake-declared capabilities, or nil
// before the service started.
func (s *Service) Capabilities() []Capability {
	handshake, ok := s.Handshake()
	if !ok {
		return nil
	}
	return handshake.Capabilities
}

// Spec returns the transport spec the service was created with.
func (s *Service) Spec() Spec {
	return s.spec
}

func (s *Service) rpcExchangeLocked(
	ctx context.Context, method string, params any,
) (json.RawMessage, error) {
	result, err := s.exchangeLocked(ctx, method, params)
	if err == nil {
		return result, nil
	}
	if _, ok := errors.AsType[*rpcError](err); ok {
		return nil, err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		if s.spec.Transport == TransportStdio {
			// A timed-out stdio read abandons a blocking reader on the
			// shared stream; the late response would desynchronize the
			// next exchange and concurrent reads race on the buffered
			// reader. Tear the process down instead of reusing it.
			transportErr := errdefs.NotAvailablef(
				"service: %s: stdio read aborted: %w", method, err)
			s.failLocked(transportErr)
			s.teardownProcess()
			return nil, transportErr
		}
		// HTTP per-call abort; the service may still be healthy.
		return nil, err
	}
	if errdefs.IsValidation(err) || errdefs.IsNotFound(err) {
		// Request-level rejections leave the service healthy.
		return nil, err
	}
	transportErr := errdefs.NotAvailablef("service: %s: transport: %v", method, err)
	s.failLocked(transportErr)
	s.teardownProcess()
	return nil, transportErr
}

func (s *Service) exchangeLocked(
	ctx context.Context, method string, params any,
) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, s.spec.callTimeout())
	defer cancel()

	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("service: encode %s params: %w", method, err)
	}
	payload, err := json.Marshal(request{
		JSONRPC: "2.0",
		ID:      s.nextID,
		Method:  method,
		Params:  paramsRaw,
	})
	if err != nil {
		return nil, fmt.Errorf("service: encode %s: %w", method, err)
	}
	requestID := s.nextID
	s.nextID++

	switch s.spec.Transport {
	case TransportStdio:
		return s.exchangeStdio(ctx, requestID, payload)
	case TransportHTTP:
		return s.exchangeHTTP(ctx, requestID, payload)
	default:
		return nil, errdefs.Validationf("service: unknown transport %q", s.spec.Transport)
	}
}

func (s *Service) exchangeStdio(
	ctx context.Context, requestID int, payload []byte,
) (json.RawMessage, error) {
	if int64(len(payload)) > s.spec.maxPayload() {
		return nil, errdefs.Validationf(
			"service: request exceeds payload limit of %d bytes", s.spec.maxPayload())
	}
	line := append(append([]byte(nil), payload...), '\n')
	if _, err := s.stdin.Write(line); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	raw, err := readLine(ctx, s.stdout, s.spec.maxPayload())
	if err != nil {
		return nil, err
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC response: %w", err)
	}
	if resp.ID != requestID {
		return nil, fmt.Errorf(
			"JSON-RPC response id %d, want %d", resp.ID, requestID)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

func (s *Service) exchangeHTTP(
	ctx context.Context, requestID int, payload []byte,
) (json.RawMessage, error) {
	if int64(len(payload)) > s.spec.maxPayload() {
		return nil, errdefs.Validationf(
			"service: request exceeds payload limit of %d bytes", s.spec.maxPayload())
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.spec.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for key, value := range s.spec.Headers {
		req.Header.Set(key, value)
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.spec.maxPayload()+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > s.spec.maxPayload() {
		return nil, fmt.Errorf(
			"response exceeds payload limit of %d bytes", s.spec.maxPayload())
	}
	var out response
	decodeErr := json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		// A well-formed JSON-RPC error body is an application-level
		// rejection even on a non-200 status; otherwise classify by
		// status so the caller can decide whether the service failed.
		if decodeErr == nil && out.Error != nil {
			return nil, out.Error
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return nil, errdefs.NotAvailablef(
				"service: %s returned %s", s.spec.URL, resp.Status)
		}
		return nil, errdefs.Validationf(
			"service: %s returned %s", s.spec.URL, resp.Status)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("invalid JSON-RPC response: %w", decodeErr)
	}
	if out.ID != requestID {
		return nil, fmt.Errorf(
			"JSON-RPC response id %d, want %d", out.ID, requestID)
	}
	if out.Error != nil {
		return nil, out.Error
	}
	return out.Result, nil
}

func (s *Service) httpClient() *http.Client {
	if s.client == nil {
		s.client = newHTTPClient(s.spec.callTimeout())
	}
	return s.client
}

// StreamingHTTPClient returns a pooled HTTP client without a total
// timeout for long-lived SSE streams. It shares the transport
// configuration with the RPC client but never imposes a deadline of
// its own; callers cancel the request context to end the stream.
func (s *Service) StreamingHTTPClient() *http.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream == nil {
		s.stream = &http.Client{Transport: newHTTPTransport()}
	}
	return s.stream
}

// newHTTPClient returns a pooled HTTP client with sane transport
// defaults for the plugin channel, capping every request at timeout.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: newHTTPTransport(),
	}
}

// newHTTPTransport returns the shared transport configuration for the
// plugin channel: pooled connections with sane DNS, TLS, and idle
// defaults.
func newHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
}

func (s *Service) teardownProcess() {
	if s.cmd == nil {
		return
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	s.cmd = nil
	s.stdin = nil
	s.stdout = nil
	s.handshake = nil
}

func mergeEnv(base []string, extra map[string]string) []string {
	merged := make([]string, 0, len(base)+len(extra))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := extra[key]; !overridden {
			merged = append(merged, entry)
		}
	}
	for key, value := range extra {
		merged = append(merged, key+"="+value)
	}
	return merged
}

// minimalBaseEnv returns the small allowlist of host environment
// variables every plugin needs to launch (PATH for exec resolution,
// TMPDIR for temporary files). Everything else — including host
// credentials — is deliberately not inherited.
func minimalBaseEnv() []string {
	var out []string
	for _, key := range []string{"PATH", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func stderrWriter(w io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return os.Stderr
}

func containsInt(values []int, target int) bool {
	return slices.Contains(values, target)
}

func mapCtxError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errdefs.Timeout(err)
	case errors.Is(err, context.Canceled):
		return errdefs.Interrupted(err)
	default:
		return err
	}
}
