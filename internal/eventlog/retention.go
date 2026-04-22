package eventlog

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// sentinelMaxSeq is the seq cutoff used when no projector has been
// registered yet. It is large enough that "seq < sentinelMaxSeq" matches
// every row in event_log, so retention can still operate at startup
// before R4 wires concrete projectors.
const sentinelMaxSeq int64 = math.MaxInt64

// RetentionConfig holds TTL and batch settings for the retention goroutine.
// Category values are durations; 0 means never delete.
type RetentionConfig struct {
	Categories map[string]time.Duration
	BatchSize  int
	Interval   time.Duration
}

// DefaultRetentionConfig is the default retention config used when none is provided.
var DefaultRetentionConfig = RetentionConfig{
	Categories: map[string]time.Duration{
		"volatile":    7 * 24 * time.Hour,
		"operational": 30 * 24 * time.Hour,
		"business":    90 * 24 * time.Hour,
		"audit":       365 * 24 * time.Hour,
		"permanent":   0,
	},
	BatchSize: 1000,
	Interval:  1 * time.Hour,
}

// StartRetentionGoroutine launches a background goroutine that periodically
// cleans up expired events. It is a no-op for TTL in R2; R3 enables actual deletion.
func StartRetentionGoroutine(l *SQLiteLog, cfg RetentionConfig, checkpoints CheckpointStore) func() {
	stop := make(chan struct{})
	ticker := time.NewTicker(cfg.Interval)
	go func() {
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := runRetention(ctx, l, cfg, checkpoints); err != nil {
					slog.Error("retention run failed", "err", err)
				}
				cancel()
			case <-stop:
				ticker.Stop()
				return
			}
		}
	}()
	return func() { close(stop) }
}

// RunRetention deletes expired events category by category.
// It protects unconsumed events by respecting the minimum checkpoint.
//
// Exported so tests can drive the deletion path without waiting for the
// 1h ticker. Production callers go through StartRetentionGoroutine.
func RunRetention(ctx context.Context, l *SQLiteLog, cfg RetentionConfig, checkpoints CheckpointStore) error {
	return runRetention(ctx, l, cfg, checkpoints)
}

// runRetention deletes expired events category by category.
//
// Two safety checks:
//
//   - ts<cutoff: only events older than the category's TTL are deleted
//   - seq<minCheckpoint: never delete rows that the slowest projector has
//     not consumed yet (sentinelMaxSeq when no projectors are registered)
//
// Rows are deleted in batches via WHERE rowid IN (SELECT rowid ... LIMIT ?)
// because modernc.org/sqlite is built without SQLITE_ENABLE_UPDATE_DELETE_LIMIT
// so a bare DELETE ... LIMIT is rejected at parse time.
func runRetention(ctx context.Context, l *SQLiteLog, cfg RetentionConfig, checkpoints CheckpointStore) error {
	minCP, ok, err := checkpoints.Min(ctx)
	if err != nil {
		return err
	}
	if !ok {
		minCP = sentinelMaxSeq
	}

	const query = `DELETE FROM event_log
WHERE rowid IN (
  SELECT rowid FROM event_log
  WHERE category=? AND ts<? AND seq<?
  LIMIT ?
)`

	for category, ttl := range cfg.Categories {
		if ttl == 0 {
			continue // permanent — admin tool only
		}
		cutoff := time.Now().Add(-ttl).Format(time.RFC3339Nano)
		batch := cfg.BatchSize
		if batch <= 0 {
			batch = 1000
		}
		for {
			res, err := l.db.ExecContext(ctx, query, category, cutoff, minCP, batch)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				break
			}
			slog.Info("retention deleted", "category", category, "rows", n)
			if n < int64(batch) {
				break
			}
		}
	}
	return nil
}
