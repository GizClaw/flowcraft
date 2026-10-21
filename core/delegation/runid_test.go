package delegation

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// runIDBackend is an AsyncBackend that records every run id reported
// through RunIDNotifier, keyed by delegation id.
type runIDBackend struct {
	mu    sync.Mutex
	notes map[string][]string
}

func newRunIDBackend() *runIDBackend {
	return &runIDBackend{notes: make(map[string][]string)}
}

func (b *runIDBackend) Submit(context.Context, AsyncRequest) (string, error) {
	return "work-submitted", nil
}

func (b *runIDBackend) Status(context.Context, string) (Response, error) {
	return Response{Status: StatusRunning}, nil
}

func (b *runIDBackend) Claim(ctx context.Context) (Work, error) {
	<-ctx.Done()
	return Work{}, ctx.Err()
}

func (b *runIDBackend) Complete(context.Context, string, string, Response) error {
	return nil
}

func (b *runIDBackend) NoteRunID(_ context.Context, id, runID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notes[id] = append(b.notes[id], runID)
	return nil
}

func (b *runIDBackend) noted(id string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.notes[id]...)
}

func (b *runIDBackend) noteIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.notes))
	for id := range b.notes {
		ids = append(ids, id)
	}
	return ids
}

// newRunIDService binds a session manager and a notifying backend to the
// same service, the shape the runtime builder assembles.
func newRunIDService(t *testing.T, engine agent.Engine, backend AsyncBackend) *LocalService {
	t.Helper()
	result := buildResult(t, delegationWithRunEnd(engine))
	directory := NewDirectory()
	if err := directory.Bind(result); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(directory, backend,
		WithSessionProvider(&testSessionProvider{contextID: "runid-ctx"}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if err := service.BindSessionManager(newTestSessionManagerForResult(t, result)); err != nil {
		t.Fatal(err)
	}
	return service
}

// A queue-owned worker outside the service only has the request, so the
// claimed work id arrives through WithWorkID and the backend learns the
// run id as the run starts.
func TestServiceRunNotesRunIDForStampedWork(t *testing.T) {
	backend := newRunIDBackend()
	service := newRunIDService(t, completedEngine("from run"), backend)

	response, err := service.Run(WithWorkID(context.Background(), "work-1"), AsyncRequest{
		Request: Request{Mode: ModeAsync, Target: "writer", Input: "do it"},
		Depth:   1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if response.Status != StatusSucceeded || response.Output != "from run" {
		t.Fatalf("response = %+v, want succeeded output", response)
	}
	runID := response.Metadata[RunIDMetadataKey]
	if runID == "" {
		t.Fatalf("terminal metadata = %+v, want %s", response.Metadata, RunIDMetadataKey)
	}
	// Run returns the run id in ID; only the worker rewrites it to the
	// delegation id.
	if response.ID != runID {
		t.Fatalf("response id = %q, want the run id %q", response.ID, runID)
	}
	if notes := backend.noted("work-1"); len(notes) != 1 || notes[0] != runID {
		t.Fatalf("noted run ids for work-1 = %v, want [%s]", notes, runID)
	}
	if ids := backend.noteIDs(); len(ids) != 1 {
		t.Fatalf("noted delegation ids = %v, want only work-1", ids)
	}
}

// The work id belongs to one run: a delegation started inside the
// subagent must not report its run id against the parent's work item.
func TestServiceNestedDelegationDoesNotInheritWorkID(t *testing.T) {
	backend := newRunIDBackend()
	var service *LocalService
	var calls atomic.Int32
	nestedRun := make(chan string, 1)
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if calls.Add(1) == 1 {
			nested, err := service.Delegate(ctx, syncRequest("researcher"))
			if err != nil {
				t.Errorf("nested Delegate: %v", err)
			} else if nested.Status != StatusSucceeded {
				t.Errorf("nested status = %q, want succeeded", nested.Status)
			} else {
				nestedRun <- nested.Metadata[RunIDMetadataKey]
			}
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	service = newRunIDService(t, engine, backend)

	outer, err := service.Run(WithWorkID(context.Background(), "work-outer"), AsyncRequest{
		Request: Request{Mode: ModeAsync, Target: "writer", Input: "do it"},
		Depth:   1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	outerRunID := outer.Metadata[RunIDMetadataKey]
	if outerRunID == "" {
		t.Fatalf("terminal metadata = %+v, want the outer run id", outer.Metadata)
	}
	var innerRunID string
	select {
	case innerRunID = <-nestedRun:
	default:
		t.Fatal("subagent did not delegate")
	}
	if innerRunID == "" || innerRunID == outerRunID {
		t.Fatalf("nested run id = %q, want a distinct run id", innerRunID)
	}
	if notes := backend.noted("work-outer"); len(notes) != 1 || notes[0] != outerRunID {
		t.Fatalf("noted run ids for work-outer = %v, want [%s]", notes, outerRunID)
	}
	if ids := backend.noteIDs(); len(ids) != 1 {
		t.Fatalf("noted delegation ids = %v, want only work-outer", ids)
	}
}

// Sync delegations have no backend record to annotate, but the run id
// still reaches the caller through the terminal metadata.
func TestServiceSyncResponseCarriesRunIDMetadata(t *testing.T) {
	service, _ := newSessionPathService(t, completedEngine("done"), nil)
	response, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Metadata[RunIDMetadataKey] == "" {
		t.Fatalf("metadata = %+v, want %s", response.Metadata, RunIDMetadataKey)
	}
	if response.ID != response.Metadata[RunIDMetadataKey] {
		t.Fatalf("response id = %q, want the run id %q",
			response.ID, response.Metadata[RunIDMetadataKey])
	}
}
