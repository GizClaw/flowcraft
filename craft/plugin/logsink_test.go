package plugin

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// logSink records the lines the package emits, so a test can assert on
// what was reported rather than only on what was returned. It is the
// shape craft/pool's helper uses: a processor that clones each record
// and keeps its body and attributes.
type logSink struct {
	mu      sync.Mutex
	records []logRecord
}

type logRecord struct {
	body  string
	attrs map[string]string
}

func (s *logSink) Enabled(context.Context, sdklog.EnabledParameters) bool {
	return true
}

func (s *logSink) OnEmit(_ context.Context, record *sdklog.Record) error {
	clone := record.Clone()
	attrs := make(map[string]string, clone.AttributesLen())
	clone.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = valueString(kv.Value)
		return true
	})
	s.mu.Lock()
	s.records = append(s.records, logRecord{
		body:  clone.Body().AsString(),
		attrs: attrs,
	})
	s.mu.Unlock()
	return nil
}

func (s *logSink) Shutdown(context.Context) error   { return nil }
func (s *logSink) ForceFlush(context.Context) error { return nil }

// find returns every recorded line whose body contains text.
func (s *logSink) find(text string) []logRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []logRecord
	for _, r := range s.records {
		if strings.Contains(r.body, text) {
			out = append(out, r)
		}
	}
	return out
}

// installLogSink points the process-wide logger at a recording sink.
// The provider is global, so a test that installs one must not run in
// parallel with another that does; the sink itself is safe for the
// background goroutines that emit while a test waits.
func installLogSink(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	shutdown, err := telemetry.InitLog(context.Background(),
		telemetry.WithLogProcessor(sink))
	if err != nil {
		t.Fatalf("init log: %v", err)
	}
	telemetry.Enable()
	t.Cleanup(func() {
		_ = shutdown(context.Background())
	})
	return sink
}

// valueString renders one attribute value the way the log line's reader
// sees it.
func valueString(v attribute.Value) string {
	switch v.Type() {
	case attribute.STRING:
		return v.AsString()
	case attribute.BOOL:
		return strconv.FormatBool(v.AsBool())
	case attribute.INT64:
		return strconv.FormatInt(v.AsInt64(), 10)
	default:
		return v.String()
	}
}
