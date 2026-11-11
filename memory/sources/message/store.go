package message

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

const messageSchemaVersion = 1

// Record is one canonical message committed to a conversation.
type Record struct {
	ID             string              `json:"id"`
	Scope          corememory.Scope    `json:"scope"`
	ConversationID string              `json:"conversation_id"`
	Seq            uint64              `json:"seq"`
	Message        coremessage.Message `json:"message"`
	Metadata       corememory.Metadata `json:"metadata,omitempty"`
	CreatedAt      time.Time           `json:"created_at"`
}

// AppendRequest atomically commits Messages in their original order.
// IdempotencyKey is scoped to Scope's hard partition and ConversationID.
type AppendRequest struct {
	Scope          corememory.Scope
	ConversationID string
	IdempotencyKey string
	Messages       []coremessage.Message
	// MessageMetadata optionally tags each message on its own, positionally
	// aligned with Messages. A nil or empty entry stores Metadata instead, so a
	// caller that tags only some messages does not have to repeat the commit's
	// own metadata.
	MessageMetadata []corememory.Metadata
	Metadata        corememory.Metadata
}

// Commit is one immutable, versioned turn and is also the durable derivation
// work item. Version is the final message sequence in the commit.
type Commit struct {
	ID             string           `json:"id"`
	Scope          corememory.Scope `json:"scope"`
	ConversationID string           `json:"conversation_id"`
	IdempotencyKey string           `json:"idempotency_key"`
	Version        uint64           `json:"version"`
	Records        []Record         `json:"records"`
	CreatedAt      time.Time        `json:"created_at"`
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

// messageMetadata returns the metadata stored with message index: its own
// entry when the caller tagged it, the commit's metadata otherwise.
func (request AppendRequest) messageMetadata(index int) corememory.Metadata {
	if index < len(request.MessageMetadata) && len(request.MessageMetadata[index]) > 0 {
		return request.MessageMetadata[index]
	}
	return request.Metadata
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

func commitID(scope corememory.Scope, conversationID, key string) string {
	sum := sha256.Sum256([]byte(scope.HardPartitionKey() + "\x00" + conversationID + "\x00" + key))
	return "commit-" + hex.EncodeToString(sum[:])
}

func messageID(seq uint64) string {
	return fmt.Sprintf("msg-%020d", seq)
}

func parseMessageID(id string) (uint64, bool) {
	if !strings.HasPrefix(id, "msg-") || len(id) != len("msg-")+20 {
		return 0, false
	}
	var seq uint64
	if _, err := fmt.Sscanf(strings.TrimPrefix(id, "msg-"), "%020d", &seq); err != nil {
		return 0, false
	}
	return seq, seq > 0 && messageID(seq) == id
}

func validateAppend(request AppendRequest) error {
	if err := validateAddress(request.Scope, request.ConversationID); err != nil {
		return err
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return errors.New("message source: idempotency_key is required")
	}
	if len(request.Messages) == 0 {
		return errors.New("message source: messages are required")
	}
	if len(request.MessageMetadata) > 0 && len(request.MessageMetadata) != len(request.Messages) {
		return fmt.Errorf("message source: %d message metadata entries for %d messages",
			len(request.MessageMetadata), len(request.Messages))
	}
	for index, item := range request.Messages {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("message source: message %d: %w", index, err)
		}
	}
	return nil
}

func validateAddress(scope corememory.Scope, conversationID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("message source: conversation_id is required")
	}
	return nil
}
