// Package agenttest is the conformance suite for the agent's storage contract.
//
// It exists because the behaviours a Store promises are subtle — a seed that is
// written once, events that only append, two version counters, a latest task per
// session — and a backend that gets one of them wrong only shows it as a
// resume that quietly loses a message. The suite is the one place those promises
// are written down; a new backend (a Postgres sibling of the SQLite one) runs it
// instead of re-deriving them.
package agenttest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// Store runs the conformance suite against one backend. open must return a fresh,
// empty store for each call.
func Store(t *testing.T, open func(t *testing.T) agent.Store) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, func(*testing.T) agent.Store)
	}{
		{"MissingTaskIsNotACheckpoint", missingTask},
		{"FirstSaveCreatesTheTask", firstSave},
		{"EventsOnlyAppend", eventsOnlyAppend},
		{"ProjectionCarriesNoHistory", projectionCarriesNoHistory},
		{"LoadDoesNotReplay", loadDoesNotReplay},
		{"EventsAfterReturnsTheSuffix", eventsAfterReturnsTheSuffix},
		{"TruncateKeepsTheTail", truncateKeepsTheTail},
		{"TruncateRefusesTheTail", truncateRefusesTheTail},
		{"SetServerVersionLeavesTheProjectionAlone", setServerVersionOnly},
		{"VersionAdvancesPerSave", versionAdvances},
		{"ServerVersionRoundTrips", serverVersionRoundTrips},
		{"LatestForSessionPicksTheNewest", latestForContext},
		{"ListIsNewestFirst", listIsNewestFirst},
		{"DeleteRemovesTaskAndEvents", deleteRemovesTaskAndEvents},
		{"SectionsSurviveLaterSaves", sectionsSurviveLaterSaves},
		{"BlobIsContentAddressed", blobIsContentAddressed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, open) })
	}
}

// task is a checkpoint with one seed message and the given identity.
func task(id, contextID string, round int) agent.Checkpoint {
	return agent.Checkpoint{
		Schema:  agent.CheckpointSchema,
		Runtime: "sha256:test",
		Task: agent.Task{
			ID:        id,
			SessionID: contextID,
			State:     agent.StateWorking,
			Round:     round,
			History: []message.Message{
				message.NewTextMessage(message.RoleUser, "write the report"),
			},
		},
	}
}

// messageEvent is one appended fact, encoded the way the loop encodes it.
func messageEvent(t *testing.T, text string) agent.EventRecord {
	t.Helper()
	body, err := json.Marshal(message.NewTextMessage(message.RoleAssistant, text))
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return agent.EventRecord{Kind: "message", Payload: body}
}

func missingTask(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	if _, err := store.Load(ctx, "absent"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Fatalf("Load(absent) = %v, want ErrNoCheckpoint", err)
	}
	if _, err := store.LatestForSession(ctx, "absent"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Fatalf("LatestForSession(absent) = %v, want ErrNoCheckpoint", err)
	}
}

func firstSave(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	want := task("task-1", "conv-1", 0)
	if _, _, err := store.Save(ctx, want, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Schema != want.Schema || got.Runtime != want.Runtime {
		t.Errorf("identity = %d/%s, want %d/%s",
			got.Schema, got.Runtime, want.Schema, want.Runtime)
	}
	if got.Task.State != want.Task.State || got.Task.Round != want.Task.Round {
		t.Errorf("task = %s/%d, want %s/%d",
			got.Task.State, got.Task.Round, want.Task.State, want.Task.Round)
	}
	if len(got.Task.History) != 0 {
		t.Errorf("history = %d messages, want the projection to carry none", len(got.Task.History))
	}
}

func eventsOnlyAppend(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 0), []agent.EventRecord{messageEvent(t, "one")}); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 1), []agent.EventRecord{messageEvent(t, "two")}); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	events, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].Seq >= events[i].Seq {
			t.Errorf("sequences = %d, %d, want increasing", events[i-1].Seq, events[i].Seq)
		}
	}
	if events[0].Kind != "message" {
		t.Errorf("kind = %q, want message", events[0].Kind)
	}
}

// A projection never carries history: what the caller passes in Task.History is
// ignored, because the log is its only source.
func projectionCarriesNoHistory(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	seed := task("task-1", "conv-1", 0)
	seed.Task.History = []message.Message{message.NewTextMessage(message.RoleUser, "write it")}
	if _, _, err := store.Save(ctx, seed, []agent.EventRecord{messageEvent(t, "one")}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Task.History) != 0 {
		t.Fatalf("history = %d messages, want none in the projection", len(loaded.Task.History))
	}
	events, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want the one written", len(events))
	}
}

// Load is the projection only: messages are not replayed into it.
func loadDoesNotReplay(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 0), nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, _, err := store.Save(ctx, task("task-1", "conv-1", i+1), []agent.EventRecord{messageEvent(t, "x")}); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	loaded, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Task.History) != 0 {
		t.Fatalf("history = %d messages, want the projection only", len(loaded.Task.History))
	}
	events, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("events = %d, want 5", len(events))
	}
}

// EventsAfter returns the suffix, exclusive of the bound.
func eventsAfterReturnsTheSuffix(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 0), nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	batch := make([]agent.EventRecord, 0, 5)
	for i := 0; i < 5; i++ {
		batch = append(batch, messageEvent(t, "x"))
	}
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 5), batch); err != nil {
		t.Fatalf("Save: %v", err)
	}
	events, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("events = %d, want 5", len(events))
	}
	suffix, err := store.EventsAfter(ctx, "task-1", events[1].Seq)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	if len(suffix) != 3 {
		t.Fatalf("suffix = %d events, want 3", len(suffix))
	}
	if suffix[0].Seq != events[2].Seq {
		t.Errorf("suffix starts at %d, want %d", suffix[0].Seq, events[2].Seq)
	}
	tail, err := store.EventsAfter(ctx, "task-1", events[len(events)-1].Seq)
	if err != nil {
		t.Fatalf("EventsAfter tail: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("tail = %d events, want none", len(tail))
	}
}

// Truncation keeps the tail: the newest event is never removable, so a later
// Save continues past it instead of reusing an identifier.
func truncateKeepsTheTail(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 0), nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	batch := make([]agent.EventRecord, 0, 5)
	for i := 0; i < 5; i++ {
		batch = append(batch, messageEvent(t, "x"))
	}
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 5), batch); err != nil {
		t.Fatalf("Save: %v", err)
	}
	stored, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if err := store.TruncateEvents(ctx, "task-1", stored[2].Seq); err != nil {
		t.Fatalf("TruncateEvents: %v", err)
	}
	kept, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events after truncate: %v", err)
	}
	if len(kept) != 2 {
		t.Fatalf("kept = %d events, want the two after the cut", len(kept))
	}
	if kept[0].Seq != stored[3].Seq {
		t.Errorf("first kept seq = %d, want %d", kept[0].Seq, stored[3].Seq)
	}
	first, last, err := store.Save(ctx, task("task-1", "conv-1", 6), []agent.EventRecord{messageEvent(t, "y")})
	if err != nil {
		t.Fatalf("Save after truncate: %v", err)
	}
	if first != stored[4].Seq+1 || last != first {
		t.Errorf("new seq range = %d..%d, want %d", first, last, stored[4].Seq+1)
	}
}

// The adapter's counter advances without the projection being rewritten: the
// engine owns the state, the adapter owns only its token.
func setServerVersionOnly(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	seed := task("task-1", "conv-1", 0)
	if _, _, err := store.Save(ctx, seed, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := store.SetServerVersion(ctx, "task-1", 9); err != nil {
		t.Fatalf("SetServerVersion: %v", err)
	}
	after, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load after: %v", err)
	}
	if after.ServerVersion != 9 {
		t.Errorf("server version = %d, want 9", after.ServerVersion)
	}
	if after.Version != before.Version {
		t.Errorf("version %d -> %d; the projection write counter moved", before.Version, after.Version)
	}
	if after.Task.State != before.Task.State || after.Task.Round != before.Task.Round {
		t.Errorf("the projection changed: %+v -> %+v", before.Task, after.Task)
	}
	if err := store.SetServerVersion(ctx, "absent", 1); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Errorf("SetServerVersion(absent) = %v, want ErrNoCheckpoint", err)
	}
}

// Refusing to remove the newest event is what keeps sequence numbers monotonic.
func truncateRefusesTheTail(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 0), []agent.EventRecord{messageEvent(t, "x")}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	events, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if err := store.TruncateEvents(ctx, "task-1", events[0].Seq); err == nil {
		t.Fatal("truncating the newest event was accepted")
	}
	kept, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(kept) != 1 {
		t.Errorf("events = %d, want the tail preserved", len(kept))
	}
}

func versionAdvances(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 0), nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	first, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 1), nil); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	second, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if second.Version <= first.Version {
		t.Fatalf("version %d -> %d, want an increase", first.Version, second.Version)
	}
}

func serverVersionRoundTrips(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	cp := task("task-1", "conv-1", 0)
	cp.ServerVersion = 7
	if _, _, err := store.Save(ctx, cp, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ServerVersion != 7 {
		t.Errorf("server version = %d, want 7", loaded.ServerVersion)
	}
}

func latestForContext(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, task("old", "conv-1", 0), nil); err != nil {
		t.Fatalf("Save old: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // updated_at must be able to differ
	if _, _, err := store.Save(ctx, task("new", "conv-1", 0), nil); err != nil {
		t.Fatalf("Save new: %v", err)
	}
	if _, _, err := store.Save(ctx, task("other", "conv-2", 0), nil); err != nil {
		t.Fatalf("Save other: %v", err)
	}
	latest, err := store.LatestForSession(ctx, "conv-1")
	if err != nil {
		t.Fatalf("LatestForSession: %v", err)
	}
	if latest.Task.ID != "new" {
		t.Errorf("latest = %q, want new", latest.Task.ID)
	}
}

func listIsNewestFirst(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, task("a", "conv-1", 0), nil); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, _, err := store.Save(ctx, task("b", "conv-1", 0), nil); err != nil {
		t.Fatalf("Save b: %v", err)
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d tasks, want 2", len(list))
	}
	if list[0].ID != "b" || list[1].ID != "a" {
		t.Errorf("list = %s, %s; want newest first", list[0].ID, list[1].ID)
	}
	if list[0].State != agent.StateWorking || list[0].Round != 0 {
		t.Errorf("summary = %+v, want the projection", list[0])
	}
}

func deleteRemovesTaskAndEvents(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	if _, _, err := store.Save(ctx, task("task-1", "conv-1", 0), []agent.EventRecord{messageEvent(t, "x")}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(ctx, "task-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(ctx, "task-1"); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Errorf("Load after Delete = %v, want ErrNoCheckpoint", err)
	}
	events, err := store.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events after Delete: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("events after Delete = %d, want none", len(events))
	}
}

// Sections are owner-keyed state an adapter above the agent writes on every save;
// they must not be frozen at the first one.
func sectionsSurviveLaterSaves(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	first := task("task-1", "conv-1", 0)
	first.Sections = map[string]json.RawMessage{"a2a": json.RawMessage(`{"context_id":"one"}`)}
	if _, _, err := store.Save(ctx, first, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	second := task("task-1", "conv-1", 1)
	second.Sections = map[string]json.RawMessage{"a2a": json.RawMessage(`{"context_id":"two"}`)}
	if _, _, err := store.Save(ctx, second, nil); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	loaded, err := store.Load(ctx, "task-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := string(loaded.Sections["a2a"]); got != `{"context_id":"two"}` {
		t.Fatalf("section = %s, want the second save's value", got)
	}
}

func blobIsContentAddressed(t *testing.T, open func(*testing.T) agent.Store) {
	store := open(t)
	ctx := context.Background()
	first, err := store.PutBlob(ctx, []byte("body"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	second, err := store.PutBlob(ctx, []byte("body"))
	if err != nil {
		t.Fatalf("PutBlob again: %v", err)
	}
	if first != second {
		t.Errorf("references %q and %q differ for identical bytes", first, second)
	}
	got, err := store.GetBlob(ctx, first)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if string(got) != "body" {
		t.Errorf("blob = %q, want body", got)
	}
	if _, err := store.GetBlob(ctx, "absent"); err == nil {
		t.Error("GetBlob(absent) = nil, want an error")
	}
}

// MemStore is the reference in-memory Store. It is not a production backend: it
// exists so the suite has something to run against that is not the backend it is
// verifying, and so tests elsewhere stop growing their own.
type MemStore struct {
	mu      sync.Mutex
	tasks   map[string]*memTask
	order   []string
	blobs   map[string][]byte
	saves   int
	nextSeq map[string]int64
}

type memTask struct {
	seed      agent.Checkpoint
	state     agent.TaskState
	round     int
	contextID string
	artifacts []agent.Artifact
	pending   *message.ToolCall
	sections  map[string]json.RawMessage
	version   int64
	serverVer int64
	updatedAt time.Time
	events    []agent.EventRecord
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{
		tasks:   make(map[string]*memTask),
		blobs:   make(map[string][]byte),
		nextSeq: make(map[string]int64),
	}
}

var _ agent.Store = (*MemStore)(nil)

func (m *MemStore) Save(ctx context.Context, cp agent.Checkpoint, events []agent.EventRecord) (int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if cp.Task.ID == "" {
		return 0, 0, errors.New("agenttest: task id is required")
	}
	cp.Task.History = nil
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	entry, ok := m.tasks[cp.Task.ID]
	if !ok {
		entry = &memTask{seed: cp, version: 1}
		m.tasks[cp.Task.ID] = entry
		m.order = append(m.order, cp.Task.ID)
	} else {
		entry.version++
	}
	entry.state = cp.Task.State
	entry.round = cp.Task.Round
	entry.contextID = cp.Task.SessionID
	entry.artifacts = cp.Task.Artifacts
	entry.pending = cp.Task.Pending
	if len(cp.Sections) > 0 {
		if entry.sections == nil {
			entry.sections = make(map[string]json.RawMessage, len(cp.Sections))
		}
		for key, value := range cp.Sections {
			entry.sections[key] = value
		}
	}
	entry.serverVer = cp.ServerVersion
	entry.updatedAt = time.Now()
	var first, last int64
	for i, event := range events {
		// Sequences start at 1, like the SQL backend: EventsAfter is exclusive,
		// so a sequence of 0 would be invisible to Events.
		seq, ok := m.nextSeq[cp.Task.ID]
		if !ok {
			seq = 1
		}
		m.nextSeq[cp.Task.ID] = seq + 1
		event.Seq = seq
		if i == 0 {
			first = seq
		}
		last = seq
		entry.events = append(entry.events, event)
	}
	return first, last, nil
}

func (m *MemStore) Load(ctx context.Context, taskID string) (agent.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return agent.Checkpoint{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.tasks[taskID]
	if !ok {
		return agent.Checkpoint{}, agent.ErrNoCheckpoint
	}
	cp := entry.seed
	cp.Task.ID = taskID
	cp.Task.SessionID = entry.contextID
	cp.Task.State = entry.state
	cp.Task.Round = entry.round
	cp.Task.Artifacts = entry.artifacts
	cp.Task.Pending = entry.pending
	if len(entry.sections) > 0 {
		cp.Sections = make(map[string]json.RawMessage, len(entry.sections))
		for key, value := range entry.sections {
			cp.Sections[key] = value
		}
	}
	cp.Version = entry.version
	cp.ServerVersion = entry.serverVer
	cp.Task.History = nil
	return cp, nil
}

func (m *MemStore) LatestForSession(ctx context.Context, sessionID string) (agent.Checkpoint, error) {
	latest, ok := m.latestID(sessionID)
	if !ok {
		return agent.Checkpoint{}, agent.ErrNoCheckpoint
	}
	return m.Load(ctx, latest)
}

func (m *MemStore) latestID(sessionID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var (
		id     string
		found  bool
		newest time.Time
	)
	for _, taskID := range m.order {
		entry := m.tasks[taskID]
		if entry.contextID != sessionID {
			continue
		}
		if !found || entry.updatedAt.After(newest) {
			id, found, newest = taskID, true, entry.updatedAt
		}
	}
	return id, found
}

func (m *MemStore) Events(ctx context.Context, taskID string) ([]agent.EventRecord, error) {
	return m.EventsAfter(ctx, taskID, 0)
}

func (m *MemStore) EventsAfter(ctx context.Context, taskID string, after int64) ([]agent.EventRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.tasks[taskID]
	if !ok {
		return nil, nil
	}
	out := make([]agent.EventRecord, 0, len(entry.events))
	for _, event := range entry.events {
		if event.Seq > after {
			out = append(out, event)
		}
	}
	return out, nil
}

func (m *MemStore) SetServerVersion(ctx context.Context, taskID string, version int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.tasks[taskID]
	if !ok {
		return agent.ErrNoCheckpoint
	}
	entry.serverVer = version
	entry.updatedAt = time.Now()
	return nil
}

func (m *MemStore) TruncateEvents(ctx context.Context, taskID string, seq int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if seq < 0 {
		return errors.New("agenttest: truncation point must not be negative")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.tasks[taskID]
	if !ok || len(entry.events) == 0 {
		return nil
	}
	newest := entry.events[len(entry.events)-1].Seq
	if seq >= newest {
		return errors.New("agenttest: truncating would remove the newest event")
	}
	kept := entry.events[:0]
	for _, event := range entry.events {
		if event.Seq > seq {
			kept = append(kept, event)
		}
	}
	entry.events = kept
	return nil
}

func (m *MemStore) List(ctx context.Context) ([]agent.TaskSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]agent.TaskSummary, 0, len(m.tasks))
	for _, taskID := range m.order {
		entry := m.tasks[taskID]
		list = append(list, agent.TaskSummary{
			ID:        taskID,
			SessionID: entry.contextID,
			State:     entry.state,
			Round:     entry.round,
			UpdatedAt: entry.updatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	return list, nil
}

func (m *MemStore) Delete(ctx context.Context, taskID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tasks, taskID)
	delete(m.nextSeq, taskID)
	for i, id := range m.order {
		if id == taskID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return nil
}

func (m *MemStore) PutBlob(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	ref := hex.EncodeToString(sum[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.blobs[ref]; !ok {
		m.blobs[ref] = append([]byte(nil), data...)
	}
	return ref, nil
}

func (m *MemStore) GetBlob(ctx context.Context, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[ref]
	if !ok {
		return nil, errors.New("agenttest: no such blob")
	}
	return append([]byte(nil), data...), nil
}
