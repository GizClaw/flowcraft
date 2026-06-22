// Package dataset normalizes LoCoMo JSON payloads.
package dataset

import "time"

// Dataset is a normalized LoCoMo JSON payload.
type Dataset struct {
	Name    string   `json:"name"`
	Samples []Sample `json:"samples"`
}

// Sample is one long-running conversation and its associated tasks.
type Sample struct {
	ID             string            `json:"id"`
	Speakers       map[string]string `json:"speakers,omitempty"`
	Sessions       []Session         `json:"sessions"`
	QA             []QAItem          `json:"qa,omitempty"`
	EventSummaries []EventSummary    `json:"event_summaries,omitempty"`
	DialogCases    []DialogCase      `json:"dialog_cases,omitempty"`
}

// Session contains the ordered dialog turns for one session_n key.
type Session struct {
	Index    int    `json:"index"`
	DateTime string `json:"date_time,omitempty"`
	Turns    []Turn `json:"turns"`
}

// Turn is one normalized dialog turn. Image turns are represented through
// caption-proxy fields so downstream memory can preserve raw source messages.
type Turn struct {
	SessionIndex int            `json:"session_index"`
	DiaID        string         `json:"dia_id,omitempty"`
	Speaker      string         `json:"speaker,omitempty"`
	Text         string         `json:"text,omitempty"`
	ImgURL       string         `json:"img_url,omitempty"`
	BlipCaption  string         `json:"blip_caption,omitempty"`
	Query        string         `json:"query,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	CreatedAt    time.Time      `json:"created_at,omitempty"`
}

// QAItem is one recall question.
type QAItem struct {
	ID                string   `json:"id,omitempty"`
	Question          string   `json:"question"`
	Answer            string   `json:"answer"`
	CategoryID        int      `json:"category_id,omitempty"`
	Category          string   `json:"category,omitempty"`
	Evidence          []string `json:"evidence,omitempty"`
	AdversarialAnswer string   `json:"adversarial_answer,omitempty"`
}

// EventSummary is one gold event-summary target for a session and speaker.
type EventSummary struct {
	SessionIndex int      `json:"session_index"`
	Speaker      string   `json:"speaker,omitempty"`
	Events       []string `json:"events"`
}

// DialogCase is a caption-proxy multimodal next-turn generation case.
type DialogCase struct {
	ID              string `json:"id"`
	SessionIndex    int    `json:"session_index"`
	SourceTurnDiaID string `json:"source_turn_dia_id,omitempty"`
	TargetDiaID     string `json:"target_dia_id,omitempty"`
	ImageURL        string `json:"image_url,omitempty"`
	Caption         string `json:"caption,omitempty"`
	Query           string `json:"query,omitempty"`
	Gold            string `json:"gold"`
}

var qaCategoryNames = map[int]string{
	1: "multi-hop",
	2: "temporal",
	3: "open-domain",
	4: "single-hop",
	5: "adversarial",
}
