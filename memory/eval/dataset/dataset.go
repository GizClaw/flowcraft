// Package dataset defines the canonical eval dataset schema and its JSONL
// codec. It is scenario-neutral: upstream converters produce a Dataset, the
// convert command serializes it, and the run command loads it.
package dataset

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/GizClaw/flowcraft/sdk/message"
)

// Turn is one conversation utterance in the eval JSONL format.
// Message carries the full multimodal content (text, image, data parts);
// EvidenceID and SessionID stay as first-class fields for k_hit mapping and
// ingest batching.
type Turn struct {
	message.Message
	EvidenceID string `json:"evidence_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
}

// Conversation is one dialog history ingested into memory.
type Conversation struct {
	ID    string `json:"id"`
	Turns []Turn `json:"turns"`
}

// Question evaluates one query against a conversation's memory.
type Question struct {
	ID             string   `json:"id"`
	ConversationID string   `json:"conversation_id"`
	Query          string   `json:"query"`
	GoldAnswers    []string `json:"gold_answers"`
	Tags           []string `json:"tags,omitempty"`
	EvidenceIDs    []string `json:"evidence_ids,omitempty"`
	AskedAt        string   `json:"asked_at,omitempty"`
}

// Dataset is one ingest + evaluate corpus.
type Dataset struct {
	Name          string         `json:"name"`
	Conversations []Conversation `json:"conversations"`
	Questions     []Question     `json:"questions"`
}

type conversationRecord struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Turns []Turn `json:"turns"`
}

type questionRecord struct {
	Type           string   `json:"type"`
	ID             string   `json:"id"`
	ConversationID string   `json:"conversation_id"`
	Query          string   `json:"query"`
	GoldAnswers    []string `json:"gold_answers"`
	Tags           []string `json:"tags,omitempty"`
	EvidenceIDs    []string `json:"evidence_ids,omitempty"`
	AskedAt        string   `json:"asked_at,omitempty"`
}

// Load reads and validates a converted eval dataset.
func Load(path string) (*Dataset, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open dataset: %w", err)
	}
	defer func() { _ = file.Close() }()
	dataset, err := Decode(file)
	if err != nil {
		return nil, fmt.Errorf("load dataset %s: %w", path, err)
	}
	dataset.Name = path
	return dataset, nil
}

// Decode reads a JSONL dataset from reader.
func Decode(reader io.Reader) (*Dataset, error) {
	dataset := &Dataset{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		switch head.Type {
		case "conversation":
			var record conversationRecord
			if err := strictUnmarshal(raw, &record); err != nil {
				return nil, fmt.Errorf("line %d conversation: %w", line, err)
			}
			dataset.Conversations = append(dataset.Conversations, Conversation{
				ID:    record.ID,
				Turns: append([]Turn(nil), record.Turns...),
			})
		case "question":
			var record questionRecord
			if err := strictUnmarshal(raw, &record); err != nil {
				return nil, fmt.Errorf("line %d question: %w", line, err)
			}
			dataset.Questions = append(dataset.Questions, Question{
				ID:             record.ID,
				ConversationID: record.ConversationID,
				Query:          record.Query,
				GoldAnswers:    append([]string(nil), record.GoldAnswers...),
				Tags:           append([]string(nil), record.Tags...),
				EvidenceIDs:    append([]string(nil), record.EvidenceIDs...),
				AskedAt:        record.AskedAt,
			})
		default:
			return nil, fmt.Errorf("line %d: unknown record type %q", line, head.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := dataset.Validate(); err != nil {
		return nil, err
	}
	return dataset, nil
}

// Write serializes a dataset to JSONL.
func Write(writer io.Writer, dataset Dataset) error {
	encoder := json.NewEncoder(writer)
	for _, conversation := range dataset.Conversations {
		if err := encoder.Encode(conversationRecord{
			Type: "conversation", ID: conversation.ID, Turns: conversation.Turns,
		}); err != nil {
			return err
		}
	}
	for _, question := range dataset.Questions {
		if err := encoder.Encode(questionRecord{
			Type: "question", ID: question.ID, ConversationID: question.ConversationID,
			Query: question.Query, GoldAnswers: question.GoldAnswers, Tags: question.Tags,
			EvidenceIDs: question.EvidenceIDs, AskedAt: question.AskedAt,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks dataset invariants.
func (d Dataset) Validate() error {
	conversations := make(map[string]struct{}, len(d.Conversations))
	for _, conversation := range d.Conversations {
		if conversation.ID == "" {
			return fmt.Errorf("dataset: conversation id is required")
		}
		if _, exists := conversations[conversation.ID]; exists {
			return fmt.Errorf("dataset: duplicate conversation %q", conversation.ID)
		}
		conversations[conversation.ID] = struct{}{}
	}
	for _, question := range d.Questions {
		if question.ID == "" {
			return fmt.Errorf("dataset: question id is required")
		}
		if _, exists := conversations[question.ConversationID]; !exists {
			return fmt.Errorf("dataset: question %q references unknown conversation %q", question.ID, question.ConversationID)
		}
		if question.Query == "" {
			return fmt.Errorf("dataset: question %q has empty query", question.ID)
		}
		if len(question.GoldAnswers) == 0 {
			return fmt.Errorf("dataset: question %q has no gold answers", question.ID)
		}
	}
	return nil
}

// ApplyConversationLimit keeps the first N conversations and only the
// questions that belong to them, so a truncated dataset stays internally
// consistent.
func ApplyConversationLimit(dataset *Dataset, limit int) {
	if limit <= 0 || len(dataset.Conversations) <= limit {
		return
	}
	dataset.Conversations = dataset.Conversations[:limit]
	kept := make(map[string]struct{}, len(dataset.Conversations))
	for _, conversation := range dataset.Conversations {
		kept[conversation.ID] = struct{}{}
	}
	questions := dataset.Questions[:0]
	for _, question := range dataset.Questions {
		if _, ok := kept[question.ConversationID]; ok {
			questions = append(questions, question)
		}
	}
	dataset.Questions = questions
}

// TurnDataPart preserves structured turn metadata (speaker, session, date,
// evidence id) as a data part that rides through memory storage and
// hydration. Inference-bound paths strip data parts before calling models.
func TurnDataPart(speaker, sessionID, dateTime, evidenceID string) message.DataPart {
	value, _ := json.Marshal(map[string]string{
		"speaker":     speaker,
		"session_id":  sessionID,
		"date_time":   dateTime,
		"evidence_id": evidenceID,
	})
	return message.DataPart{
		MediaType: "application/x.flowcraft.eval.turn+json",
		Value:     value,
	}
}

func strictUnmarshal(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
