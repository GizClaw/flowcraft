package dynamic

// state is the per-session injection state. All mutations happen
// through explicit methods; Definitions() only reads a snapshot, so the
// visibility computation stays a pure function of (candidates, state,
// policy).
type state struct {
	required map[string]struct{}
	selected map[string]int
	recent   map[string]uint64
	turn     uint64
}

func newState() *state {
	return &state{
		required: make(map[string]struct{}),
		selected: make(map[string]int),
		recent:   make(map[string]uint64),
	}
}

// snapshot returns a defensive copy safe to read without the state
// mutex held.
func (s *state) snapshot() stateSnapshot {
	snap := stateSnapshot{
		required: make(map[string]struct{}, len(s.required)),
		selected: make(map[string]int, len(s.selected)),
		recent:   make(map[string]uint64, len(s.recent)),
		turn:     s.turn,
	}
	for name := range s.required {
		snap.required[name] = struct{}{}
	}
	for name, rounds := range s.selected {
		snap.selected[name] = rounds
	}
	for name, at := range s.recent {
		snap.recent[name] = at
	}
	return snap
}

type stateSnapshot struct {
	required map[string]struct{}
	selected map[string]int
	recent   map[string]uint64
	turn     uint64
}

func (s stateSnapshot) isRequired(name string) bool {
	_, ok := s.required[name]
	return ok
}

func (s stateSnapshot) isRecent(name string, window int) bool {
	at, ok := s.recent[name]
	if !ok || window <= 0 {
		return false
	}
	return s.turn-at <= uint64(window)
}

// require adds names to the RequiredByName set. Idempotent.
func (s *state) require(names ...string) {
	for _, name := range names {
		s.required[name] = struct{}{}
	}
}

// selectNames marks names as selected for retention rounds. Selecting
// an already selected tool refreshes its remaining rounds to the full
// retention, matching "Selected 保持 M 轮" semantics.
func (s *state) selectNames(names []string, retention int) {
	for _, name := range names {
		s.selected[name] = retention
	}
}

// recordCall marks a tool as selected and recently used at the current
// turn.
func (s *state) recordCall(name string, retention int) {
	s.selected[name] = retention
	s.recent[name] = s.turn
}

// advanceTurn moves to the next round and expires selected tools whose
// remaining rounds reached zero. UsedRecently entries older than the
// window are dropped so the recent map stays bounded.
func (s *state) advanceTurn(recentWindow int) {
	s.turn++
	for name, rounds := range s.selected {
		if rounds <= 1 {
			delete(s.selected, name)
			continue
		}
		s.selected[name] = rounds - 1
	}
	for name, at := range s.recent {
		if s.turn-at > uint64(recentWindow) {
			delete(s.recent, name)
		}
	}
}
