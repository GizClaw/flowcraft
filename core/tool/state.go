package tool

// sessionState is the per-session injection state. All mutations
// happen through explicit methods; Definitions() only reads a
// snapshot, so the visibility computation stays a pure function of
// (candidates, state, policy).
type sessionState struct {
	required   map[string]struct{}
	discovered map[string]discoveredEntry
	turn       uint64
	seq        uint64
}

func newSessionState() *sessionState {
	return &sessionState{
		required:   make(map[string]struct{}),
		discovered: make(map[string]discoveredEntry),
	}
}

// discoveredEntry tracks one tool in the discovery pool. lastUse is the
// turn of the latest use/discovery refresh; seq breaks same-turn ties in
// deterministic MRU order. bytes is the serialized definition size used
// for budget accounting (re-measured from live definitions each round).
type discoveredEntry struct {
	lastUse uint64
	seq     uint64
	bytes   int64
}

func (s *sessionState) snapshot() stateSnapshot {
	snap := stateSnapshot{
		required:   make(map[string]struct{}, len(s.required)),
		discovered: make(map[string]discoveredEntry, len(s.discovered)),
		turn:       s.turn,
	}
	for name := range s.required {
		snap.required[name] = struct{}{}
	}
	for name, entry := range s.discovered {
		snap.discovered[name] = entry
	}
	return snap
}

type stateSnapshot struct {
	required   map[string]struct{}
	discovered map[string]discoveredEntry
	turn       uint64
}

func (s stateSnapshot) isRequired(name string) bool {
	_, ok := s.required[name]
	return ok
}

func (s stateSnapshot) isDiscovered(name string) bool {
	_, ok := s.discovered[name]
	return ok
}

func (s *sessionState) require(names ...string) {
	for _, name := range names {
		s.required[name] = struct{}{}
	}
}

// touch inserts or refreshes one discovery pool entry as the most
// recently used item, recording the current definition size.
func (s *sessionState) touch(name string, size int64) {
	s.seq++
	s.discovered[name] = discoveredEntry{
		lastUse: s.turn,
		seq:     s.seq,
		bytes:   size,
	}
}

func (s *sessionState) removeDiscovered(name string) {
	delete(s.discovered, name)
}

func (s *sessionState) discoveredTotals() (int, int64) {
	var total int64
	for _, entry := range s.discovered {
		total += entry.bytes
	}
	return len(s.discovered), total
}

// advanceTurn increments the round counter and returns the names whose
// discovery entries have gone idle (no use for idleRounds rounds) or
// whose tools are no longer in the catalog. Callers perform the actual
// removal so the catalog check stays outside the state type.
func (s *sessionState) advanceTurn(idleRounds int, alive func(string) bool) []string {
	s.turn++
	var stale []string
	for name, entry := range s.discovered {
		idle := idleRounds > 0 && s.turn-entry.lastUse > uint64(idleRounds)
		if idle || !alive(name) {
			stale = append(stale, name)
		}
	}
	for _, name := range stale {
		delete(s.discovered, name)
	}
	return stale
}
