package kanban

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"maps"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

const watchBufSize = 32

type watcher struct {
	filter CardFilter
	ch     chan *Card
}

// Board is the kanban task board: card coordination + scope + EventBus.
// Graph execution uses workflow.Board separately (see sdk/workflow).
type Board struct {
	cards       []*Card
	cardIndex   map[string]*Card
	statusCount map[CardStatus]int
	maxCards    int
	cardTTL     time.Duration

	cardMu   sync.RWMutex
	wmu      sync.Mutex
	watchers []*watcher

	scopeID   string
	bus       event.EventBus
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// TaskBoard is a legacy alias for Board.
//
// Deprecated: Use Board directly. Removed in v0.2.0.
type TaskBoard = Board

// BoardOption configures optional Board parameters.
type BoardOption func(*Board)

// WithMaxCards sets the maximum number of completed cards retained on the board.
// When exceeded, the oldest terminal-state (Done/Failed) cards are evicted.
func WithMaxCards(n int) BoardOption {
	return func(b *Board) { b.maxCards = n }
}

// WithCardTTL sets the time-to-live for terminal-state cards.
// Cards in Done/Failed status older than the TTL are evicted during Produce.
func WithCardTTL(d time.Duration) BoardOption {
	return func(b *Board) { b.cardTTL = d }
}

// NewBoard creates a scope-scoped board with a persistent EventBus.
func NewBoard(scopeID string, opts ...BoardOption) *Board {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Board{
		cards:       make([]*Card, 0),
		cardIndex:   make(map[string]*Card),
		statusCount: make(map[CardStatus]int),
		scopeID:     scopeID,
		bus:         event.NewMemoryBus(),
		ctx:         ctx,
		cancel:      cancel,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// NewTaskBoard is an alias for NewBoard.
//
// Deprecated: Use NewBoard directly. Removed in v0.2.0.
func NewTaskBoard(scopeID string) *Board { return NewBoard(scopeID) }

// Bus returns the persistent EventBus bound to the board.
func (b *Board) Bus() event.EventBus { return b.bus }

// Close releases the persistent EventBus. Safe to call multiple times.
func (b *Board) Close() {
	b.closeOnce.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
		if b.bus != nil {
			_ = b.bus.Close()
		}
	})
}

// ScopeID returns the board owner scope identifier.
func (b *Board) ScopeID() string { return b.scopeID }

// Context returns the board lifecycle context. It is cancelled on Close.
func (b *Board) Context() context.Context { return b.ctx }

// Produce creates a new Card in Pending status and notifies matching watchers.
func (b *Board) Produce(cardType, producer string, payload any, opts ...CardOption) *Card {
	now := time.Now()
	card := &Card{
		ID:        generateKanbanCardID(),
		Type:      cardType,
		Producer:  producer,
		Consumer:  "*",
		Status:    CardPending,
		Payload:   normalizePayload(payload),
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, opt := range opts {
		opt(card)
	}

	b.cardMu.Lock()
	b.cards = append(b.cards, card)
	b.cardIndex[card.ID] = card
	b.statusCount[card.Status]++
	b.evictTerminalCardsLocked()
	b.cardMu.Unlock()

	snap := copyKanbanCard(card)
	b.notifyWatchers(snap)
	return snap
}

// Claim transitions a pending card to claimed status for the given consumer.
func (b *Board) Claim(cardID, consumer string) bool {
	b.cardMu.Lock()
	var snap *Card
	if c, ok := b.cardIndex[cardID]; ok && c.Status == CardPending {
		b.statusCount[c.Status]--
		c.Status = CardClaimed
		c.Consumer = consumer
		c.UpdatedAt = time.Now()
		b.statusCount[c.Status]++
		cp := *c
		cp.Meta = copyMetaStringMap(c.Meta)
		snap = &cp
	}
	b.cardMu.Unlock()
	if snap != nil {
		b.notifyWatchers(snap)
		return true
	}
	return false
}

// Done transitions a claimed card to done status with a result payload.
func (b *Board) Done(cardID string, result any) bool {
	b.cardMu.Lock()
	var snap *Card
	if c, ok := b.cardIndex[cardID]; ok && c.Status == CardClaimed {
		b.statusCount[c.Status]--
		c.Status = CardDone
		c.Payload = normalizePayload(result)
		c.UpdatedAt = time.Now()
		b.statusCount[c.Status]++
		cp := *c
		cp.Meta = copyMetaStringMap(c.Meta)
		snap = &cp
	}
	b.cardMu.Unlock()
	if snap != nil {
		b.notifyWatchers(snap)
		return true
	}
	return false
}

// Fail transitions a pending or claimed card to failed status with an error message.
func (b *Board) Fail(cardID string, errMsg string) bool {
	b.cardMu.Lock()
	var snap *Card
	if c, ok := b.cardIndex[cardID]; ok && (c.Status == CardClaimed || c.Status == CardPending) {
		b.statusCount[c.Status]--
		c.Status = CardFailed
		c.Error = errMsg
		c.UpdatedAt = time.Now()
		b.statusCount[c.Status]++
		cp := *c
		cp.Meta = copyMetaStringMap(c.Meta)
		snap = &cp
	}
	b.cardMu.Unlock()
	if snap != nil {
		b.notifyWatchers(snap)
		return true
	}
	return false
}

// Query returns copies of all cards matching the filter.
func (b *Board) Query(filter CardFilter) []*Card {
	b.cardMu.RLock()
	defer b.cardMu.RUnlock()
	var result []*Card
	for _, c := range b.cards {
		if matchKanbanCard(c, filter) {
			result = append(result, copyKanbanCard(c))
		}
	}
	return result
}

// Last returns a copy of the most recently produced card matching the filter, or nil.
func (b *Board) Last(filter CardFilter) *Card {
	b.cardMu.RLock()
	defer b.cardMu.RUnlock()
	for i := len(b.cards) - 1; i >= 0; i-- {
		if matchKanbanCard(b.cards[i], filter) {
			return copyKanbanCard(b.cards[i])
		}
	}
	return nil
}

// RawCards returns copies of all cards (including internal "result" cards).
func (b *Board) RawCards() []*Card {
	b.cardMu.RLock()
	defer b.cardMu.RUnlock()
	cp := make([]*Card, len(b.cards))
	for i, c := range b.cards {
		cp[i] = copyKanbanCard(c)
	}
	return cp
}

// Len returns the number of cards.
func (b *Board) Len() int {
	b.cardMu.RLock()
	defer b.cardMu.RUnlock()
	return len(b.cards)
}

// GetCardByID returns a deep copy of the card with the given ID, or nil if not found.
func (b *Board) GetCardByID(id string) (*Card, bool) {
	b.cardMu.RLock()
	defer b.cardMu.RUnlock()
	c, ok := b.cardIndex[id]
	if !ok {
		return nil, false
	}
	return copyKanbanCard(c), true
}

// CountByStatus returns the number of cards in the given status.
// If cardType is non-empty, only cards of that type are counted.
func (b *Board) CountByStatus(status CardStatus, cardType string) int {
	b.cardMu.RLock()
	defer b.cardMu.RUnlock()
	if cardType == "" {
		return b.statusCount[status]
	}
	count := 0
	for _, c := range b.cards {
		if c.Status == status && c.Type == cardType {
			count++
		}
	}
	return count
}

// evictTerminalCardsLocked removes the oldest Done/Failed cards when limits are
// exceeded. Must be called with cardMu held.
func (b *Board) evictTerminalCardsLocked() {
	if b.maxCards <= 0 && b.cardTTL <= 0 {
		return
	}
	now := time.Now()
	alive := b.cards[:0]
	for _, c := range b.cards {
		terminal := c.Status == CardDone || c.Status == CardFailed
		evict := false
		if terminal && b.cardTTL > 0 && now.Sub(c.UpdatedAt) > b.cardTTL {
			evict = true
		}
		if evict {
			delete(b.cardIndex, c.ID)
			b.statusCount[c.Status]--
		} else {
			alive = append(alive, c)
		}
	}
	b.cards = alive

	if b.maxCards > 0 && len(b.cards) > b.maxCards {
		excess := len(b.cards) - b.maxCards
		evicted := 0
		alive = b.cards[:0]
		for _, c := range b.cards {
			terminal := c.Status == CardDone || c.Status == CardFailed
			if terminal && evicted < excess {
				delete(b.cardIndex, c.ID)
				b.statusCount[c.Status]--
				evicted++
			} else {
				alive = append(alive, c)
			}
		}
		b.cards = alive
	}
}

// WatchFiltered returns a channel that receives newly produced cards matching the filter.
// The channel is closed when ctx is cancelled.
func (b *Board) WatchFiltered(ctx context.Context, filter CardFilter) <-chan *Card {
	w := &watcher{
		filter: filter,
		ch:     make(chan *Card, watchBufSize),
	}

	b.wmu.Lock()
	b.watchers = append(b.watchers, w)
	b.wmu.Unlock()

	go func() {
		<-ctx.Done()
		b.wmu.Lock()
		for i, ww := range b.watchers {
			if ww == w {
				b.watchers = append(b.watchers[:i], b.watchers[i+1:]...)
				break
			}
		}
		b.wmu.Unlock()
		close(w.ch)
	}()

	return w.ch
}

// Watch subscribes to all card changes. If ctx is nil, the subscription uses the board lifecycle context.
// The returned channel is closed when the outer ctx is done or the board is closed.
func (b *Board) Watch(ctx context.Context) <-chan *Card {
	if ctx == nil {
		return b.WatchFiltered(b.ctx, CardFilter{})
	}
	watchCtx, cancel := context.WithCancel(b.ctx)
	stop := context.AfterFunc(ctx, cancel)
	ch := b.WatchFiltered(watchCtx, CardFilter{})
	go func() {
		<-watchCtx.Done()
		stop()
	}()
	return ch
}

// RemapCardID replaces a card's ID. Used during board restoration from persistence.
func (b *Board) RemapCardID(oldID, newID string) {
	b.cardMu.Lock()
	defer b.cardMu.Unlock()
	if c, ok := b.cardIndex[oldID]; ok {
		delete(b.cardIndex, oldID)
		c.ID = newID
		b.cardIndex[newID] = c
	}
}

// Cards returns a snapshot of task cards on the board (excludes "result" cards).
func (b *Board) Cards() []CardInfo {
	raw := b.RawCards()
	out := make([]CardInfo, 0, len(raw))
	for _, c := range raw {
		if c.Type == "result" {
			continue
		}
		ci := CardInfo{
			ID:        c.ID,
			Type:      c.Type,
			Status:    string(c.Status),
			Producer:  c.Producer,
			Consumer:  c.Consumer,
			Error:     c.Error,
			CreatedAt: c.CreatedAt,
			UpdatedAt: c.UpdatedAt,
			Meta:      c.Meta,
		}
		if c.UpdatedAt.After(c.CreatedAt) {
			ci.ElapsedMs = c.UpdatedAt.Sub(c.CreatedAt).Milliseconds()
		}
		ci.Query, ci.TargetAgentID, ci.Output = extractPayloadFields(c.Payload)
		ci.RunID = extractRunID(c.Payload)
		out = append(out, ci)
	}
	return out
}

// Timeline returns card state history suitable for Gantt chart rendering.
func (b *Board) Timeline() []TimelineEntry {
	raw := b.RawCards()
	out := make([]TimelineEntry, 0, len(raw))
	for _, c := range raw {
		if c.Type == "result" {
			continue
		}
		te := TimelineEntry{
			CardID:    c.ID,
			Type:      c.Type,
			Status:    string(c.Status),
			AgentID:   c.Consumer,
			CreatedAt: c.CreatedAt,
			UpdatedAt: c.UpdatedAt,
			Error:     c.Error,
		}
		if c.UpdatedAt.After(c.CreatedAt) {
			te.ElapsedMs = c.UpdatedAt.Sub(c.CreatedAt).Milliseconds()
		}
		te.Query, te.TargetAgentID, _ = extractPayloadFields(c.Payload)
		out = append(out, te)
	}
	return out
}

// Topology returns the agent-task dependency graph.
func (b *Board) Topology() Topology {
	nodeSet := make(map[string]TopologyNode)
	var edges []TopologyEdge
	for _, c := range b.RawCards() {
		if _, ok := nodeSet[c.Producer]; !ok {
			nodeSet[c.Producer] = TopologyNode{ID: c.Producer, Type: "agent", Name: c.Producer}
		}
		target := c.Consumer
		if target == "" || target == "*" {
			continue
		}
		if _, ok := nodeSet[target]; !ok {
			nodeSet[target] = TopologyNode{ID: target, Type: "agent", Name: target}
		}
		edges = append(edges, TopologyEdge{
			Source: c.Producer,
			Target: target,
			CardID: c.ID,
			Type:   c.Type,
		})
	}
	nodes := make([]TopologyNode, 0, len(nodeSet))
	for _, n := range nodeSet {
		nodes = append(nodes, n)
	}
	return Topology{Nodes: nodes, Edges: edges}
}

// RestoreTaskBoard reconstructs a Board from persisted KanbanCards.
func RestoreTaskBoard(scopeID string, cards []*KanbanCardModel) *Board {
	b := NewBoard(scopeID)
	for _, c := range cards {
		payload := map[string]any{
			"query":           c.Query,
			"target_agent_id": c.TargetAgentID,
		}
		card := b.Produce(c.Type, c.Producer, payload, WithConsumer(c.Consumer))

		remapKanbanCardID(b, card.ID, c.ID)

		switch CardStatus(c.Status) {
		case CardClaimed:
			b.Claim(c.ID, c.Consumer)
		case CardDone:
			donePayload := map[string]any{
				"query":           c.Query,
				"target_agent_id": c.TargetAgentID,
				"output":          c.Output,
				"run_id":          c.RunID,
			}
			b.Claim(c.ID, c.Consumer)
			b.Done(c.ID, donePayload)
		case CardFailed:
			b.Claim(c.ID, c.Consumer)
			b.Fail(c.ID, c.Error)
		}
	}
	return b
}

func remapKanbanCardID(b *Board, oldID, newID string) {
	if oldID == newID {
		return
	}
	b.RemapCardID(oldID, newID)
}

func notifyKanbanMatch(c *Card, f CardFilter) bool {
	if f.Type != "" && c.Type != f.Type {
		return false
	}
	if f.Producer != "" && c.Producer != f.Producer {
		return false
	}
	if f.Consumer != "" && c.Consumer != f.Consumer && c.Consumer != "*" {
		return false
	}
	if f.Status != "" && c.Status != f.Status {
		return false
	}
	return true
}

func matchKanbanCard(c *Card, f CardFilter) bool {
	return notifyKanbanMatch(c, f)
}

func (b *Board) notifyWatchers(snap *Card) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	for _, w := range b.watchers {
		if notifyKanbanMatch(snap, w.filter) {
			select {
			case w.ch <- snap:
			default:
				telemetry.Warn(b.ctx, "kanban: watcher channel full, card notification dropped",
					otellog.String("card_id", snap.ID),
					otellog.String("status", string(snap.Status)))
			}
		}
	}
}

func copyKanbanCard(c *Card) *Card {
	cp := *c
	cp.Meta = copyMetaStringMap(c.Meta)
	cp.Payload = deepCopyJSONValue(c.Payload)
	return &cp
}

func copyMetaStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	maps.Copy(cp, m)
	return cp
}

func generateKanbanCardID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("kanban: failed to generate card ID: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

func deepCopyJSONValue(v any) any {
	if v == nil {
		return nil
	}
	switch v.(type) {
	case string, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, bool:
		return v
	}
	data, err := json.Marshal(v)
	if err != nil {
		telemetry.Warn(context.Background(), "kanban: deepCopyJSONValue marshal failed, returning nil",
			otellog.String("error", err.Error()))
		return nil
	}
	var cp any
	if err := json.Unmarshal(data, &cp); err != nil {
		telemetry.Warn(context.Background(), "kanban: deepCopyJSONValue unmarshal failed, returning nil",
			otellog.String("error", err.Error()))
		return nil
	}
	return cp
}

// normalizePayload converts typed payloads into map[string]any at write time
// so that all reads see a consistent type.
func normalizePayload(v any) any {
	if v == nil {
		return nil
	}
	switch v.(type) {
	case map[string]any:
		return v
	case string, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, bool:
		return v
	}
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var m any
	if err := json.Unmarshal(data, &m); err != nil {
		return v
	}
	return m
}

// CardInfo is an API-friendly card representation.
type CardInfo struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	Status        string            `json:"status"`
	Producer      string            `json:"producer"`
	Consumer      string            `json:"consumer"`
	Query         string            `json:"query,omitempty"`
	TargetAgentID string            `json:"target_agent_id,omitempty"`
	Output        string            `json:"output,omitempty"`
	Error         string            `json:"error,omitempty"`
	RunID         string            `json:"run_id,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	ElapsedMs     int64             `json:"elapsed_ms,omitempty"`
	Meta          map[string]string `json:"meta,omitempty"`
}

// TimelineEntry represents a card state snapshot for timeline rendering.
type TimelineEntry struct {
	CardID        string    `json:"card_id"`
	Type          string    `json:"type"`
	Status        string    `json:"status"`
	AgentID       string    `json:"agent_id,omitempty"`
	Query         string    `json:"query,omitempty"`
	TargetAgentID string    `json:"target_agent_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	ElapsedMs     int64     `json:"elapsed_ms,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// TopologyNode represents an agent in the topology graph.
type TopologyNode struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
}

// TopologyEdge represents a task/result flow between nodes.
type TopologyEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	CardID string `json:"card_id"`
	Type   string `json:"type"`
}

// Topology is the agent dependency graph for topology visualization.
type Topology struct {
	Nodes []TopologyNode `json:"nodes"`
	Edges []TopologyEdge `json:"edges"`
}

// KanbanCardModel is a minimal card record for persistence (avoids importing model here).
type KanbanCardModel struct {
	ID            string
	RuntimeID     string
	Type          string
	Status        string
	Producer      string
	Consumer      string
	TargetAgentID string
	Query         string
	Output        string
	Error         string
	RunID         string
	ElapsedMs     int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
