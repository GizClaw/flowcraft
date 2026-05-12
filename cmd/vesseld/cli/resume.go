package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// cmdResume is the operator-facing wrapper around POST
// /v1/vessels/{vessel}/resume. It runs out-of-process against a
// running vesseld daemon (unix socket or TCP) — it does not load
// configs or boot a Captain itself, so the daemon's authoritative
// CheckpointStore is the one consulted.
//
// Usage:
//
//	vesseld resume --vessel=NAME --run-id=ID [--socket=PATH | --addr=HOST:PORT --token=T]
//
// Exits 0 on a 2xx response (run accepted for resume), non-zero
// when the daemon returns an error or the connection fails. Prints
// the daemon's JSON response body verbatim to stdout so scripts can
// pipe it into jq.
func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	vesselName := fs.String("vessel", "", "vessel name owning the run")
	runID := fs.String("run-id", "", "run id to resume")
	socket := fs.String("socket", "", "unix socket path (mutually exclusive with --addr)")
	addr := fs.String("addr", "", "tcp addr (host:port) of the daemon API")
	token := fs.String("token", "", "bearer token for tcp transport (required with --addr)")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *vesselName == "" {
		return fmt.Errorf("vesseld resume: --vessel is required")
	}
	if *runID == "" {
		return fmt.Errorf("vesseld resume: --run-id is required")
	}
	if *socket == "" && *addr == "" {
		return fmt.Errorf("vesseld resume: one of --socket or --addr is required")
	}
	if *socket != "" && *addr != "" {
		return fmt.Errorf("vesseld resume: --socket and --addr are mutually exclusive")
	}
	if *addr != "" && *token == "" {
		return fmt.Errorf("vesseld resume: --token is required with --addr")
	}

	body, _ := json.Marshal(map[string]string{"run_id": *runID})

	var (
		client *http.Client
		url    string
	)
	if *socket != "" {
		client = newUnixHTTPClient(*socket, *timeout)
		url = "http://unix/v1/vessels/" + *vesselName + "/resume"
	} else {
		client = &http.Client{Timeout: *timeout}
		url = "http://" + *addr + "/v1/vessels/" + *vesselName + "/resume"
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("vesseld resume: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("vesseld resume: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	// Always echo the daemon's body so failure modes (NotFound,
	// NotAvailable, Validation) carry their structured detail
	// straight to operators / wrapping scripts.
	fmt.Println(strings.TrimRight(string(respBody), "\n"))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("vesseld resume: daemon returned %s", resp.Status)
	}
	return nil
}

// newUnixHTTPClient returns an http.Client whose transport dials
// the given unix socket path. The "host" portion of any URL the
// client receives is ignored — the dialer always targets the
// socket — so callers conventionally use http://unix/... to make
// the intent visible at the call site.
func newUnixHTTPClient(socketPath string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}
