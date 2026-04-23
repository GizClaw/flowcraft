// Package apitest spins up the WS hub, SSE hub, and HTTP pull endpoint over
// a real net/http test server so we can assert that all three transports
// emit byte-identical Envelope payloads.
package apitest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/api"
	"github.com/GizClaw/flowcraft/internal/api/ssehub"
	"github.com/GizClaw/flowcraft/internal/api/wshub"
	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
	"github.com/GizClaw/flowcraft/internal/store"
	"github.com/coder/websocket"
)

// Rig owns the eventlog, hubs, and httptest.Server. Tests Seed events through
// Rig.Log, then call Rig.PullPartition / Rig.SSEPartition / Rig.WSPartition
// to compare the bytes the three transports emit.
type Rig struct {
	tb       testing.TB
	Log      *eventlog.SQLiteLog
	WSHub    *wshub.Hub
	SSEHub   *ssehub.Hub
	Server   *httptest.Server
	policy   policy.Policy
	mux      *http.ServeMux
	closeFns []func()
	mu       sync.Mutex
}

// allowAllPolicy bypasses authorization so transport tests stay focused on
// envelope shape, not on policy regressions.
type allowAllPolicy struct{}

func (allowAllPolicy) AllowAppend(context.Context, policy.Actor, policy.EnvelopeDraft) (policy.Decision, error) {
	return policy.Allow, nil
}
func (allowAllPolicy) AllowSubscribe(context.Context, policy.Actor, policy.SubscribeOptions) (policy.Decision, error) {
	return policy.Allow, nil
}
func (allowAllPolicy) AllowRead(context.Context, policy.Actor, policy.ReadOptions) (policy.Decision, error) {
	return policy.Allow, nil
}

// NewRig builds a fresh test rig with a tmpfile SQLite log and three
// transports wired up.
func NewRig(tb testing.TB) *Rig {
	tb.Helper()
	dir := tb.TempDir()
	dsn := filepath.Join(dir, "events.db")
	st, err := store.NewSQLiteStore(context.Background(), dsn)
	if err != nil {
		tb.Fatalf("apitest: store: %v", err)
	}
	log := eventlog.NewSQLiteLog(st.DB())
	pol := allowAllPolicy{}
	wh := wshub.NewHub(log, pol, wshub.DefaultHubConfig)
	sh := ssehub.NewHub(log, pol, ssehub.DefaultHubConfig)
	wh.Start()
	sh.Start()

	r := &Rig{
		tb:     tb,
		Log:    log,
		WSHub:  wh,
		SSEHub: sh,
		policy: pol,
		mux:    http.NewServeMux(),
	}
	r.closeFns = append(r.closeFns, func() { st.Close() }, wh.Stop, sh.Stop)

	handler := api.NewEventsHandler(log, pol, nil)
	r.mux.HandleFunc("/api/events", func(w http.ResponseWriter, req *http.Request) {
		ctx := policy.WithActor(req.Context(), policy.Actor{Type: policy.ActorUser, ID: "u-test"})
		handler.GetEventsHTTP(w, req.WithContext(ctx))
	})
	r.mux.HandleFunc("/sse", r.serveSSE)
	r.mux.HandleFunc("/ws", r.serveWS)

	r.Server = httptest.NewServer(r.mux)
	r.closeFns = append(r.closeFns, r.Server.Close)
	tb.Cleanup(r.Stop)
	return r
}

// Stop releases all rig resources.
func (r *Rig) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.closeFns) - 1; i >= 0; i-- {
		r.closeFns[i]()
	}
	r.closeFns = nil
}

// Seed appends drafts to the log; convenience around log.Atomic.
func (r *Rig) Seed(drafts ...eventlog.EnvelopeDraft) []eventlog.Envelope {
	r.tb.Helper()
	envs, err := r.Log.Atomic(context.Background(), func(uow eventlog.UnitOfWork) error {
		return uow.Append(context.Background(), drafts...)
	})
	if err != nil {
		r.tb.Fatalf("apitest: seed: %v", err)
	}
	return envs
}

// PullPartition fetches one page from the HTTP-pull endpoint and returns the
// raw JSON for each envelope (eventlog.MarshalEnvelope output).
func (r *Rig) PullPartition(partition string, since int64, limit int) [][]byte {
	r.tb.Helper()
	u := fmt.Sprintf("%s/api/events?partition=%s&since=%d&limit=%d",
		r.Server.URL, partition, since, limit)
	resp, err := http.Get(u)
	if err != nil {
		r.tb.Fatalf("apitest: pull: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.tb.Fatalf("apitest: pull status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// The handler emits {"events":[<env>,<env>...],"next_since":N,"has_more":..}.
	// Re-parse to extract per-envelope raw JSON without re-serializing.
	var raw struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		r.tb.Fatalf("apitest: parse pull: %v", err)
	}
	out := make([][]byte, len(raw.Events))
	for i, e := range raw.Events {
		out[i] = []byte(e)
	}
	return out
}

// SSEPartition opens an SSE stream and reads back N envelopes' data
// fields. Frames carrying `event: heartbeat` are skipped — only frames
// preceded by `event: envelope` count toward N. Comments (lines starting
// with `:`) are also dropped.
func (r *Rig) SSEPartition(partition string, since int64, n int, timeout time.Duration) [][]byte {
	r.tb.Helper()
	u := fmt.Sprintf("%s/sse?partition=%s&since=%d", r.Server.URL, partition, since)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.tb.Fatalf("apitest: sse: %v", err)
	}
	defer resp.Body.Close()
	deadline := time.Now().Add(timeout)
	out := make([][]byte, 0, n)
	br := bufReader{r: resp.Body}
	currentEvent := ""
	for len(out) < n && time.Now().Before(deadline) {
		line, err := br.readLine()
		if err != nil {
			break
		}
		if strings.HasPrefix(line, ":") || line == "" {
			currentEvent = ""
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") && currentEvent == "envelope" {
			out = append(out, []byte(strings.TrimPrefix(line, "data: ")))
		}
	}
	return out
}

// WSPartition opens a WebSocket, sends a `subscribe` control frame, and
// reads back N envelope payloads. The data field of the
// {"type":"envelope","data":<env>} frame is what we return — byte-equal
// to the SSE / HTTP-pull `MarshalEnvelope` outputs (§6.5).
func (r *Rig) WSPartition(partition string, since int64, n int, timeout time.Duration) [][]byte {
	r.tb.Helper()
	u := strings.Replace(r.Server.URL, "http", "ws", 1) + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		r.tb.Fatalf("apitest: ws dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	subFrame, _ := json.Marshal(map[string]any{
		"type":      "subscribe",
		"partition": partition,
		"since":     since,
	})
	if err := c.Write(ctx, websocket.MessageText, subFrame); err != nil {
		r.tb.Fatalf("apitest: ws subscribe: %v", err)
	}
	out := make([][]byte, 0, n)
	for len(out) < n {
		_, msg, err := c.Read(ctx)
		if err != nil {
			break
		}
		var frame struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}
		if frame.Type != "envelope" {
			continue
		}
		out = append(out, []byte(frame.Data))
	}
	return out
}

func (r *Rig) serveSSE(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	partition := req.URL.Query().Get("partition")
	since, _ := strconv.ParseInt(req.URL.Query().Get("since"), 10, 64)
	conn, err := r.SSEHub.Open(req.Context(), policy.Actor{Type: policy.ActorUser, ID: "u-test"}, partition, since, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	conn.Run()
}

func (r *Rig) serveWS(w http.ResponseWriter, req *http.Request) {
	c, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn, err := r.WSHub.Open(req.Context(), policy.Actor{Type: policy.ActorUser, ID: "u-test"})
	if err != nil {
		c.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	conn.Attach(c)
}

// bufReader is a minimal buffered line reader for SSE; we don't pull in
// bufio because this is the only place we need it.
type bufReader struct {
	r   io.Reader
	buf []byte
}

func (b *bufReader) readLine() (string, error) {
	tmp := make([]byte, 1024)
	for {
		if i := indexByte(b.buf, '\n'); i >= 0 {
			line := string(b.buf[:i])
			b.buf = b.buf[i+1:]
			return strings.TrimRight(line, "\r"), nil
		}
		n, err := b.r.Read(tmp)
		if n > 0 {
			b.buf = append(b.buf, tmp[:n]...)
		}
		if err != nil {
			if len(b.buf) > 0 {
				line := string(b.buf)
				b.buf = nil
				return line, nil
			}
			return "", err
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
