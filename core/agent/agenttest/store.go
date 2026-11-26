package agenttest

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/goccy/go-json"
)

// MemStore is the in-memory reference store: the contract's smallest implementation,
// enough for a test and honest about what a store must do.
//
// It is not optimised and does not pretend to be: sequence numbers are per task, the
// blob reference is the content hash, and a projection load never replays the log. Those
// three properties are the contract's, and a second backend is expected to keep them.
type MemStore struct {
	mu    sync.Mutex
	tasks map[string]*task
	blobs map[string][]byte
	// saved is a store-wide ordinal: "the newest task of a session" is a question about
	// save order, and a per-task version cannot answer it — two tasks both sit at version
	// 1 after their first save.
	saved int64
}

type task struct {
	cp       agent.Checkpoint
	events   []agent.EventRecord
	seq      int64
	savedSeq int64
}

// MemStore satisfies the narrow contract a driver is handed as well as the full one, which is
// what lets a test pass it wherever a [agent.DriverStore] is expected.
var _ agent.DriverStore = (*MemStore)(nil)

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{tasks: map[string]*task{}, blobs: map[string][]byte{}}
}

// Save implements agent.Store.
func (m *MemStore) Save(ctx context.Context, cp agent.Checkpoint, events []agent.EventRecord, options ...agent.SaveOption) (int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.tasks[cp.Task.ID]
	if !ok {
		entry = &task{}
		m.tasks[cp.Task.ID] = entry
	}
	m.saved++
	// Sections are owner-keyed: a key this save omits keeps the value the previous one
	// wrote, which is what makes a once-written section (the manifest) survive.
	if len(entry.cp.Sections) > 0 {
		merged := make(map[string]json.RawMessage, len(entry.cp.Sections)+len(cp.Sections))
		for key, value := range entry.cp.Sections {
			merged[key] = value
		}
		for key, value := range cp.Sections {
			merged[key] = value
		}
		cp.Sections = merged
	}
	cp.Schema = agent.Schema
	cp.Version = entry.cp.Version + 1
	cp.ServerVersion = entry.cp.ServerVersion
	entry.cp = cp
	entry.savedSeq = m.saved

	var first, last int64
	for _, event := range events {
		entry.seq++
		event.Seq = entry.seq
		if first == 0 {
			first = event.Seq
		}
		last = event.Seq
		entry.events = append(entry.events, event)
	}

	for _, option := range options {
		settings := agent.SaveOptions{}
		option(&settings)
		if settings.Truncate {
			m.truncateLocked(entry, settings.Through, settings.Kinds)
		}
	}
	return first, last, nil
}

// Load implements agent.Store.
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
	return entry.cp, nil
}

// LatestForSession implements agent.Store.
func (m *MemStore) LatestForSession(ctx context.Context, sessionID string) (agent.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return agent.Checkpoint{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var newest agent.Checkpoint
	var newestSaved int64
	for _, entry := range m.tasks {
		if entry.cp.Task.SessionID != sessionID {
			continue
		}
		if newest.Task.ID == "" || entry.savedSeq > newestSaved {
			newest, newestSaved = entry.cp, entry.savedSeq
		}
	}
	if newest.Task.ID == "" {
		return agent.Checkpoint{}, agent.ErrNoCheckpoint
	}
	return newest, nil
}

// Events implements agent.Store.
func (m *MemStore) Events(ctx context.Context, taskID string) ([]agent.EventRecord, error) {
	return m.EventsAfter(ctx, taskID, 0)
}

// EventsAfter implements agent.Store.
func (m *MemStore) EventsAfter(ctx context.Context, taskID string, after int64) ([]agent.EventRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.tasks[taskID]
	if !ok {
		return nil, agent.ErrNoCheckpoint
	}
	var out []agent.EventRecord
	for _, event := range entry.events {
		if event.Seq > after {
			out = append(out, event)
		}
	}
	return out, nil
}

// SetServerVersion implements agent.Store.
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
	entry.cp.ServerVersion = version
	return nil
}

// TruncateEvents implements agent.Store.
func (m *MemStore) TruncateEvents(ctx context.Context, taskID string, seq int64, kinds ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.tasks[taskID]
	if !ok {
		return agent.ErrNoCheckpoint
	}
	m.truncateLocked(entry, seq, kinds)
	return nil
}

// List implements agent.Store.
func (m *MemStore) List(ctx context.Context) ([]agent.TaskSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]agent.TaskSummary, 0, len(m.tasks))
	for _, entry := range m.tasks {
		out = append(out, agent.TaskSummary{
			ID:        entry.cp.Task.ID,
			SessionID: entry.cp.Task.SessionID,
			State:     entry.cp.Task.State,
		})
	}
	return out, nil
}

// Delete implements agent.Store.
func (m *MemStore) Delete(ctx context.Context, taskID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tasks, taskID)
	return nil
}

// PutBlob implements agent.Store.
func (m *MemStore) PutBlob(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ref := blobRef(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.blobs[ref]; !ok {
		m.blobs[ref] = append([]byte(nil), data...)
	}
	return ref, nil
}

// GetBlob implements agent.Store.
func (m *MemStore) GetBlob(ctx context.Context, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[ref]
	if !ok {
		return nil, agent.ErrNoCheckpoint
	}
	return append([]byte(nil), data...), nil
}

// truncateLocked drops events at or before seq, never the newest. The caller holds mu.
//
// The newest event is the one exception because sequence numbers must stay monotonic: a
// store that dropped the tail would hand the next save a number it has already used. It
// is per event, not per call, so a kinds filter can still compact an older fact while a
// newer fact of another kind survives.
func (m *MemStore) truncateLocked(entry *task, seq int64, kinds []string) {
	if len(entry.events) == 0 {
		return
	}
	newest := entry.events[len(entry.events)-1].Seq
	keepKinds := map[string]struct{}{}
	for _, kind := range kinds {
		keepKinds[kind] = struct{}{}
	}
	kept := entry.events[:0]
	for _, event := range entry.events {
		drop := event.Seq <= seq && event.Seq != newest
		if drop && len(keepKinds) > 0 {
			_, drop = keepKinds[event.Kind]
		}
		if drop {
			continue
		}
		kept = append(kept, event)
	}
	entry.events = kept
}

var _ agent.Store = (*MemStore)(nil)
