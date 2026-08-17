package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// TestHelperProcess is the re-executed stdio plugin used by the
// transport tests. It is inert in normal test runs.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("PLUGIN_HELPER") != "1" {
		return
	}
	defer os.Exit(0)
	runHelper(os.Getenv, os.Stdin, os.Stdout)
}

func runHelper(getenv func(string) string, stdin io.Reader, stdout io.Writer) {
	protocol := ProtocolVersion1
	if v := getenv("PLUGIN_HELPER_PROTOCOL"); v != "" {
		protocol, _ = strconv.Atoi(v)
	}
	exitAfterHandshake := getenv("PLUGIN_HELPER_EXIT_AFTER") == "1"
	sleepMS := 0
	if v := getenv("PLUGIN_HELPER_SLEEP_MS"); v != "" {
		sleepMS, _ = strconv.Atoi(v)
	}

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	encoder := json.NewEncoder(stdout)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = encoder.Encode(response{
				JSONRPC: "2.0", ID: -1,
				Error: &rpcError{Code: -32700, Message: "parse error"},
			})
			continue
		}
		switch req.Method {
		case "plugin.handshake":
			_ = encoder.Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustRaw(Handshake{
					ProtocolVersion: protocol,
					Name:            "test.echo",
					Capabilities: []Capability{{
						Kind: "example.Echo",
						Impl: "echo",
						Spec: resource.Spec{Kind: "example.Echo", Impl: "echo"},
					}},
				}),
			})
			if exitAfterHandshake {
				return
			}
		case "resource.new":
			_ = encoder.Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustRaw(struct {
					Handle string `json:"handle"`
				}{Handle: "h-1"}),
			})
		case "resource.call":
			var params callParams
			_ = json.Unmarshal(req.Params, &params)
			if sleepMS > 0 {
				time.Sleep(time.Duration(sleepMS) * time.Millisecond)
			}
			if params.Method == "boom" {
				_ = encoder.Encode(response{
					JSONRPC: "2.0", ID: req.ID,
					Error: &rpcError{Code: -32000, Message: "boom: rejected"},
				})
				continue
			}
			_ = encoder.Encode(response{
				JSONRPC: "2.0", ID: req.ID, Result: params.Args,
			})
		case "resource.close":
			_ = encoder.Encode(response{
				JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage("{}"),
			})
		default:
			_ = encoder.Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{Code: -32601, Message: "method not found"},
			})
		}
	}
}

func mustRaw(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func helperSpec(extraEnv map[string]string) Spec {
	env := map[string]string{"PLUGIN_HELPER": "1"}
	for key, value := range extraEnv {
		env[key] = value
	}
	return Spec{
		Transport: TransportStdio,
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestHelperProcess", "--"},
		Env:       env,
		Stderr:    io.Discard,
	}
}

func TestStdioRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, err := New(helperSpec(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.Healthy() {
		t.Fatal("service must be lazy: not healthy before first use")
	}

	handle, err := svc.New(ctx, "example.Echo/echo", []byte(`{"who":"world"}`))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if handle != "h-1" {
		t.Fatalf("handle = %q, want h-1", handle)
	}
	if !svc.Healthy() {
		t.Fatal("service should be healthy after first use")
	}

	handshake, ok := svc.Handshake()
	if !ok {
		t.Fatal("handshake missing after start")
	}
	if handshake.ProtocolVersion != ProtocolVersion1 || handshake.Name != "test.echo" {
		t.Fatalf("handshake = %+v", handshake)
	}
	if len(svc.Capabilities()) != 1 {
		t.Fatalf("capabilities = %+v", svc.Capabilities())
	}

	out, err := svc.Call(ctx, handle, "echo", []byte(`{"ping":"pong"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["ping"] != "pong" {
		t.Fatalf("echo = %v", got)
	}

	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if svc.Healthy() {
		t.Fatal("service should be unhealthy after Close")
	}
	if _, err := svc.Call(ctx, handle, "echo", nil); err == nil {
		t.Fatal("Call after Close must fail")
	}
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStartImmediate(t *testing.T) {
	svc, err := Start(context.Background(), helperSpec(nil))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = svc.Close(context.Background()) }()
	if !svc.Healthy() {
		t.Fatal("expected healthy after immediate Start")
	}
}

func TestHandshakeVersionMismatch(t *testing.T) {
	svc, err := New(helperSpec(map[string]string{"PLUGIN_HELPER_PROTOCOL": "2"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.New(context.Background(), "example.Echo/echo", nil)
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
	if svc.Healthy() {
		t.Fatal("service must not be healthy after failed start")
	}
}

func TestCallError(t *testing.T) {
	ctx := context.Background()
	svc, err := New(helperSpec(nil))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := svc.New(ctx, "example.Echo/echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Call(ctx, handle, "boom", nil)
	if err == nil || !strings.Contains(err.Error(), "boom: rejected") {
		t.Fatalf("err = %v, want boom rejection", err)
	}
	if !svc.Healthy() {
		t.Fatal("an RPC error must not mark the service unhealthy")
	}
}

func TestCloseHandle(t *testing.T) {
	ctx := context.Background()
	svc, err := New(helperSpec(nil))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := svc.New(ctx, "example.Echo/echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseHandle(ctx, handle); err != nil {
		t.Fatalf("CloseHandle: %v", err)
	}
	if !svc.Healthy() {
		t.Fatal("CloseHandle must not tear the plugin process down")
	}
	// The process is still usable after releasing one handle.
	if _, err := svc.Call(ctx, handle, "echo", nil); err != nil {
		t.Fatalf("Call after CloseHandle: %v", err)
	}
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestProcessDeath(t *testing.T) {
	ctx := context.Background()
	svc, err := New(helperSpec(map[string]string{"PLUGIN_HELPER_EXIT_AFTER": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.New(ctx, "example.Echo/echo", nil)
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
	if svc.Healthy() {
		t.Fatal("service must be unhealthy after process death")
	}
}

func TestCallTimeout(t *testing.T) {
	svc, err := New(helperSpec(map[string]string{"PLUGIN_HELPER_SLEEP_MS": "3000"}))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := svc.New(context.Background(), "example.Echo/echo", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err = svc.Call(ctx, handle, "echo", nil)
	cancel()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
	if svc.Healthy() {
		t.Fatal("a stdio timeout must mark the service unhealthy")
	}
	// The abandoned stream must never be reused: the next call fails
	// fast with NotAvailable instead of hanging or racing the stale
	// reader.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	_, err = svc.Call(ctx2, handle, "echo", nil)
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("call after timeout = %v, want NotAvailable", err)
	}
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestStartAfterClose(t *testing.T) {
	svc, err := New(helperSpec(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := svc.Start(context.Background()); err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("Start after Close = %v, want NotAvailable", err)
	}
}

func TestHTTPTransport(t *testing.T) {
	var sawHeader bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") == "1" {
			sawHeader = true
		}
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		switch req.Method {
		case "plugin.handshake":
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustRaw(Handshake{
					ProtocolVersion: ProtocolVersion1,
					Name:            "test.http",
					Capabilities: []Capability{{
						Kind: "example.Echo",
						Impl: "echo",
						Spec: resource.Spec{Kind: "example.Echo", Impl: "echo"},
					}},
				}),
			})
		case "resource.new":
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustRaw(struct {
					Handle string `json:"handle"`
				}{Handle: "h-http"}),
			})
		case "resource.call":
			var params callParams
			_ = json.Unmarshal(req.Params, &params)
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID, Result: params.Args,
			})
		default:
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{Code: -32601, Message: "method not found"},
			})
		}
	}))
	defer server.Close()

	svc, err := New(Spec{
		Transport: TransportHTTP,
		URL:       server.URL,
		Headers:   map[string]string{"X-Test": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := svc.New(context.Background(), "example.Echo/echo", []byte(`{}`))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if handle != "h-http" {
		t.Fatalf("handle = %q, want h-http", handle)
	}
	out, err := svc.Call(context.Background(), handle, "echo", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !bytes.Contains(out, []byte(`"ok":true`)) {
		t.Fatalf("echo = %s", out)
	}
	if !sawHeader {
		t.Fatal("custom header was not sent")
	}
	if err := svc.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPErrorBodyNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "plugin.handshake":
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustRaw(Handshake{
					ProtocolVersion: ProtocolVersion1,
					Name:            "test.http",
					Capabilities: []Capability{{
						Kind: "example.Echo",
						Impl: "echo",
						Spec: resource.Spec{Kind: "example.Echo", Impl: "echo"},
					}},
				}),
			})
		case "resource.new":
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustRaw(struct {
					Handle string `json:"handle"`
				}{Handle: "h-http"}),
			})
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{Code: -32000, Message: "boom"},
			})
		}
	}))
	defer server.Close()

	svc, err := New(Spec{Transport: TransportHTTP, URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := svc.New(context.Background(), "example.Echo/echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Call(context.Background(), handle, "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want boom rejection", err)
	}
	if !svc.Healthy() {
		t.Fatal("a JSON-RPC error body on non-200 must not mark the service unhealthy")
	}
}

func TestHTTPStatus503(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "plugin.handshake":
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustRaw(Handshake{
					ProtocolVersion: ProtocolVersion1,
					Name:            "test.http",
					Capabilities: []Capability{{
						Kind: "example.Echo",
						Impl: "echo",
						Spec: resource.Spec{Kind: "example.Echo", Impl: "echo"},
					}},
				}),
			})
		case "resource.new":
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustRaw(struct {
					Handle string `json:"handle"`
				}{Handle: "h-http"}),
			})
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	svc, err := New(Spec{Transport: TransportHTTP, URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := svc.New(context.Background(), "example.Echo/echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Call(context.Background(), handle, "echo", nil)
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
	if svc.Healthy() {
		t.Fatal("a bare 5xx must mark the service unhealthy")
	}
}

func TestHTTPStatus400(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "plugin.handshake":
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustRaw(Handshake{
					ProtocolVersion: ProtocolVersion1,
					Name:            "test.http",
					Capabilities: []Capability{{
						Kind: "example.Echo",
						Impl: "echo",
						Spec: resource.Spec{Kind: "example.Echo", Impl: "echo"},
					}},
				}),
			})
		case "resource.new":
			_ = json.NewEncoder(w).Encode(response{
				JSONRPC: "2.0", ID: req.ID,
				Result: mustRaw(struct {
					Handle string `json:"handle"`
				}{Handle: "h-http"}),
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	svc, err := New(Spec{Transport: TransportHTTP, URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := svc.New(context.Background(), "example.Echo/echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Call(context.Background(), handle, "echo", nil)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation", err)
	}
	if !svc.Healthy() {
		t.Fatal("a bare 4xx must not mark the service unhealthy")
	}
}

func TestSpecValidation(t *testing.T) {
	for _, spec := range []Spec{
		{},
		{Transport: TransportStdio},
		{Transport: TransportHTTP},
		{Transport: "tcp"},
	} {
		if _, err := New(spec); err == nil || !errdefs.IsValidation(err) {
			t.Errorf("spec %+v: err = %v, want Validation", spec, err)
		}
	}
}
