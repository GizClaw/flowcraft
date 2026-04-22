package eventlog

import (
	"context"
	"log/slog"
	"time"
)

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

// runRetention deletes expired events category by category.
// In R2 this is a no-op that only logs; R3 enables actual deletion.
func runRetention(ctx context.Context, l *SQLiteLog, cfg RetentionConfig, checkpoints CheckpointStore) error {
	// R2: retention goroutine runs but TTL enforcement is disabled.
	// R3 will uncomment the actual DELETE logic below.
	slog.Debug("retention tick", "categories", len(cfg.Categories))
	return nil
}

// runRetentionR3 deletes expired events. Kept as a reference for R3.
func runRetentionR3(ctx context.Context, l *SQLiteLog, cfg RetentionConfig, checkpoints CheckpointStore) error {
	// Get minimum checkpoint to protect unconsumed events.
	minCP, ok, err := checkpoints.Min(ctx)
	if err != nil {
		return err
	}
	if !ok {
		minCP = 0 // no projectors; be conservative
	}

	for category, ttl := range cfg.Categories {
		if ttl == 0 {
			continue // permanent
		}
		cutoff := time.Now().Add(-ttl).Format(time.RFC3339Nano)
		batch := cfg.BatchSize
		if batch <= 0 {
			batch = 1000
		}
		// Partition-aware delete: only delete rows whose seq is behind the slowest consumer.
		query := `DELETE FROM event_log
WHERE category=? AND ts<? AND seq<?
LIMIT ?`
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
		}
	}
	return nil
}
