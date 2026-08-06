package message

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

// Record is one canonical message committed to a conversation.
type Record struct {
	ID             string             `json:"id"`
	Scope          sdkmemory.Scope    `json:"scope"`
	ConversationID string             `json:"conversation_id"`
	Seq            uint64             `json:"seq"`
	Message        sdkmessage.Message `json:"message"`
	Metadata       sdkmemory.Metadata `json:"metadata,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
}

// AppendRequest atomically commits Messages in their original order.
// IdempotencyKey is scoped to Scope's hard partition and ConversationID.
type AppendRequest struct {
	Scope          sdkmemory.Scope
	ConversationID string
	IdempotencyKey string
	Messages       []sdkmessage.Message
	Metadata       sdkmemory.Metadata
}

// Commit is one immutable, versioned turn and is also the durable derivation
// work item. Version is the final message sequence in the commit.
type Commit struct {
	ID             string          `json:"id"`
	Scope          sdkmemory.Scope `json:"scope"`
	ConversationID string          `json:"conversation_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Version        uint64          `json:"version"`
	Records        []Record        `json:"records"`
	CreatedAt      time.Time       `json:"created_at"`
}

// ListCommitOptions selects commits strictly after AfterVersion.
type ListCommitOptions struct {
	AfterVersion uint64
	Limit        int
}

// ListOptions selects records strictly after AfterSeq. A non-positive Limit
// means no limit.
type ListOptions struct {
	AfterSeq uint64
	Limit    int
}

// LatestOptions selects the newest records by canonical commit sequence.
// Results are returned in ascending Seq so they can be packed as dialogue.
type LatestOptions struct {
	Limit int
}

func cloneRecord(record Record) Record {
	record.Message = record.Message.Clone()
	record.Metadata = record.Metadata.Clone()
	return record
}

func cloneRecords(records []Record) []Record {
	if records == nil {
		return nil
	}
	out := make([]Record, len(records))
	for index, record := range records {
		out[index] = cloneRecord(record)
	}
	return out
}

func cloneCommit(commit Commit) Commit {
	commit.Records = cloneRecords(commit.Records)
	return commit
}

func commitFromPersisted(value persistedCommit) Commit {
	last := value.Records[len(value.Records)-1]
	return Commit{
		ID:             commitID(last.Scope, value.ConversationID, value.IdempotencyKey),
		Scope:          last.Scope,
		ConversationID: value.ConversationID,
		IdempotencyKey: value.IdempotencyKey,
		Version:        last.Seq,
		Records:        cloneRecords(value.Records),
		CreatedAt:      value.Records[0].CreatedAt,
	}
}

func commitID(scope sdkmemory.Scope, conversationID, key string) string {
	sum := sha256.Sum256([]byte(scope.HardPartitionKey() + "\x00" + conversationID + "\x00" + key))
	return "commit-" + hex.EncodeToString(sum[:])
}
