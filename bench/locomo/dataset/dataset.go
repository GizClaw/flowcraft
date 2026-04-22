// Package dataset defines the conversation/question schema shared by all
// LoCoMo-style benchmarks.
package dataset

import (
	"encoding/json"
	"fmt"
	"os"
)

// Turn is a single conversation utterance.
type Turn struct {
	Role       string `json:"role"`                  // "user" | "assistant"
	Content    string `json:"content"`               // raw text
	EvidenceID string `json:"evidence_id,omitempty"` // upstream id (e.g. LoCoMo dia_id "D1:3"); preserved as MemoryEntry.ID by SaveRaw so recall.k_hit is meaningful.
	SessionID  string `json:"session_id,omitempty"`  // upstream session bucket (e.g. LoCoMo "session_3"); used by eval to chunk LLM extractor calls so a single Save doesn't exceed model context / output budget.
}

// Conversation is a single dialog history that the runner ingests via Save.
type Conversation struct {
	ID    string `json:"id"`
	Turns []Turn `json:"turns"`
}

// Question evaluates one query against the memory store after ingestion.
type Question struct {
	ID             string   `json:"id"`
	ConversationID string   `json:"conversation_id"`
	Query          string   `json:"query"`
	GoldAnswers    []string `json:"gold_answers"`           // any-match counts as correct (EM)
	Tags           []string `json:"tags,omitempty"`         // e.g. "temporal", "entity"
	EvidenceIDs    []string `json:"evidence_ids,omitempty"` // optional: doc IDs that should rank top-k
}

// Dataset is one ingest+evaluate corpus.
type Dataset struct {
	Name          string         `json:"name"`
	Conversations []Conversation `json:"conversations"`
	Questions     []Question     `json:"questions"`
}

// LoadJSONL loads a Dataset from a single .jsonl file with the layout:
//
//	{"type":"conversation", ...}
//	{"type":"question",     ...}
func LoadJSONL(path string) (*Dataset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ds := &Dataset{Name: path}
	dec := json.NewDecoder(newLineReader(b))
	for dec.More() {
		raw := json.RawMessage{}
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, err
		}
		switch head.Type {
		case "conversation":
			var c Conversation
			if err := json.Unmarshal(raw, &c); err != nil {
				return nil, fmt.Errorf("conversation: %w", err)
			}
			ds.Conversations = append(ds.Conversations, c)
		case "question":
			var q Question
			if err := json.Unmarshal(raw, &q); err != nil {
				return nil, fmt.Errorf("question: %w", err)
			}
			ds.Questions = append(ds.Questions, q)
		default:
			return nil, fmt.Errorf("unknown record type %q", head.Type)
		}
	}
	return ds, nil
}

// Synthetic returns the bundled tiny dataset (no I/O), so unit tests and
// `go run ./bench/locomo/cmd/eval --dataset synthetic` always work.
func Synthetic() *Dataset {
	return &Dataset{
		Name: "synthetic",
		Conversations: []Conversation{
			{
				ID: "c1",
				Turns: []Turn{
					{Role: "user", Content: "I moved from New York to San Francisco last month."},
					{Role: "assistant", Content: "Welcome to SF! Anything you need?"},
					{Role: "user", Content: "I love black coffee, no sugar."},
				},
			},
			{
				ID: "c2",
				Turns: []Turn{
					{Role: "user", Content: "我家有一只叫旺财的金毛犬。"},
					{Role: "assistant", Content: "好可爱！需要遛狗的提醒吗？"},
				},
			},
		},
		Questions: []Question{
			{
				ID: "q1", ConversationID: "c1",
				Query:       "Where does the user live now?",
				GoldAnswers: []string{"San Francisco", "SF"},
				Tags:        []string{"profile"},
			},
			{
				ID: "q2", ConversationID: "c1",
				Query:       "What kind of coffee does the user prefer?",
				GoldAnswers: []string{"black coffee", "black", "no sugar"},
				Tags:        []string{"preference"},
			},
			{
				ID: "q3", ConversationID: "c2",
				Query:       "用户家里宠物的名字是什么?",
				GoldAnswers: []string{"旺财"},
				Tags:        []string{"entity", "zh"},
			},
		},
	}
}
