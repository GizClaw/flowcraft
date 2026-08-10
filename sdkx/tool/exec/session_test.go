package exec_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdkx/tool/exec"
)

func TestNewSession_NilManager_Rejected(t *testing.T) {
	if _, err := exec.NewSession(nil); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("NewSession(nil) = %v, want Validation (deny-by-default)", err)
	}
}

func TestNewSession_FakeManager_OK(t *testing.T) {
	tl, err := exec.NewSession(&fakePM{})
	if err != nil {
		t.Fatalf("NewSession(fake): %v", err)
	}
	defer func() { _ = tl.Close() }()
	if tl == nil {
		t.Fatal("NewSession returned nil tool")
	}
}

func TestSessionDefinition_Shape(t *testing.T) {
	tl, err := exec.NewSession(&fakePM{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()

	def := tl.Definition()
	if def.Name != exec.SessionName {
		t.Fatalf("Definition.Name = %q, want %q", def.Name, exec.SessionName)
	}
	var schema map[string]any
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("InputSchema not valid JSON: %v", err)
	}
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "action" {
		t.Fatalf("required = %v, want [action]", required)
	}
	if got := schema["additionalProperties"]; got != false {
		t.Fatalf("additionalProperties = %v, want false", got)
	}
	props, _ := schema["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	enums, _ := action["enum"].([]any)
	if len(enums) != 8 {
		t.Fatalf("action enum = %v, want 8 actions", enums)
	}
	signalProp, _ := props["signal"].(map[string]any)
	signalEnums, _ := signalProp["enum"].([]any)
	if len(signalEnums) != 1 || signalEnums[0] != "interrupt" {
		t.Fatalf("signal enum = %v, want [interrupt]", signalEnums)
	}
}

func TestSessionExecute_UnknownAction_Validation(t *testing.T) {
	tl, err := exec.NewSession(&fakePM{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()
	_, err = tl.Execute(context.Background(), `{"action":"explode"}`)
	if !errdefs.IsValidation(err) {
		t.Fatalf("unknown action = %v, want Validation", err)
	}
}

func TestSessionExecute_Start_RequiresCommand(t *testing.T) {
	tl, err := exec.NewSession(&fakePM{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()
	_, err = tl.Execute(context.Background(), `{"action":"start","command":"  "}`)
	if !errdefs.IsValidation(err) {
		t.Fatalf("blank command = %v, want Validation", err)
	}
}

func TestSessionExecute_Read_UnknownSession_NotFound(t *testing.T) {
	tl, err := exec.NewSession(&fakePM{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()
	_, err = tl.Execute(context.Background(), `{"action":"read","session_id":"nope"}`)
	if !errdefs.IsNotFound(err) {
		t.Fatalf("unknown session read = %v, want NotFound", err)
	}
	_, err = tl.Execute(context.Background(), `{"action":"read"}`)
	if !errdefs.IsValidation(err) {
		t.Fatalf("missing session_id = %v, want Validation", err)
	}
}

func TestSessionExecute_Write_EmptyData_Validation(t *testing.T) {
	tl, err := exec.NewSession(&fakePM{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()
	start, err := tl.Execute(context.Background(), `{"action":"start","command":"sh"}`)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := sessionID(t, start)
	_, err = tl.Execute(context.Background(), `{"action":"write","session_id":"`+id+`","data":""}`)
	if !errdefs.IsValidation(err) {
		t.Fatalf("empty write = %v, want Validation", err)
	}
}

func TestSessionExecute_Resize_Validation(t *testing.T) {
	tl, err := exec.NewSession(&fakePM{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()
	start, err := tl.Execute(context.Background(), `{"action":"start","command":"sh"}`)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := sessionID(t, start)
	_, err = tl.Execute(context.Background(), `{"action":"resize","session_id":"`+id+`","rows":0,"cols":80}`)
	if !errdefs.IsValidation(err) {
		t.Fatalf("bad resize = %v, want Validation", err)
	}
}

func TestSessionExecute_Signal_UnsupportedBackend(t *testing.T) {
	tl, err := exec.NewSession(&fakePM{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()
	start, err := tl.Execute(context.Background(), `{"action":"start","command":"sh"}`)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := sessionID(t, start)
	_, err = tl.Execute(context.Background(), `{"action":"signal","session_id":"`+id+`"}`)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("signal on unsupported backend = %v, want NotAvailable", err)
	}
	_, err = tl.Execute(context.Background(), `{"action":"signal","session_id":"`+id+`","signal":"term"}`)
	if !errdefs.IsValidation(err) {
		t.Fatalf("unknown signal = %v, want Validation", err)
	}
}

func TestSessionExecute_Signal_E2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty/process sessions are unix-only")
	}
	rn := sandbox.NewLocalRunner(t.TempDir())
	tl, err := exec.NewSession(sandbox.ProcessManagerOf(rn))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()

	start, err := tl.Execute(context.Background(),
		`{"action":"start","command":"/bin/sh","args":["-c","trap 'echo caught; exit 0' INT; echo ready; read x"]}`)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := sessionID(t, start)

	var sb strings.Builder
	var seq int64
	for !strings.Contains(sb.String(), "ready") {
		out := readSessionAction(t, tl, id, seq)
		for _, ch := range out.Chunks {
			sb.WriteString(ch.Data)
		}
		seq = out.NextSeq
		if out.EOF {
			break
		}
	}
	if _, err := tl.Execute(context.Background(),
		`{"action":"signal","session_id":"`+id+`"}`); err != nil {
		t.Fatalf("signal: %v", err)
	}
	for !strings.Contains(sb.String(), "caught") {
		out := readSessionAction(t, tl, id, seq)
		for _, ch := range out.Chunks {
			sb.WriteString(ch.Data)
		}
		seq = out.NextSeq
		if out.EOF {
			break
		}
	}
	if !strings.Contains(sb.String(), "caught") {
		t.Fatalf("signal did not interrupt the session: %q", sb.String())
	}
	st := waitSessionStatus(t, tl, id, false, 5*time.Second)
	if st.Reason != "exited" || st.ExitCode != 0 {
		t.Fatalf("status = %+v, want exited(0)", st)
	}
	closeSessionAction(t, tl, id)
}

func TestSessionExecute_Close_TerminatesSessions(t *testing.T) {
	pm := &fakePM{}
	tl, err := exec.NewSession(pm)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	start, err := tl.Execute(context.Background(), `{"action":"start","command":"sh"}`)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := sessionID(t, start)
	if err := tl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(pm.closed) != 1 || pm.closed[0] != id {
		t.Fatalf("closed sessions = %v, want [%s]", pm.closed, id)
	}
	if _, err := tl.Execute(context.Background(), `{"action":"status","session_id":"`+id+`"}`); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Execute after Close = %v, want NotAvailable", err)
	}
}

func TestSessionExecute_E2E_PipeSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty/process sessions are unix-only")
	}
	rn := sandbox.NewLocalRunner(t.TempDir())
	pm := sandbox.ProcessManagerOf(rn)
	if pm == nil {
		t.Fatal("LocalRunner must implement ProcessManager")
	}
	tl, err := exec.NewSession(pm)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()

	start, err := tl.Execute(context.Background(),
		`{"action":"start","command":"/bin/sh","args":["-c","printf OUT; printf ERR >&2; exit 7"]}`)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := sessionID(t, start)

	var stdout, stderr strings.Builder
	var seq int64
	for {
		out := readSessionAction(t, tl, id, seq)
		for _, ch := range out.Chunks {
			switch ch.Stream {
			case "stdout":
				stdout.WriteString(ch.Data)
			case "stderr":
				stderr.WriteString(ch.Data)
			}
		}
		seq = out.NextSeq
		if out.EOF {
			break
		}
	}
	if stdout.String() != "OUT" || stderr.String() != "ERR" {
		t.Fatalf("stdout=%q stderr=%q, want OUT/ERR", stdout.String(), stderr.String())
	}

	st := waitSessionStatus(t, tl, id, false, 5*time.Second)
	if st.ExitCode != 7 || st.Reason != "exited" {
		t.Fatalf("status = %+v, want exited(7)", st)
	}
	closeSessionAction(t, tl, id)
}

func TestSessionExecute_E2E_TTYSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty/process sessions are unix-only")
	}
	rn := sandbox.NewLocalRunner(t.TempDir())
	tl, err := exec.NewSession(sandbox.ProcessManagerOf(rn))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()

	start, err := tl.Execute(context.Background(),
		`{"action":"start","command":"/bin/sh","tty":true,"rows":24,"cols":80}`)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := sessionID(t, start)

	if _, err := tl.Execute(context.Background(),
		`{"action":"write","session_id":"`+id+`","data":"printf ok\n"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	var sb strings.Builder
	var seq int64
	for !strings.Contains(sb.String(), "ok") {
		out := readSessionAction(t, tl, id, seq)
		for _, ch := range out.Chunks {
			sb.WriteString(ch.Data)
		}
		seq = out.NextSeq
		if out.EOF {
			break
		}
	}
	if !strings.Contains(sb.String(), "ok") {
		t.Fatalf("TTY output missing 'ok': %q", sb.String())
	}
	if _, err := tl.Execute(context.Background(),
		`{"action":"resize","session_id":"`+id+`","rows":40,"cols":120}`); err != nil {
		t.Fatalf("resize: %v", err)
	}
	st := waitSessionStatus(t, tl, id, true, 5*time.Second)
	if !st.Running {
		t.Fatalf("session should still be running: %+v", st)
	}
	if _, err := tl.Execute(context.Background(),
		`{"action":"terminate","session_id":"`+id+`"}`); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	st = waitSessionStatus(t, tl, id, false, 5*time.Second)
	if st.Reason != "terminated" {
		t.Fatalf("reason = %q, want terminated", st.Reason)
	}
	closeSessionAction(t, tl, id)
}

func TestSessionExecute_TTL_ExpiresIdleSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty/process sessions are unix-only")
	}
	rn := sandbox.NewLocalRunner(t.TempDir())
	pm := sandbox.ProcessManagerOf(rn)
	tl, err := exec.NewSession(pm, exec.WithSessionTTL(50*time.Millisecond))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tl.Close() }()

	start, err := tl.Execute(context.Background(),
		`{"action":"start","command":"/bin/sleep","args":["30"]}`)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := sessionID(t, start)

	deadline := time.Now().Add(5 * time.Second)
	for {
		infos, err := pm.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(infos) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle session was not expired by TTL: %+v", infos)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The tool's own map must forget it too: status now fails NotFound.
	if _, err := tl.Execute(context.Background(), `{"action":"status","session_id":"`+id+`"}`); !errdefs.IsNotFound(err) {
		t.Fatalf("status after TTL expiry = %v, want NotFound", err)
	}
}

// sessionReadResult / sessionStatusResult mirror the session tool's
// wire output.
type sessionReadResult struct {
	NextSeq int64 `json:"next_seq"`
	EOF     bool  `json:"eof"`
	Chunks  []struct {
		Seq    int64  `json:"seq"`
		Stream string `json:"stream"`
		Data   string `json:"data"`
	} `json:"chunks"`
}

type sessionStatusResult struct {
	Running  bool     `json:"running"`
	ExitCode int      `json:"exit_code"`
	Reason   string   `json:"reason"`
	Signal   int      `json:"signal"`
	PID      int      `json:"pid"`
	TTY      bool     `json:"tty"`
	Argv     []string `json:"argv"`
}

func sessionID(t *testing.T, start string) string {
	t.Helper()
	var got struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(start), &got); err != nil {
		t.Fatalf("start result not valid JSON %q: %v", start, err)
	}
	if got.SessionID == "" {
		t.Fatalf("start result missing session_id: %q", start)
	}
	return got.SessionID
}

func readSessionAction(t *testing.T, tl *exec.SessionTool, id string, afterSeq int64) sessionReadResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := tl.Execute(ctx, fmt.Sprintf(
		`{"action":"read","session_id":"%s","after_seq":%d,"max_bytes":4096}`, id, afterSeq))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out sessionReadResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("read result not valid JSON %q: %v", raw, err)
	}
	return out
}

func waitSessionStatus(t *testing.T, tl *exec.SessionTool, id string, wantRunning bool, timeout time.Duration) sessionStatusResult {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		raw, err := tl.Execute(context.Background(), `{"action":"status","session_id":"`+id+`"}`)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		var st sessionStatusResult
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatalf("status result not valid JSON %q: %v", raw, err)
		}
		if st.Running == wantRunning {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("status did not reach running=%v: %+v", wantRunning, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func closeSessionAction(t *testing.T, tl *exec.SessionTool, id string) {
	t.Helper()
	if _, err := tl.Execute(context.Background(), `{"action":"close","session_id":"`+id+`"}`); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// fakePM records starts/closes for unit-level session-tool tests.
type fakePM struct {
	closed []string
}

func (f *fakePM) Start(_ context.Context, spec sandbox.ProcessSpec) (sandbox.Process, error) {
	if len(spec.Argv) == 0 {
		return nil, errdefs.Validationf("fake: empty argv")
	}
	return &fakeProcess{id: "fake-" + spec.Argv[0], pm: f}, nil
}

func (f *fakePM) List(context.Context) ([]sandbox.ProcessInfo, error) {
	return nil, nil
}

func (f *fakePM) Terminate(context.Context, string) error {
	return nil
}

type fakeProcess struct {
	id string
	pm *fakePM
}

func (p *fakeProcess) ID() string { return p.id }
func (p *fakeProcess) PID() int   { return 42 }
func (p *fakeProcess) Read(context.Context, int64, int) (sandbox.ProcessOutput, error) {
	return sandbox.ProcessOutput{}, nil
}
func (p *fakeProcess) Write(context.Context, []byte) error { return nil }
func (p *fakeProcess) Resize(context.Context, int, int) error {
	return nil
}
func (p *fakeProcess) Terminate(context.Context) error { return nil }
func (p *fakeProcess) Wait(context.Context) (sandbox.ProcessExit, error) {
	return sandbox.ProcessExit{}, nil
}
func (p *fakeProcess) Close() error {
	p.pm.closed = append(p.pm.closed, p.id)
	return nil
}
