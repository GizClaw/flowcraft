package projection

import (
	"context"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// RestoreMode selects how a projector restores its state on startup.
type RestoreMode int

const (
	// RestoreReplay replays all events from seq=0 (or last checkpoint).
	RestoreReplay RestoreMode = iota
	// RestoreSnapshot loads the latest snapshot then replays events after it.
	RestoreSnapshot
	// RestoreWindow loads only events within the sliding time window.
	RestoreWindow
)

func (m RestoreMode) String() string {
	switch m {
	case RestoreReplay:
		return "replay"
	case RestoreSnapshot:
		return "snapshot"
	case RestoreWindow:
		return "window"
	}
	return "unknown"
}

// ErrSnapshotIncompatible is returned when a snapshot's format version doesn't match.
var ErrSnapshotIncompatible = errors.New("snapshot format version mismatch")

// Projector is implemented by every read-model maintained from the event log.
//
// Apply MUST run inside the uow's transaction: any business writes the
// projector performs share the same atomic boundary as the checkpoint update,
// so the projector cannot get out of sync with its checkpoint.
type Projector interface {
	Name() string
	Subscribes() []string
	RestoreMode() RestoreMode
	Apply(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error
	OnReady(ctx context.Context) error
}

// PartitionFilter is an optional refinement: when implemented, the runner
// passes Partitions to Subscribe so the SQLite layer skips events that don't
// match. Useful for partition-scoped projectors (e.g. card view, webhook outbound).
type PartitionFilter interface {
	Partitions() []string
}

// Snapshotter is implemented by projectors that use RestoreSnapshot mode.
type Snapshotter interface {
	// Snapshot returns the cursor and payload to persist; called periodically.
	Snapshot(ctx context.Context) (cursor int64, payload []byte, err error)
	// LoadSnapshot restores projector state from a previously saved snapshot.
	LoadSnapshot(ctx context.Context, cursor int64, payload []byte) error
	// SnapshotEvery returns the (event count, period) snapshot triggers.
	SnapshotEvery() (eventsThreshold int64, period time.Duration)
	// SnapshotFormatVersion is the format ID; LoadSnapshot must reject any
	// payload whose stored version differs.
	SnapshotFormatVersion() int
}

// Windowed is implemented by projectors that use RestoreWindow mode.
type Windowed interface {
	// WindowSize returns the size of the sliding window.
	WindowSize() time.Duration
}

// DefaultSnapshotEveryEvents is the default snapshot interval (event count).
const DefaultSnapshotEveryEvents int64 = 1000

// DefaultSnapshotEveryPeriod is the default snapshot interval (time-based).
const DefaultSnapshotEveryPeriod = 10 * time.Minute
