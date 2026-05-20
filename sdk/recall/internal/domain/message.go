package domain

import "time"

// Message is one caller-supplied conversational turn for LLM extract
// prompt context (Phase D.7). Recall does not fetch history itself —
// callers compose RecentMessages from their own history store.
type Message struct {
	Role    string
	Speaker string
	Text    string
	Time    time.Time
}
