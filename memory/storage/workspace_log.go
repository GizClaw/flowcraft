package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const logSchemaVersion = 1

type persistedHead struct {
	SchemaVersion int    `json:"schema_version"`
	LastSeq       uint64 `json:"last_seq"`
}

type persistedPending struct {
	SchemaVersion  int       `json:"schema_version"`
	IdempotencyKey string    `json:"idempotency_key"`
	FirstSeq       uint64    `json:"first_seq"`
	LastSeq        uint64    `json:"last_seq"`
	Digest         string    `json:"digest"`
	CreatedAt      time.Time `json:"created_at"`
}

type persistedCommit struct {
	SchemaVersion  int       `json:"schema_version"`
	IdempotencyKey string    `json:"idempotency_key"`
	FirstSeq       uint64    `json:"first_seq"`
	LastSeq        uint64    `json:"last_seq"`
	Digest         string    `json:"digest"`
	CreatedAt      time.Time `json:"created_at"`
}

// WorkspaceLog implements Log on one workspace. Atomicity is single-process:
// one adapter instance serializes writers with a mutex, and a pending marker
// plus head pointer give crash recovery (publish-forward or rollback). It is
// not a database transaction; real cross-process atomicity is the obligation
// of transactional substrates (SQLite/PG).
type WorkspaceLog struct {
	ws workspace.Workspace
	mu sync.Mutex
}

var _ Log = (*WorkspaceLog)(nil)
var _ CommitLog = (*WorkspaceLog)(nil)

// NewWorkspaceLog constructs a workspace-backed Log.
func NewWorkspaceLog(ws workspace.Workspace) (*WorkspaceLog, error) {
	if nilWorkspace(ws) {
		return nil, errors.New("storage: workspace is required")
	}
	return &WorkspaceLog{ws: ws}, nil
}

// Append implements Log.
func (store *WorkspaceLog) Append(ctx context.Context, stream string, events []Event, opts AppendOptions) (Commit, error) {
	if err := validateAppend(stream, events, opts); err != nil {
		return Commit{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	dir, err := store.streamDir(stream)
	if err != nil {
		return Commit{}, err
	}
	if err := store.recoverLocked(ctx, stream, dir); err != nil {
		return Commit{}, err
	}
	digest, err := batchDigest(stream, opts.IdempotencyKey, events)
	if err != nil {
		return Commit{}, err
	}
	if commit, ok, err := store.readCommitLocked(ctx, dir, opts.IdempotencyKey); err != nil {
		return Commit{}, err
	} else if ok {
		if commit.Digest != digest {
			return Commit{}, fmt.Errorf("%w: idempotency key %q replayed with different content",
				ErrConflict, opts.IdempotencyKey)
		}
		return commitFromPersisted(stream, commit), nil
	}

	head, ok, err := store.readHeadLocked(ctx, dir)
	if err != nil {
		return Commit{}, err
	}
	firstSeq := uint64(1)
	if ok {
		firstSeq = head.LastSeq + 1
	}
	lastSeq := firstSeq + uint64(len(events)) - 1
	pending := persistedPending{
		SchemaVersion:  logSchemaVersion,
		IdempotencyKey: opts.IdempotencyKey,
		FirstSeq:       firstSeq,
		LastSeq:        lastSeq,
		Digest:         digest,
		CreatedAt:      time.Now().UTC(),
	}
	if err := writeJSON(ctx, store.ws, store.pendingPath(dir), pending); err != nil {
		return Commit{}, fmt.Errorf("storage log: reserve append: %w", err)
	}
	for index, event := range events {
		event.Seq = firstSeq + uint64(index)
		if err := writeImmutableJSON(ctx, store.ws, store.eventPath(dir, event.Seq), event); err != nil {
			return Commit{}, fmt.Errorf("storage log: write event %d: %w", event.Seq, err)
		}
	}
	commit := persistedCommit{
		SchemaVersion:  logSchemaVersion,
		IdempotencyKey: opts.IdempotencyKey,
		FirstSeq:       firstSeq,
		LastSeq:        lastSeq,
		Digest:         digest,
		CreatedAt:      pending.CreatedAt,
	}
	if err := writeImmutableJSON(ctx, store.ws, store.commitPath(dir, opts.IdempotencyKey), commit); err != nil {
		return Commit{}, fmt.Errorf("storage log: write commit marker: %w", err)
	}
	if err := writeJSON(ctx, store.ws, store.headPath(dir), persistedHead{
		SchemaVersion: logSchemaVersion, LastSeq: lastSeq,
	}); err != nil {
		return Commit{}, fmt.Errorf("storage log: publish head: %w", err)
	}
	if err := deleteIfExists(ctx, store.ws, store.pendingPath(dir)); err != nil {
		return Commit{}, fmt.Errorf("storage log: clear pending: %w", err)
	}
	return commitFromPersisted(stream, commit), nil
}

// Read implements Log.
func (store *WorkspaceLog) Read(ctx context.Context, stream string, after uint64, limit int) ([]Event, error) {
	if err := validateStream(stream); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	dir, err := store.streamDir(stream)
	if err != nil {
		return nil, err
	}
	if err := store.recoverLocked(ctx, stream, dir); err != nil {
		return nil, err
	}
	head, ok, err := store.readHeadLocked(ctx, dir)
	if err != nil {
		return nil, err
	}
	if !ok || after >= head.LastSeq {
		return []Event{}, nil
	}
	events := make([]Event, 0)
	for seq := after + 1; seq <= head.LastSeq; seq++ {
		event, found, err := store.readEventLocked(ctx, dir, seq)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("storage log: indexed event %d is missing", seq)
		}
		events = append(events, event)
		if limit > 0 && len(events) == limit {
			break
		}
	}
	return events, nil
}

// ReadAt implements Log.
func (store *WorkspaceLog) ReadAt(ctx context.Context, stream string, seq uint64) (Event, error) {
	if err := validateStream(stream); err != nil {
		return Event{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	dir, err := store.streamDir(stream)
	if err != nil {
		return Event{}, err
	}
	if err := store.recoverLocked(ctx, stream, dir); err != nil {
		return Event{}, err
	}
	event, found, err := store.readEventLocked(ctx, dir, seq)
	if err != nil {
		return Event{}, err
	}
	if !found {
		return Event{}, ErrNotFound
	}
	return event, nil
}

// ReadLatest implements Log.
func (store *WorkspaceLog) ReadLatest(ctx context.Context, stream string, n int) ([]Event, error) {
	if err := validateStream(stream); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	dir, err := store.streamDir(stream)
	if err != nil {
		return nil, err
	}
	if err := store.recoverLocked(ctx, stream, dir); err != nil {
		return nil, err
	}
	head, ok, err := store.readHeadLocked(ctx, dir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Event{}, nil
	}
	start := uint64(1)
	if n > 0 && uint64(n) < head.LastSeq {
		start = head.LastSeq - uint64(n) + 1
	}
	events := make([]Event, 0, head.LastSeq-start+1)
	for seq := start; seq <= head.LastSeq; seq++ {
		event, found, err := store.readEventLocked(ctx, dir, seq)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("storage log: indexed event %d is missing", seq)
		}
		events = append(events, event)
	}
	return events, nil
}

// ListStreams implements Log.
func (store *WorkspaceLog) ListStreams(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" {
		if err := validateName(prefix); err != nil {
			return nil, err
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	base := prefixBase(prefix)
	basePath := logRoot
	if base != "" {
		var err error
		basePath, err = nameToPath(logRoot, base)
		if err != nil {
			return nil, err
		}
	}
	streams := make([]string, 0)
	err := workspace.Walk(ctx, store.ws, basePath, func(child string, entry fs.DirEntry) error {
		if !entry.IsDir() {
			return nil
		}
		if !strings.HasPrefix(entry.Name(), "k_") {
			// Adapter-internal directories (events/, commits/) are never
			// streams; encoded stream segments always start with "k_".
			return filepath.SkipDir
		}
		name, err := pathToName(logRoot, child)
		if err != nil {
			return err
		}
		if !nameHasPrefix(name, prefix) {
			// Neither this directory nor any descendant can match.
			return filepath.SkipDir
		}
		headExists, err := store.ws.Exists(ctx, store.headPath(child))
		if err != nil {
			return err
		}
		if !headExists {
			// A committed batch may be missing its head pointer after a
			// crash between the commit marker and head publish. Resolve any
			// interrupted append (publish-forward or rollback) before
			// deciding whether this directory is a stream.
			for _, marker := range []string{"pending.json", "events", "commits"} {
				exists, err := store.ws.Exists(ctx, path.Join(child, marker))
				if err != nil {
					return err
				}
				if exists {
					if err := store.recoverLocked(ctx, name, child); err != nil {
						return err
					}
					headExists, err = store.ws.Exists(ctx, store.headPath(child))
					if err != nil {
						return err
					}
					break
				}
			}
		}
		if headExists {
			streams = append(streams, name)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(streams)
	return streams, nil
}

// ListCommits implements CommitLog.
func (store *WorkspaceLog) ListCommits(ctx context.Context, stream string, after uint64, limit int) ([]Commit, error) {
	if err := validateStream(stream); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	dir, err := store.streamDir(stream)
	if err != nil {
		return nil, err
	}
	if err := store.recoverLocked(ctx, stream, dir); err != nil {
		return nil, err
	}
	entries, err := store.ws.List(ctx, path.Join(dir, "commits"))
	if err != nil {
		if isNotFound(err) {
			return []Commit{}, nil
		}
		return nil, fmt.Errorf("storage log: list commits for %q: %w", stream, err)
	}
	commits := make([]persistedCommit, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		key, ok := decodeCommitFilename(entry.Name())
		if !ok {
			return nil, fmt.Errorf("storage log: invalid commit marker %q", entry.Name())
		}
		commit, found, err := store.readCommitLocked(ctx, dir, key)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("storage log: commit marker %q disappeared during scan", key)
		}
		commits = append(commits, commit)
	}
	sort.Slice(commits, func(i, j int) bool { return commits[i].FirstSeq < commits[j].FirstSeq })
	result := make([]Commit, 0, len(commits))
	for _, commit := range commits {
		if commit.LastSeq <= after {
			continue
		}
		result = append(result, commitFromPersisted(stream, commit))
		if limit > 0 && len(result) == limit {
			break
		}
	}
	return result, nil
}

// ReadCommitByKey implements CommitLog.
func (store *WorkspaceLog) ReadCommitByKey(ctx context.Context, stream, idempotencyKey string) (Commit, bool, error) {
	if err := validateStream(stream); err != nil {
		return Commit{}, false, err
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return Commit{}, false, errors.New("storage log: idempotency key is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	dir, err := store.streamDir(stream)
	if err != nil {
		return Commit{}, false, err
	}
	if err := store.recoverLocked(ctx, stream, dir); err != nil {
		return Commit{}, false, err
	}
	commit, found, err := store.readCommitLocked(ctx, dir, idempotencyKey)
	if err != nil {
		return Commit{}, false, err
	}
	if !found {
		return Commit{}, false, nil
	}
	return commitFromPersisted(stream, commit), true, nil
}

func decodeCommitFilename(name string) (string, bool) {
	if !strings.HasSuffix(name, ".json") {
		return "", false
	}
	key, err := decodeSegment(strings.TrimSuffix(name, ".json"))
	if err != nil {
		return "", false
	}
	return key, true
}

// recoverLocked resolves an interrupted append: if the commit marker exists
// the batch was published (head may lag and is advanced); otherwise the
// reserved events are rolled back and the pending marker cleared.
func (store *WorkspaceLog) recoverLocked(ctx context.Context, stream, dir string) error {
	var pending persistedPending
	ok, err := readJSON(ctx, store.ws, store.pendingPath(dir), &pending)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := validatePending(pending, stream); err != nil {
		return err
	}
	commit, committed, err := store.readCommitLocked(ctx, dir, pending.IdempotencyKey)
	if err != nil {
		return err
	}
	if !committed {
		for seq := pending.FirstSeq; seq <= pending.LastSeq; seq++ {
			if err := deleteIfExists(ctx, store.ws, store.eventPath(dir, seq)); err != nil {
				return err
			}
		}
		return deleteIfExists(ctx, store.ws, store.pendingPath(dir))
	}
	if commit.Digest != pending.Digest {
		return fmt.Errorf("%w: pending digest does not match committed batch", ErrConflict)
	}
	head, hasHead, err := store.readHeadLocked(ctx, dir)
	if err != nil {
		return err
	}
	if !hasHead || head.LastSeq < pending.LastSeq {
		if err := writeJSON(ctx, store.ws, store.headPath(dir), persistedHead{
			SchemaVersion: logSchemaVersion, LastSeq: pending.LastSeq,
		}); err != nil {
			return err
		}
	}
	return deleteIfExists(ctx, store.ws, store.pendingPath(dir))
}

func validatePending(pending persistedPending, stream string) error {
	if pending.SchemaVersion != logSchemaVersion {
		return errors.New("storage log: unsupported pending schema")
	}
	if pending.IdempotencyKey == "" || pending.FirstSeq == 0 ||
		pending.LastSeq < pending.FirstSeq || pending.Digest == "" {
		return errors.New("storage log: corrupt pending marker")
	}
	return nil
}

func validateAppend(stream string, events []Event, opts AppendOptions) error {
	if err := validateStream(stream); err != nil {
		return err
	}
	if strings.TrimSpace(opts.IdempotencyKey) == "" {
		return errors.New("storage log: idempotency key is required")
	}
	if len(events) == 0 {
		return errors.New("storage log: events are required")
	}
	for index, event := range events {
		if event.Stream != stream {
			return fmt.Errorf("storage log: event %d stream %q does not match %q", index, event.Stream, stream)
		}
		if strings.TrimSpace(event.Type) == "" {
			return fmt.Errorf("storage log: event %d type is required", index)
		}
	}
	return nil
}

func validateStream(stream string) error {
	if err := validateName(stream); err != nil {
		return err
	}
	return nil
}

func (store *WorkspaceLog) streamDir(stream string) (string, error) {
	return nameToPath(logRoot, stream)
}

func (store *WorkspaceLog) headPath(dir string) string {
	return path.Join(dir, "head.json")
}

func (store *WorkspaceLog) pendingPath(dir string) string {
	return path.Join(dir, "pending.json")
}

func (store *WorkspaceLog) commitPath(dir, key string) string {
	return path.Join(dir, "commits", encodeSegment(key)+".json")
}

func (store *WorkspaceLog) eventPath(dir string, seq uint64) string {
	return path.Join(dir, "events", fmt.Sprintf("%020d.json", seq))
}

func (store *WorkspaceLog) readHeadLocked(ctx context.Context, dir string) (persistedHead, bool, error) {
	var head persistedHead
	ok, err := readJSON(ctx, store.ws, store.headPath(dir), &head)
	if err != nil || !ok {
		return persistedHead{}, ok, err
	}
	if head.SchemaVersion != logSchemaVersion || head.LastSeq == 0 {
		return persistedHead{}, false, errors.New("storage log: corrupt head")
	}
	return head, true, nil
}

func (store *WorkspaceLog) readCommitLocked(ctx context.Context, dir, key string) (persistedCommit, bool, error) {
	var commit persistedCommit
	ok, err := readJSON(ctx, store.ws, store.commitPath(dir, key), &commit)
	if err != nil || !ok {
		return persistedCommit{}, ok, err
	}
	if commit.SchemaVersion != logSchemaVersion || commit.IdempotencyKey != key ||
		commit.FirstSeq == 0 || commit.LastSeq < commit.FirstSeq || commit.Digest == "" {
		return persistedCommit{}, false, errors.New("storage log: corrupt commit marker")
	}
	return commit, true, nil
}

func (store *WorkspaceLog) readEventLocked(ctx context.Context, dir string, seq uint64) (Event, bool, error) {
	var event Event
	ok, err := readJSON(ctx, store.ws, store.eventPath(dir, seq), &event)
	if err != nil || !ok {
		return Event{}, ok, err
	}
	if event.Seq != seq {
		return Event{}, false, fmt.Errorf("storage log: corrupt event %d", seq)
	}
	return event, true, nil
}

func writeJSON(ctx context.Context, ws workspace.Workspace, name string, value any) error {
	data, err := marshalJSON(value)
	if err != nil {
		return err
	}
	return workspace.AtomicWrite(ctx, ws, name, data)
}

func commitFromPersisted(stream string, commit persistedCommit) Commit {
	return Commit{
		ID:             commitID(stream, commit.IdempotencyKey),
		Stream:         stream,
		FirstSeq:       commit.FirstSeq,
		LastSeq:        commit.LastSeq,
		IdempotencyKey: commit.IdempotencyKey,
		CreatedAt:      commit.CreatedAt,
	}
}

func commitID(stream, key string) string {
	sum := sha256.Sum256([]byte(stream + "\x00" + key))
	return "commit-" + hex.EncodeToString(sum[:])
}

type digestEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func batchDigest(stream, key string, events []Event) (string, error) {
	wire := make([]digestEvent, len(events))
	for index, event := range events {
		wire[index] = digestEvent{Type: event.Type, Payload: event.Payload}
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(stream+"\x00"+key+"\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}
