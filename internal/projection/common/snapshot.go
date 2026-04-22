package projection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func nowRFC3339Nano() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// SnapshotEntry is the on-disk shape of projector_snapshots (migration 008).
type SnapshotEntry struct {
	ProjectorName string
	FormatVersion int
	Cursor        int64
	Payload       []byte
	CreatedAt     string
}

// SnapshotStore is the persistence API used by the runner during restore and
// by snapshot writers during steady state. Implementations must be safe for
// concurrent use.
type SnapshotStore interface {
	// Latest returns the highest-cursor snapshot for the projector, or nil.
	Latest(ctx context.Context, projectorName string) (*SnapshotEntry, error)
	// Save persists a snapshot row; older rows for the same projector should
	// be left in place for recovery (eviction is the operator's job).
	Save(ctx context.Context, e SnapshotEntry) error
}

// SQLiteSnapshots is the SQLite-backed implementation backed by table
// projector_snapshots. It is wired by bootstrap when the runner uses
// RestoreSnapshot.
type SQLiteSnapshots struct{ DB *sql.DB }

// NewSQLiteSnapshots returns a SnapshotStore backed by the given DB.
func NewSQLiteSnapshots(db *sql.DB) *SQLiteSnapshots { return &SQLiteSnapshots{DB: db} }

var _ SnapshotStore = (*SQLiteSnapshots)(nil)

func (s *SQLiteSnapshots) Latest(ctx context.Context, projectorName string) (*SnapshotEntry, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT projector_name,payload_fmt,cursor_seq,payload,created_at
		FROM projector_snapshots
		WHERE projector_name=?
		ORDER BY cursor_seq DESC
		LIMIT 1`, projectorName)
	var e SnapshotEntry
	if err := row.Scan(&e.ProjectorName, &e.FormatVersion, &e.Cursor, &e.Payload, &e.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapshots: latest %s: %w", projectorName, err)
	}
	return &e, nil
}

func (s *SQLiteSnapshots) Save(ctx context.Context, e SnapshotEntry) error {
	if e.CreatedAt == "" {
		e.CreatedAt = nowRFC3339Nano()
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO projector_snapshots(projector_name,payload_fmt,cursor_seq,payload,created_at)
		VALUES(?,?,?,?,?)`, e.ProjectorName, e.FormatVersion, e.Cursor, e.Payload, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("snapshots: save %s: %w", e.ProjectorName, err)
	}
	return nil
}
