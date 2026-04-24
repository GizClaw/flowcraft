// Aggregator for v0.3.0 removals. Every symbol declared in this file is
// scheduled for removal in v0.3.0; new code should not depend on it.
//
// Why this file exists at all: the v0.2.x surface exposed a handful of
// top-level helpers (Archive, RecoverArchive, LoadArchivedMessages,
// LoadManifest, SaveManifest, Closer.Close) that each let callers reach
// into the implementation in incompatible ways and bypass per-
// conversation serialization. The v0.3 redesign funnels every state-
// mutating operation through [Coordinator]; the wrappers here exist
// only so existing callers keep compiling for one release while they
// migrate.

package history

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Archive runs message archiving for one conversation.
//
// Deprecated: use [Coordinator.Archive] (obtained by type-asserting the
// [History] returned by [NewCompacted] to [Coordinator]). Direct calls
// here bypass the per-conversation worker queue, which means a
// concurrent [History.Append] can race against the trim step inside
// archive and silently drop messages. Will be removed in v0.3.0.
func Archive(ctx context.Context, ws workspace.Workspace, store Store, prefix, convID string, cfg ArchiveConfig) (ArchiveResult, error) {
	return archiveImpl(ctx, ws, store, prefix, convID, cfg)
}

// RecoverArchive checks for incomplete archive operations and completes them.
//
// Deprecated: [NewCompacted] now performs a startup scan and lazy per-
// conversation recovery automatically; manual calls are no longer
// required. Will be removed in v0.3.0.
func RecoverArchive(ctx context.Context, ws workspace.Workspace, store Store, prefix, archivePrefix, convID string) error {
	return recoverArchiveImpl(ctx, ws, store, prefix, archivePrefix, convID)
}

// LoadArchivedMessages reads messages from gzip archive segments.
//
// Deprecated: cold-segment loading is an implementation detail of the
// history_expand tool. Use the tool registered by [RegisterTools]
// instead of calling this directly. Will be removed in v0.3.0.
func LoadArchivedMessages(ctx context.Context, ws workspace.Workspace, prefix, archivePrefix, convID string, startSeq, endSeq int) ([]model.Message, error) {
	return loadArchivedMessagesImpl(ctx, ws, prefix, archivePrefix, convID, startSeq, endSeq)
}

// LoadManifest reads the archive manifest for a conversation.
//
// Deprecated: the manifest is an internal artefact of [Coordinator];
// callers reasoning about archived state should query
// [Coordinator.Archive]'s result instead. Will be removed in v0.3.0.
func LoadManifest(ctx context.Context, ws workspace.Workspace, prefix, archivePrefix, convID string) (*ArchiveManifest, error) {
	return loadManifestImpl(ctx, ws, prefix, archivePrefix, convID)
}

// SaveManifest writes the archive manifest atomically.
//
// Deprecated: writing the manifest from outside [Coordinator] cannot be
// serialized against background archive runs and will corrupt accounting
// under concurrency. Will be removed in v0.3.0.
func SaveManifest(ctx context.Context, ws workspace.Workspace, prefix, archivePrefix, convID string, m *ArchiveManifest) error {
	return saveManifestImpl(ctx, ws, prefix, archivePrefix, convID, m)
}
