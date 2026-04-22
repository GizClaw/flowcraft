package projection

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// DeadLetter is one envelope that a projector failed to apply past its
// retry threshold. The runner persists the failure so operators can inspect,
// fix, and replay later.
type DeadLetter struct {
	ProjectorName string
	Seq           int64
	Type          string
	Partition     string
	Payload       json.RawMessage
	Err           string
	At            time.Time
}

// DeadLetterSink is the interface every dead-letter destination satisfies.
type DeadLetterSink interface {
	Write(ctx context.Context, dl DeadLetter) error
}

// LogDLT prints to slog.Warn; used as a safe default in tests.
type LogDLT struct{}

func (LogDLT) Write(_ context.Context, dl DeadLetter) error {
	slog.Warn("projector dead-letter",
		"projector", dl.ProjectorName,
		"seq", dl.Seq,
		"type", dl.Type,
		"partition", dl.Partition,
		"err", dl.Err)
	return nil
}

// SQLiteDLT writes to the dead_letters table (migration 010).
type SQLiteDLT struct{ DB *sql.DB }

// NewSQLiteDLT returns a SQLite-backed dead-letter sink.
func NewSQLiteDLT(db *sql.DB) *SQLiteDLT { return &SQLiteDLT{DB: db} }

var _ DeadLetterSink = (*SQLiteDLT)(nil)

func (s *SQLiteDLT) Write(ctx context.Context, dl DeadLetter) error {
	envelope, err := json.Marshal(struct {
		Seq       int64           `json:"seq"`
		Partition string          `json:"partition"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}{Seq: dl.Seq, Partition: dl.Partition, Type: dl.Type, Payload: dl.Payload})
	if err != nil {
		return fmt.Errorf("dlt: marshal: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO dead_letters(projector_name,event_seq,event_type,error_class,error_message,envelope,created_at)
		VALUES(?,?,?,?,?,?,?)`,
		dl.ProjectorName, dl.Seq, dl.Type, "ApplyError", dl.Err, envelope,
		dl.At.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("dlt: insert: %w", err)
	}
	return nil
}
