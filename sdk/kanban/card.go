package kanban

import "time"

// CardStatus represents the lifecycle state of a Card on the kanban board.
type CardStatus string

const (
	CardPending CardStatus = "pending"
	CardClaimed CardStatus = "claimed"
	CardDone    CardStatus = "done"
	CardFailed  CardStatus = "failed"
)

// Card is a status-tracked message on the board for async multi-agent coordination.
type Card struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Producer  string            `json:"producer"`
	Consumer  string            `json:"consumer"`
	Status    CardStatus        `json:"status"`
	Payload   any               `json:"payload"`
	Error     string            `json:"error,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// CardFilter specifies criteria for querying cards. Zero-value fields act as wildcards.
type CardFilter struct {
	Type     string
	Producer string
	Consumer string
	Status   CardStatus
}

// CardOption configures optional Card fields during Produce.
type CardOption func(*Card)

// WithConsumer sets the target consumer for a card. Default is "*" (broadcast).
func WithConsumer(c string) CardOption {
	return func(card *Card) { card.Consumer = c }
}

// WithMeta attaches a key-value pair to the card's metadata.
func WithMeta(k, v string) CardOption {
	return func(card *Card) {
		if card.Meta == nil {
			card.Meta = make(map[string]string)
		}
		card.Meta[k] = v
	}
}

// WithTimestamps overrides the CreatedAt / UpdatedAt fields that Produce would
// otherwise stamp with time.Now(). Pass the zero time on either argument to
// keep the default (current time) for that field.
//
// Primary use case: RestoreBoard, which must preserve the historical
// creation / update timestamps captured in persistence so timeline / Gantt
// views and SLA metrics survive a process restart.
func WithTimestamps(created, updated time.Time) CardOption {
	return func(card *Card) {
		if !created.IsZero() {
			card.CreatedAt = created
		}
		if !updated.IsZero() {
			card.UpdatedAt = updated
		}
	}
}
