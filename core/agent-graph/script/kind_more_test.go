package script

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/goccy/go-json"
)

func TestKindIsNamedForItsJob(t *testing.T) {
	kind := testKind(t, newFake(), memorySource{})
	if got := kind.Name(); got != "script" {
		t.Fatalf("name = %q, want script", got)
	}
}

// TestDecodeConfigIsReusedByTheInvocation covers the path the engine takes: Build
// decodes once, and every invocation reads the decoded plan instead of parsing the
// document again. Passing a deliberately broken config proves the decoded value is
// what the invocation used.
func TestDecodeConfigIsReusedByTheInvocation(t *testing.T) {
	kind := testKind(t, newFake(), memorySource{"echo": `{"prompt":"$prompt"}`})
	decoder, ok := kind.(graph.ConfigDecoder)
	if !ok {
		t.Fatal("the kind must decode its own config")
	}
	decoded, err := decoder.DecodeConfig(json.RawMessage(
		`{"backend":"fake","path":"echo","reads":["prompt"],"writes":["prompt"]}`))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	in := invoke("n", `{"backend":"nowhere","path":"x"}`, declared())
	in.Decoded = decoded
	result := drain(t, mustInvoke(t, kind, in))
	if len(result.State) != 1 {
		t.Fatalf("updates = %d, want the plan Build decoded", len(result.State))
	}
}

func TestProgramCacheIsBounded(t *testing.T) {
	sources := memorySource{}
	for i := 0; i < programCacheLimit+8; i++ {
		sources[pathFor(i)] = `{"prompt":"v` + string(rune('a'+i%26)) + `"}`
	}
	kind := testKind(t, newFake(), sources)
	for i := 0; i < programCacheLimit+8; i++ {
		drain(t, mustInvoke(t, kind, invoke("n",
			`{"backend":"fake","path":"`+pathFor(i)+`","reads":["prompt"],"writes":["prompt"]}`, declared())))
	}
	internal := kind.(*Kind)
	internal.mu.Lock()
	defer internal.mu.Unlock()
	if len(internal.programs) != programCacheLimit {
		t.Fatalf("cached programs = %d, want the limit %d", len(internal.programs), programCacheLimit)
	}
	if len(internal.recent) != programCacheLimit {
		t.Fatalf("eviction order = %d entries, want %d", len(internal.recent), programCacheLimit)
	}
}

func TestConcurrentInvocationsShareOneCompiledProgram(t *testing.T) {
	fake := newFake()
	kind := testKind(t, fake, memorySource{"echo": `{"prompt":"$prompt"}`})
	in := invoke("n", `{"backend":"fake","path":"echo","reads":["prompt"],"writes":["prompt"]}`, declared())
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := kind.Invoke(context.Background(), in)
			if err != nil {
				failures <- err
				return
			}
			for {
				if _, err := stream.Next(context.Background()); err != nil {
					break
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("invoke: %v", err)
	}
	internal := kind.(*Kind)
	internal.mu.Lock()
	defer internal.mu.Unlock()
	// Several invocations may compile the same version while the first is still
	// working; the invariant is that one program survives.
	if len(internal.programs) != 1 {
		t.Fatalf("cached programs = %d, want 1", len(internal.programs))
	}
}

func TestNewIgnoresAnUnusableOption(t *testing.T) {
	kind, err := New(
		WithBackends(nil, &fakeBackend{name: ""}, newFake()),
		WithSource(nil),
		WithLog(nil),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := kind.Name(); got != "script" {
		t.Fatalf("name = %q", got)
	}
}

func TestLogWithNoNodeStillWrites(t *testing.T) {
	var buf strings.Builder
	host := hostFor(graph.Invoke{}, &buf, nil, nil)
	host.Log(LogWarn, "no node here")
	if !strings.Contains(buf.String(), "script: no node here") {
		t.Fatalf("log = %q", buf.String())
	}
	if strings.Contains(buf.String(), `node ""`) {
		t.Fatalf("log = %q, want no empty node name", buf.String())
	}
}

func pathFor(i int) string {
	return "rule-" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + ".star"
}
