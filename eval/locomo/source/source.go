// Package source adapts normalized LoCoMo turns into FlowCraft source messages.
package source

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	"github.com/GizClaw/flowcraft/memory"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/sdk/model"
)

const HistoryMessageMimeType = "application/flowcraft.history-message+json"

// SampleScope returns the hard partition used for one LoCoMo sample.
func SampleScope(runID, datasetName string, sample dataset.Sample) memory.Scope {
	return memory.Scope{
		RuntimeID:      runID,
		UserID:         sample.ID,
		ConversationID: sample.ID,
		DatasetID:      datasetName,
	}
}

// IngestSession appends all turns for a session and runs sync memory stages.
func IngestSession(ctx context.Context, mem *memory.System, scope memory.Scope, session dataset.Session) error {
	return IngestTurns(ctx, mem, scope, session, session.Turns)
}

// IngestTurns appends the selected turns and requires a fully synchronous spec.
func IngestTurns(ctx context.Context, mem *memory.System, scope memory.Scope, session dataset.Session, turns []dataset.Turn) error {
	if mem == nil {
		return fmt.Errorf("locomo source: memory system is required")
	}
	messages := make([]sourcemessage.Message, 0, len(turns))
	for _, turn := range turns {
		messages = append(messages, MessageFromTurn(session, turn))
	}
	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope:    scope,
		Messages: messages,
	})
	if err != nil {
		return err
	}
	if len(result.Jobs) != 0 {
		return fmt.Errorf("locomo source: sync memory spec unexpectedly enqueued %d jobs", len(result.Jobs))
	}
	return nil
}

// MessageFromTurn converts one normalized LoCoMo turn into a source message.
func MessageFromTurn(session dataset.Session, turn dataset.Turn) sourcemessage.Message {
	meta := map[string]any{
		"source_kind": "locomo_turn",
		"session":     session.Index,
		"dia_id":      turn.DiaID,
		"speaker":     turn.Speaker,
	}
	if session.DateTime != "" {
		meta["session_date_time"] = session.DateTime
	}
	if turn.ImgURL != "" {
		meta["img_url"] = turn.ImgURL
	}
	if turn.BlipCaption != "" {
		meta["blip_caption"] = turn.BlipCaption
	}
	if turn.Query != "" {
		meta["query"] = turn.Query
	}
	maps.Copy(meta, turn.Metadata)
	createdAt := turn.CreatedAt
	if createdAt.IsZero() && session.DateTime != "" {
		createdAt = ParseLooseTime(session.DateTime)
	}
	return sourcemessage.Message{
		ID:             turn.DiaID,
		ConversationID: "",
		Message: model.Message{
			// LoCoMo turns are historical source messages supplied to memory, not
			// live assistant replies. Preserve the real speaker in text/metadata
			// instead of encoding a fake chat role.
			Role:  model.RoleUser,
			Parts: turnParts(session, turn),
		},
		Metadata:  meta,
		CreatedAt: createdAt,
	}
}

func turnParts(session dataset.Session, turn dataset.Turn) []model.Part {
	parts := []model.Part{historyDataPart(session, turn)}
	if text := renderTurnText(turn); text != "" {
		parts = append(parts, model.Part{Type: model.PartText, Text: text})
	}
	if turn.ImgURL != "" {
		parts = append(parts, model.Part{
			Type:  model.PartImage,
			Image: &model.MediaRef{URL: turn.ImgURL},
		})
	}
	return parts
}

func historyDataPart(session dataset.Session, turn dataset.Turn) model.Part {
	value := map[string]any{
		"source_kind":   "locomo_turn",
		"session_index": session.Index,
		"dia_id":        turn.DiaID,
	}
	if session.DateTime != "" {
		value["session_datetime"] = session.DateTime
	}
	if turn.Speaker != "" {
		value["speaker_name"] = turn.Speaker
	}
	if turn.Query != "" {
		value["image_query"] = turn.Query
	}
	if turn.BlipCaption != "" {
		value["image_caption"] = turn.BlipCaption
	}
	if turn.ImgURL != "" {
		value["image_url"] = turn.ImgURL
	}
	maps.Copy(value, turn.Metadata)
	return model.Part{
		Type: model.PartData,
		Data: &model.DataRef{
			MimeType: HistoryMessageMimeType,
			Value:    value,
		},
	}
}

func renderTurnText(turn dataset.Turn) string {
	var lines []string
	if text := strings.TrimSpace(turn.Text); text != "" {
		lines = append(lines, text)
	}
	if caption := strings.TrimSpace(turn.BlipCaption); caption != "" {
		lines = append(lines, "Image caption: "+caption)
	}
	return strings.Join(lines, "\n")
}

// ParseLooseTime parses the small set of time shapes present in LoCoMo exports.
func ParseLooseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"3:04 pm on 2 January, 2006",
		"3:04 PM on 2 January, 2006",
		"Jan 2, 2006",
		"January 2, 2006",
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}
	return time.Time{}
}
