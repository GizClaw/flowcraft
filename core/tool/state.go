package tool

// sessionState is the per-session injection state. All mutations
// happen through explicit methods; Definitions() only reads a
// snapshot, so the visibility computation stays a pure function of
// (candidates, state, policy).
type sessionState struct {
	required map[string]struct{}
	selected map[string]int
	recent   map[string]uint64
	turn     uint64
}

func newSessionState() *sessionState {
	return &sessionState{
		required: make(map[string]struct{}),
		selected: make(map[string]int),
		recent:   make(map[string]uint64),
	}
}

func (s *sessionState) snapshot() stateSnapshot {
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

func (s *sessionState) require(names ...string) {
	for _, name := range names {
		s.required[name] = struct{}{}
	}
}

func (s *sessionState) selectNames(names []string, retention int) {
	for _, name := range names {
		s.selected[name] = retention
	}
}

func (s *sessionState) recordCall(name string, retention int) {
	s.selected[name] = retention
	s.recent[name] = s.turn
}

func (s *sessionState) advanceTurn(recentWindow int) {
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
