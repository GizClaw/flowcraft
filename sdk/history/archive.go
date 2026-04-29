package history

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

var (
	archiveMeter = telemetry.MeterWithSuffix("memory.archive")

	archiveDuration, _      = archiveMeter.Float64Histogram("duration", metric.WithDescription("Archive duration in seconds"))
	archiveMessagesTotal, _ = archiveMeter.Int64Counter("messages_total", metric.WithDescription("Total archived messages"))
)

// ArchiveManifest tracks archived message segments.
type ArchiveManifest struct {
	Segments    []ArchiveSegment `json:"segments"`
	HotStartSeq int              `json:"hot_start_seq"`
}

// ArchiveSegment describes a single archived file.
type ArchiveSegment struct {
	File      string    `json:"file"`
	StartSeq  int       `json:"start_seq"`
	EndSeq    int       `json:"end_seq"`
	Count     int       `json:"count"`
	CreatedAt time.Time `json:"created_at"`
}

// ArchiveResult holds the result of an archive operation.
type ArchiveResult struct {
	MessagesArchived int    `json:"messages_archived"`
	ArchiveFile      string `json:"archive_file,omitempty"`
	HotStartSeq      int    `json:"hot_start_seq"`
}

// loadManifestImpl reads the archive manifest for a conversation.
func loadManifestImpl(ctx context.Context, ws workspace.Workspace, prefix, archivePrefix, convID string) (*ArchiveManifest, error) {
	path := manifestPath(prefix, archivePrefix, convID)
	exists, err := ws.Exists(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("archive: check manifest: %w", err)
	}
	if !exists {
		return &ArchiveManifest{HotStartSeq: 0}, nil
	}
	data, err := ws.Read(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("archive: read manifest: %w", err)
	}
	var m ArchiveManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("archive: unmarshal manifest: %w", err)
	}
	return &m, nil
}

// saveManifestImpl writes the archive manifest atomically (write-tmp +
// rename), so a crash mid-write cannot leave readers seeing a half-
// serialized manifest.
func saveManifestImpl(ctx context.Context, ws workspace.Workspace, prefix, archivePrefix, convID string, m *ArchiveManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("archive: marshal manifest: %w", err)
	}
	path := manifestPath(prefix, archivePrefix, convID)
	return workspace.AtomicWrite(ctx, ws, path, data)
}

func manifestPath(prefix, archivePrefix, convID string) string {
	if prefix != "" {
		return fmt.Sprintf("%s/%s/%s/manifest.json", prefix, convID, archivePrefix)
	}
	return fmt.Sprintf("%s/%s/manifest.json", convID, archivePrefix)
}

func intentPath(prefix, archivePrefix, convID string) string {
	return archiveDir(prefix, archivePrefix, convID) + "/intent.json"
}

// archiveIntent records the in-progress archive operation for crash recovery.
type archiveIntent struct {
	ConvID      string `json:"conv_id"`
	StartSeq    int    `json:"start_seq"`
	EndSeq      int    `json:"end_seq"`
	BatchSize   int    `json:"batch_size"`
	ArchiveFile string `json:"archive_file"`
	Phase       string `json:"phase"` // "gzip_written" | "manifest_updated"
}

// writeIntent records the in-progress archive operation atomically.
// RecoverArchive depends on the intent being either fully present or
// fully absent — partial writes would make the recovery state machine
// indeterministic.
func writeIntent(ctx context.Context, ws workspace.Workspace, prefix, archivePrefix, convID string, intent *archiveIntent) error {
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return workspace.AtomicWrite(ctx, ws, intentPath(prefix, archivePrefix, convID), data)
}

// deleteIntent removes the intent file. The previous implementation wrote
// an empty payload; that worked only because loadIntent special-cased
// len(data) == 0, but it left a zombie file behind and meant Exists() lied
// to other code paths. Use Delete for honest semantics.
func deleteIntent(ctx context.Context, ws workspace.Workspace, prefix, archivePrefix, convID string) {
	path := intentPath(prefix, archivePrefix, convID)
	_ = ws.Delete(ctx, path)
}

func loadIntent(ctx context.Context, ws workspace.Workspace, prefix, archivePrefix, convID string) (*archiveIntent, error) {
	path := intentPath(prefix, archivePrefix, convID)
	exists, err := ws.Exists(ctx, path)
	if err != nil || !exists {
		return nil, err
	}
	data, err := ws.Read(ctx, path)
	if err != nil || len(data) == 0 {
		return nil, err
	}
	var intent archiveIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return nil, nil
	}
	return &intent, nil
}

// recoverArchiveImpl checks for incomplete archive operations and
// completes them. The [Coordinator] runs this at construction (startup
// scan) and on the first task per conversation (lazy recovery); the
// deprecated top-level [RecoverArchive] shim calls through to here for
// callers still on the v0.2.x manual-recovery flow.
func recoverArchiveImpl(ctx context.Context, ws workspace.Workspace, store Store, prefix, archivePrefix, convID string) error {
	intent, err := loadIntent(ctx, ws, prefix, archivePrefix, convID)
	if err != nil || intent == nil {
		return err
	}

	telemetry.Info(ctx, "archive: recovering incomplete operation",
		otellog.String(telemetry.AttrConversationID, convID),
		otellog.String("phase", intent.Phase))

	switch intent.Phase {
	case "manifest_updated":
		// Gzip + manifest done, just need to trim messages.
		msgs, err := store.GetMessages(ctx, convID)
		if err != nil {
			return fmt.Errorf("archive: recovery get messages: %w", err)
		}
		if len(msgs) > intent.BatchSize {
			remaining := msgs[intent.BatchSize:]
			if err := store.SaveMessages(ctx, convID, remaining); err != nil {
				return fmt.Errorf("archive: recovery rewrite messages: %w", err)
			}
		}
	case "gzip_written":
		// Gzip done but manifest not updated — update manifest then trim.
		manifest, err := loadManifestImpl(ctx, ws, prefix, archivePrefix, convID)
		if err != nil {
			return fmt.Errorf("archive: recovery load manifest: %w", err)
		}
		// Check idempotency: skip if segment already in manifest.
		alreadyDone := false
		for _, seg := range manifest.Segments {
			if seg.File == intent.ArchiveFile {
				alreadyDone = true
				break
			}
		}
		if !alreadyDone {
			manifest.Segments = append(manifest.Segments, ArchiveSegment{
				File:      intent.ArchiveFile,
				StartSeq:  intent.StartSeq,
				EndSeq:    intent.EndSeq,
				Count:     intent.BatchSize,
				CreatedAt: time.Now(),
			})
			manifest.HotStartSeq = intent.EndSeq + 1
			if err := saveManifestImpl(ctx, ws, prefix, archivePrefix, convID, manifest); err != nil {
				return fmt.Errorf("archive: recovery save manifest: %w", err)
			}
		}
		msgs, err := store.GetMessages(ctx, convID)
		if err != nil {
			return fmt.Errorf("archive: recovery get messages: %w", err)
		}
		if len(msgs) > intent.BatchSize {
			remaining := msgs[intent.BatchSize:]
			if err := store.SaveMessages(ctx, convID, remaining); err != nil {
				return fmt.Errorf("archive: recovery rewrite messages: %w", err)
			}
		}
	}

	deleteIntent(ctx, ws, prefix, archivePrefix, convID)
	telemetry.Info(ctx, "archive: recovery completed", otellog.String(telemetry.AttrConversationID, convID))
	return nil
}

// archiveImpl moves old messages to gzip-compressed archive files. It is
// the package-private implementation called by the [Coordinator] (via
// internalArchive) and by the deprecated top-level [Archive] shim.
//
// Crash recovery is handled by recoverArchiveImpl; new callers should
// drive both through [Coordinator] rather than invoking the package
// helpers directly.
func archiveImpl(ctx context.Context, ws workspace.Workspace, store Store, prefix, convID string, cfg ArchiveConfig) (ArchiveResult, error) {
	start := time.Now()
	defer func() {
		archiveDuration.Record(ctx, time.Since(start).Seconds())
	}()

	ctx, span := telemetry.Tracer().Start(ctx, "memory.archive")
	defer span.End()

	var result ArchiveResult

	msgs, err := store.GetMessages(ctx, convID)
	if err != nil {
		return result, fmt.Errorf("archive: get messages: %w", err)
	}

	threshold := cfg.ArchiveThreshold
	if threshold <= 0 {
		threshold = 1000
	}
	if len(msgs) < threshold {
		return result, nil
	}

	batchSize := cfg.ArchiveBatchSize
	if batchSize <= 0 {
		batchSize = 500
	}
	if batchSize > len(msgs) {
		batchSize = len(msgs) / 2
	}
	if batchSize <= 0 {
		return result, nil
	}

	archivePrefix := cfg.ArchivePrefix
	if archivePrefix == "" {
		archivePrefix = "archive"
	}

	manifest, err := loadManifestImpl(ctx, ws, prefix, archivePrefix, convID)
	if err != nil {
		return result, err
	}

	toArchive := msgs[:batchSize]
	remaining := msgs[batchSize:]

	startSeq := manifest.HotStartSeq
	endSeq := startSeq + batchSize - 1
	archiveFile := fmt.Sprintf("messages_%d_%d.jsonl.gz", startSeq, endSeq)

	// Phase 1: compress and write gzip.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gw)
	enc.SetEscapeHTML(false)
	for _, msg := range toArchive {
		if err := enc.Encode(msg); err != nil {
			_ = gw.Close()
			return result, fmt.Errorf("archive: encode message: %w", err)
		}
	}
	if err := gw.Close(); err != nil {
		return result, fmt.Errorf("archive: gzip close: %w", err)
	}

	archivePath := archiveDir(prefix, archivePrefix, convID) + "/" + archiveFile
	if err := ws.Write(ctx, archivePath, buf.Bytes()); err != nil {
		return result, fmt.Errorf("archive: write gzip: %w", err)
	}

	intent := &archiveIntent{
		ConvID: convID, StartSeq: startSeq, EndSeq: endSeq,
		BatchSize: batchSize, ArchiveFile: archiveFile, Phase: "gzip_written",
	}
	if err := writeIntent(ctx, ws, prefix, archivePrefix, convID, intent); err != nil {
		return result, fmt.Errorf("archive: write intent: %w", err)
	}

	// Phase 2: update manifest.
	manifest.Segments = append(manifest.Segments, ArchiveSegment{
		File:      archiveFile,
		StartSeq:  startSeq,
		EndSeq:    endSeq,
		Count:     batchSize,
		CreatedAt: time.Now(),
	})
	manifest.HotStartSeq = endSeq + 1

	if err := saveManifestImpl(ctx, ws, prefix, archivePrefix, convID, manifest); err != nil {
		return result, fmt.Errorf("archive: save manifest: %w", err)
	}

	intent.Phase = "manifest_updated"
	if err := writeIntent(ctx, ws, prefix, archivePrefix, convID, intent); err != nil {
		return result, fmt.Errorf("archive: update intent: %w", err)
	}

	// Phase 3: trim hot messages.
	if err := store.SaveMessages(ctx, convID, remaining); err != nil {
		return result, fmt.Errorf("archive: rewrite messages: %w", err)
	}

	deleteIntent(ctx, ws, prefix, archivePrefix, convID)

	result.MessagesArchived = batchSize
	result.ArchiveFile = archiveFile
	result.HotStartSeq = manifest.HotStartSeq

	archiveMessagesTotal.Add(ctx, int64(batchSize))
	telemetry.Info(ctx, "archive: completed",
		otellog.Int("messages_archived", batchSize),
		otellog.String("file", archiveFile))

	return result, nil
}

func archiveDir(prefix, archivePrefix, convID string) string {
	if prefix != "" {
		return fmt.Sprintf("%s/%s/%s", prefix, convID, archivePrefix)
	}
	return fmt.Sprintf("%s/%s", convID, archivePrefix)
}

// loadArchivedMessagesImpl reads messages from gzip archive segments. It
// powers history_expand's cold-segment path; callers outside the
// history package should obtain archived turns via the history_expand
// tool registered through [RegisterTools].
func loadArchivedMessagesImpl(ctx context.Context, ws workspace.Workspace, prefix, archivePrefix, convID string, startSeq, endSeq int) ([]model.Message, error) {
	manifest, err := loadManifestImpl(ctx, ws, prefix, archivePrefix, convID)
	if err != nil {
		return nil, err
	}

	var result []model.Message
	dir := archiveDir(prefix, archivePrefix, convID)

	for _, seg := range manifest.Segments {
		if seg.EndSeq < startSeq || seg.StartSeq > endSeq {
			continue
		}

		path := dir + "/" + seg.File
		data, err := ws.Read(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("archive: read %q: %w", path, err)
		}

		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("archive: gzip open %q: %w", path, err)
		}

		decompressed, err := io.ReadAll(gr)
		_ = gr.Close()
		if err != nil {
			return nil, fmt.Errorf("archive: gzip read %q: %w", path, err)
		}

		scanner := json.NewDecoder(bytes.NewReader(decompressed))
		seq := seg.StartSeq
		for scanner.More() {
			var msg model.Message
			if err := scanner.Decode(&msg); err != nil {
				break
			}
			if seq >= startSeq && seq <= endSeq {
				result = append(result, msg)
			}
			seq++
		}
	}

	return result, nil
}
