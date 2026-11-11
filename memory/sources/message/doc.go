// Package message provides the canonical source of raw conversation
// messages. Each idempotent turn is an immutable commit stored as a batch of
// record events in a storage.Log. Commit listings come from the Log's commit
// metadata (storage.CommitLog). Records are hard-partitioned by memory scope
// and isolated by conversation. Retrying an idempotency key returns the
// original commit.
package message
