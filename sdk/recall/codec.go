package recall

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// EntryToDoc serializes a [Entry] into a [retrieval.Doc].
//
// Metadata layout:
//
//	user_id      string
//	agent_id     string
//	categories   []string
//	entities     []string
//	keywords     []string
//	confidence   float64
//	expires_at   int64 (unix-millis), only when ExpiresAt != nil
//	category     string (legacy single-value mirror)
//	runtime_id   string
//
// Note: session_id was removed in v0.2.0 (see Scope godoc). Old rows
// that still carry session_id metadata are read back with the field
// silently dropped, since [DocToEntry] no longer looks for it.
func EntryToDoc(e Entry) retrieval.Doc {
	md := map[string]any{
		"user_id":  e.Scope.UserID,
		"agent_id": e.Scope.AgentID,
	}
	if e.Scope.RuntimeID != "" {
		md["runtime_id"] = e.Scope.RuntimeID
	} else if e.Source.RuntimeID != "" {
		md["runtime_id"] = e.Source.RuntimeID
	}
	if e.Category != "" {
		md["category"] = string(e.Category)
	}
	if len(e.Categories) > 0 {
		md["categories"] = append([]string(nil), e.Categories...)
	}
	if len(e.Entities) > 0 {
		md["entities"] = append([]string(nil), e.Entities...)
	}
	if len(e.Keywords) > 0 {
		md["keywords"] = append([]string(nil), e.Keywords...)
	}
	if e.Confidence > 0 {
		md["confidence"] = e.Confidence
	}
	if e.ExpiresAt != nil && !e.ExpiresAt.IsZero() {
		md["expires_at"] = e.ExpiresAt.UnixMilli()
	}
	ts := e.UpdatedAt
	if ts.IsZero() {
		ts = e.CreatedAt
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	return retrieval.Doc{
		ID:        e.ID,
		Content:   e.Content,
		Metadata:  md,
		Timestamp: ts.UTC(),
	}
}

// DocToEntry reverses [EntryToDoc]. Vector / sparse fields are dropped.
func DocToEntry(d retrieval.Doc) Entry {
	e := Entry{
		ID:        d.ID,
		Content:   d.Content,
		CreatedAt: d.Timestamp,
		UpdatedAt: d.Timestamp,
	}
	if d.Metadata == nil {
		return e
	}
	if v, ok := d.Metadata["user_id"].(string); ok {
		e.Scope.UserID = v
	}
	if v, ok := d.Metadata["agent_id"].(string); ok {
		e.Scope.AgentID = v
	}
	if v, ok := d.Metadata["runtime_id"].(string); ok {
		e.Source.RuntimeID = v
		e.Scope.RuntimeID = v
	}
	if v, ok := d.Metadata["category"].(string); ok {
		e.Category = Category(v)
	}
	e.Categories = stringSlice(d.Metadata["categories"])
	e.Entities = stringSlice(d.Metadata["entities"])
	e.Keywords = stringSlice(d.Metadata["keywords"])
	if v, ok := d.Metadata["confidence"]; ok {
		e.Confidence = toFloat(v)
	}
	if v, ok := d.Metadata["expires_at"]; ok {
		ms := toInt64(v)
		if ms > 0 {
			t := time.UnixMilli(ms).UTC()
			e.ExpiresAt = &t
		}
	}
	if e.Category == "" && len(e.Categories) > 0 {
		e.Category = Category(e.Categories[0])
	}
	return e
}

func stringSlice(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	return 0
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case float32:
		return int64(t)
	}
	return 0
}
