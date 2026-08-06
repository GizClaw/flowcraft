package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ContextProvider selects, hydrates, and packs memory for an agent turn.
// Retrieval indexes and fusion strategies remain implementation details.
type ContextProvider interface {
	Context(context.Context, ContextRequest) (ContextResult, error)
}

type ContextRequest struct {
	Scope           Scope
	ConversationID  string
	DatasetIDs      []string
	Query           string
	Budget          Budget
	RecentLimit     int
	RecentMaxTokens int
	MinScore        float64
	Metadata        Metadata
	// RecallEventID is a stable invocation/event identity. Providers must not
	// reinforce retrieval when it is empty.
	RecallEventID string
}

func (r ContextRequest) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Query) == "" && strings.TrimSpace(r.ConversationID) == "" {
		return NewError(KindInvalidRequest, "context", errors.New("memory: context query or conversation_id is required"))
	}
	if err := r.Budget.Validate(); err != nil {
		return err
	}
	if math.IsNaN(r.MinScore) || math.IsInf(r.MinScore, 0) || r.MinScore < 0 || r.MinScore > 1 {
		return NewError(KindInvalidRequest, "context", fmt.Errorf("memory: min_score must be in [0,1], got %v", r.MinScore))
	}
	if r.RecentLimit < 0 || r.RecentMaxTokens < 0 {
		return NewError(KindInvalidRequest, "context", errors.New("memory: recent limits must not be negative"))
	}
	if r.RecallEventID != "" && strings.TrimSpace(r.RecallEventID) != r.RecallEventID {
		return NewError(KindInvalidRequest, "context", errors.New("memory: recall_event_id must be stable and trimmed"))
	}
	return nil
}

type ContextResult struct {
	Items         []ContextItem
	TokenCount    int
	Truncated     bool
	RecallEventID string
}

// RecallEvent records only items actually returned from long-term or summary
// retrieval. ItemIDs and Scores are parallel canonical arrays.
type RecallEvent struct {
	ID      string
	Scope   Scope
	ItemIDs []string
	Time    time.Time
	Scores  []float64
}

func (event RecallEvent) Validate() error {
	if strings.TrimSpace(event.ID) == "" {
		return errors.New("memory: recall event id is required")
	}
	if err := event.Scope.Validate(); err != nil {
		return err
	}
	if event.Time.IsZero() || len(event.ItemIDs) == 0 || len(event.ItemIDs) != len(event.Scores) {
		return errors.New("memory: recall event requires time and parallel non-empty items/scores")
	}
	if !sort.StringsAreSorted(event.ItemIDs) {
		return errors.New("memory: recall event item_ids must be sorted")
	}
	for index, id := range event.ItemIDs {
		if strings.TrimSpace(id) == "" || (index > 0 && event.ItemIDs[index-1] == id) {
			return errors.New("memory: recall event item_ids must be unique and non-empty")
		}
		score := event.Scores[index]
		if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
			return errors.New("memory: recall event scores must be in [0,1]")
		}
	}
	return nil
}

func (r ContextResult) Validate() error {
	if r.TokenCount < 0 {
		return errors.New("memory: context result token_count must not be negative")
	}
	if r.RecallEventID != "" && strings.TrimSpace(r.RecallEventID) != r.RecallEventID {
		return errors.New("memory: result recall_event_id must be trimmed")
	}
	for index, item := range r.Items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("memory: context result item %d: %w", index, err)
		}
	}
	return nil
}

func (r ContextResult) Clone() ContextResult {
	r.Items = append([]ContextItem(nil), r.Items...)
	for index := range r.Items {
		r.Items[index] = r.Items[index].Clone()
	}
	return r
}
