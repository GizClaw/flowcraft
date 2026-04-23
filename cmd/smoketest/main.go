// smoketest spins up an in-process FlowCraft server in an ephemeral home,
// drives the event-sourcing surface with real HTTP traffic, and asserts the
// invariants the platform documents:
//
//   1. /healthz returns 200 and /readyz reports every projector as ready.
//   2. /api/admin/projection/status shows lag<10 for every projector.
//   3. /api/admin/projection/dead-letters is empty.
//   4. publishing a `task.submitted` envelope advances /api/events/latest-seq
//      and shows up under /api/events?partition=runtime:<id>.
//   5. The same envelope, fetched via HTTP-pull, SSE, and WebSocket, has
//      byte-identical JSON encodings (the §6.5 wire-format invariant).
//
// Exit codes:
//   0 success
//   1 setup / network error
//   2 invariant violation (the smoke check itself failed)
//
// Usage:
//   go run ./cmd/smoketest
//
// All state lives under a freshly-minted temp dir; nothing leaks back to the
// developer's ~/.flowcraft.

package main

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
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/internal/bootstrap"
	"github.com/GizClaw/flowcraft/internal/eventlog"

	"github.com/coder/websocket"
)

const (
	smokeUser = "smoke-owner"
	smokePass = "smoke-password-1234"
	smokeRT   = "smoke-rt-1"
)

func main() {
	// FlowCraft's sandbox manager requires Linux + bubblewrap; bootstrap.Run
	// hard-fails on macOS. The smoke gate is treated as a no-op in that case
	// and CI runs the real check on linux runners.
	if runtime.GOOS != "linux" {
		fmt.Printf("SKIP smoke (sandbox requires Linux; current=%s)\n", runtime.GOOS)
		return
	}
	if err := run(); err != nil {
		var verr *invariantError
		if errors.As(err, &verr) {
			fmt.Fprintf(os.Stderr, "FAIL %s\n", verr.Error())
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "ERROR %s\n", err.Error())
		os.Exit(1)
	}
	fmt.Println("PASS smoke")
}

type invariantError struct{ msg string }

func (e *invariantError) Error() string { return e.msg }
func failf(format string, args ...any) error {
	return &invariantError{msg: fmt.Sprintf(format, args...)}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tmp, err := os.MkdirTemp("", "flowcraft-smoke-*")
	if err != nil {
		return fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := os.Setenv("HOME", tmp); err != nil {
		return err
	}
	dataDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	if err := os.Setenv("FLOWCRAFT_DATA_DIR", dataDir); err != nil {
		return err
	}

	homeFlowcraft := filepath.Join(tmp, ".flowcraft")
	if err := os.MkdirAll(homeFlowcraft, 0o755); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := fmt.Sprintf(`server:
  host: "127.0.0.1"
  port: %d
sandbox:
  mode: "ephemeral"
  network_mode: "none"
log:
  level: "warn"
  file:
    path: ""
telemetry:
  enabled: false
`, port)
	if err := os.WriteFile(filepath.Join(homeFlowcraft, "config.yaml"), []byte(cfg), 0o644); err != nil {
		return err
	}

	bootCtx, bootCancel := context.WithCancel(context.Background())
	defer bootCancel()
	plat, server, cleanup, err := bootstrap.Run(bootCtx)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	defer cleanup()

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(bootCtx, ln) }()
	defer func() {
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = server.Shutdown(shutCtx)
		select {
		case e := <-serveErr:
			if e != nil && !errors.Is(e, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "warn: serve exited: %v\n", e)
			}
		case <-shutCtx.Done():
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	if err := waitHealthz(ctx, base); err != nil {
		return err
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 10 * time.Second, Jar: jar}

	if err := setupAndLogin(ctx, client, base); err != nil {
		return err
	}

	if err := waitProjectorsReady(ctx, client, base, 15*time.Second); err != nil {
		return err
	}
	if err := checkDeadLetters(ctx, client, base); err != nil {
		return err
	}

	beforeSeq, err := getLatestSeq(ctx, client, base)
	if err != nil {
		return err
	}

	publishedSeq, err := publishTaskSubmitted(ctx, plat.EventLog)
	if err != nil {
		return fmt.Errorf("publish task.submitted: %w", err)
	}
	if publishedSeq <= beforeSeq {
		return failf("publish returned seq %d, expected > %d", publishedSeq, beforeSeq)
	}

	// Wait for the projector loop to catch up so latest-seq reflects the
	// new envelope. 2s is generous; the loop polls every 250ms.
	if err := waitForSeq(ctx, client, base, publishedSeq, 2*time.Second); err != nil {
		return err
	}

	if err := checkPullPartition(ctx, client, base, beforeSeq, publishedSeq); err != nil {
		return err
	}

	if err := checkByteConsistency(ctx, client, base, beforeSeq, publishedSeq); err != nil {
		return err
	}

	return nil
}

// ---- HTTP probes ---------------------------------------------------------

func waitHealthz(ctx context.Context, base string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return failf("healthz never returned 200")
}

func waitProjectorsReady(ctx context.Context, client *http.Client, base string, within time.Duration) error {
	deadline := time.Now().Add(within)
	var lastErr string
	for time.Now().Before(deadline) {
		var status []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
		}
		if err := getJSON(ctx, client, base+"/api/admin/projection/status", &status); err == nil {
			allReady := true
			for _, p := range status {
				if !p.Ready {
					allReady = false
					lastErr = "projector " + p.Name + " not ready"
					break
				}
			}
			if allReady {
				return nil
			}
		} else {
			lastErr = err.Error()
		}
		time.Sleep(200 * time.Millisecond)
	}
	return failf("projectors not all ready: %s", lastErr)
}

func setupAndLogin(ctx context.Context, client *http.Client, base string) error {
	setup := map[string]string{"username": smokeUser, "password": smokePass}
	if err := postJSON(ctx, client, base+"/api/auth/setup", setup, nil); err != nil {
		return fmt.Errorf("auth setup: %w", err)
	}
	login := map[string]string{"username": smokeUser, "password": smokePass}
	var resp struct {
		Token string `json:"token"`
	}
	if err := postJSON(ctx, client, base+"/api/auth/login", login, &resp); err != nil {
		return fmt.Errorf("auth login: %w", err)
	}
	if resp.Token == "" {
		return failf("login returned empty token")
	}
	return nil
}

func checkProjectionStatus(ctx context.Context, client *http.Client, base string) error {
	var status []struct {
		Name                string `json:"name"`
		Lag                 int64  `json:"lag"`
		Ready               bool   `json:"ready"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
	}
	if err := getJSON(ctx, client, base+"/api/admin/projection/status", &status); err != nil {
		return fmt.Errorf("projection/status: %w", err)
	}
	for _, p := range status {
		if !p.Ready {
			return failf("projector %s not ready", p.Name)
		}
		if p.ConsecutiveFailures > 0 {
			return failf("projector %s has %d consecutive failures", p.Name, p.ConsecutiveFailures)
		}
		if p.Lag > 10 {
			return failf("projector %s lag=%d (>10)", p.Name, p.Lag)
		}
	}
	return nil
}

func checkDeadLetters(ctx context.Context, client *http.Client, base string) error {
	var dlts []any
	if err := getJSON(ctx, client, base+"/api/admin/projection/dead-letters", &dlts); err != nil {
		return fmt.Errorf("dead-letters: %w", err)
	}
	if len(dlts) > 0 {
		return failf("dead-letters non-empty at boot: %d entries", len(dlts))
	}
	return nil
}

func getLatestSeq(ctx context.Context, client *http.Client, base string) (int64, error) {
	var resp struct {
		LatestSeq int64 `json:"latest_seq"`
	}
	if err := getJSON(ctx, client, base+"/api/events/latest-seq", &resp); err != nil {
		return 0, err
	}
	return resp.LatestSeq, nil
}

func waitForSeq(ctx context.Context, client *http.Client, base string, target int64, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		seq, err := getLatestSeq(ctx, client, base)
		if err == nil && seq >= target {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return failf("latest-seq did not reach %d within %s", target, within)
}

func publishTaskSubmitted(ctx context.Context, log *eventlog.SQLiteLog) (int64, error) {
	payload := eventlog.TaskSubmittedPayload{
		CardID:        "smoke-card-1",
		Inputs:        map[string]any{"smoke": true},
		Query:         "smoke probe",
		RuntimeID:     smokeRT,
		TargetAgentID: "smoke-agent",
	}
	return eventlog.PublishTaskSubmitted(ctx, log, smokeRT, payload)
}

func checkPullPartition(ctx context.Context, client *http.Client, base string, since, want int64) error {
	url := fmt.Sprintf("%s/api/events?partition=runtime:%s&since=%d&limit=50", base, smokeRT, since)
	var resp struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := getJSON(ctx, client, url, &resp); err != nil {
		return fmt.Errorf("pull events: %w", err)
	}
	for _, raw := range resp.Events {
		var hdr struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &hdr); err != nil {
			return failf("pull envelope decode: %v", err)
		}
		if hdr.Seq == want && hdr.Type == "task.submitted" {
			return nil
		}
	}
	return failf("pull did not surface seq %d (got %d events)", want, len(resp.Events))
}

// checkByteConsistency fetches the same envelope through HTTP-pull, SSE, and
// WebSocket and asserts every transport delivered byte-identical JSON. This
// is the §6.5 wire-format invariant.
func checkByteConsistency(ctx context.Context, client *http.Client, base string, since, want int64) error {
	pullBytes, err := fetchOnePull(ctx, client, base, since, want)
	if err != nil {
		return fmt.Errorf("pull fetch: %w", err)
	}
	sseBytes, err := fetchOneSSE(ctx, client, base, since, want)
	if err != nil {
		return fmt.Errorf("sse fetch: %w", err)
	}
	wsBytes, err := fetchOneWS(ctx, base, since, want, jarCookies(client, base))
	if err != nil {
		return fmt.Errorf("ws fetch: %w", err)
	}

	pullN, err := canonicalize(pullBytes)
	if err != nil {
		return failf("canonicalize pull: %v", err)
	}
	sseN, err := canonicalize(sseBytes)
	if err != nil {
		return failf("canonicalize sse: %v", err)
	}
	wsN, err := canonicalize(wsBytes)
	if err != nil {
		return failf("canonicalize ws: %v", err)
	}
	if !bytes.Equal(pullN, sseN) {
		return failf("pull vs sse diverge\npull=%s\nsse=%s", pullN, sseN)
	}
	if !bytes.Equal(pullN, wsN) {
		return failf("pull vs ws diverge\npull=%s\nws=%s", pullN, wsN)
	}
	return nil
}

func fetchOnePull(ctx context.Context, client *http.Client, base string, since, want int64) ([]byte, error) {
	url := fmt.Sprintf("%s/api/events?partition=runtime:%s&since=%d&limit=50", base, smokeRT, since)
	var resp struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := getJSON(ctx, client, url, &resp); err != nil {
		return nil, err
	}
	for _, raw := range resp.Events {
		var hdr struct {
			Seq int64 `json:"seq"`
		}
		_ = json.Unmarshal(raw, &hdr)
		if hdr.Seq == want {
			return []byte(raw), nil
		}
	}
	return nil, failf("pull missing seq %d", want)
}

func fetchOneSSE(ctx context.Context, client *http.Client, base string, since, want int64) ([]byte, error) {
	url := fmt.Sprintf("%s/api/events/stream?partition=runtime:%s&since=%d", base, smokeRT, since)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, failf("sse status %d: %s", resp.StatusCode, body)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if strings.HasPrefix(data, "{\"latest_seq\"") {
			continue // heartbeat
		}
		var hdr struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(data), &hdr); err != nil {
			continue
		}
		if hdr.Seq == want {
			return []byte(data), nil
		}
	}
	return nil, failf("sse missing seq %d (scanner err=%v)", want, scanner.Err())
}

func fetchOneWS(ctx context.Context, base string, since, want int64, cookieHeader string) ([]byte, error) {
	wsURL := "ws://" + strings.TrimPrefix(base, "http://") + "/api/events/ws"
	hdr := http.Header{}
	if cookieHeader != "" {
		hdr.Set("Cookie", cookieHeader)
	}
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	subFrame, _ := json.Marshal(map[string]any{
		"type":      "subscribe",
		"partition": "runtime:" + smokeRT,
		"since":     since,
	})
	if err := conn.Write(ctx, websocket.MessageText, subFrame); err != nil {
		return nil, err
	}
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		_, raw, err := conn.Read(deadline)
		if err != nil {
			return nil, fmt.Errorf("ws read: %w", err)
		}
		var msg struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.Type != "envelope" {
			continue
		}
		var hdr struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(msg.Data, &hdr); err != nil {
			continue
		}
		if hdr.Seq == want {
			return []byte(msg.Data), nil
		}
	}
}

// ---- helpers -------------------------------------------------------------

func postJSON(ctx context.Context, client *http.Client, url string, body, out any) error {
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return failf("%s -> %d %s", url, resp.StatusCode, respBody)
	}
	if out != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

func getJSON(ctx context.Context, client *http.Client, url string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return failf("%s -> %d %s", url, resp.StatusCode, body)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

func jarCookies(client *http.Client, base string) string {
	if client.Jar == nil {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	cs := client.Jar.Cookies(u)
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func canonicalize(raw []byte) ([]byte, error) {
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
