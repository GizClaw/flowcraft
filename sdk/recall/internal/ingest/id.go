package ingest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

var (
	_ port.IDGenerator = (*timeOrderedGen)(nil)
	_ port.IDGenerator = (*sequentialGen)(nil)
)

// newULIDGenerator returns the default time-ordered generator. The
// shape is "fct_<unixmillis>_<hex>" so IDs sort by observation order
// while staying globally unique within a process.
func newULIDGenerator() port.IDGenerator {
	return &timeOrderedGen{}
}

type timeOrderedGen struct {
	mu   sync.Mutex
	last int64
	seq  uint16
}

func (g *timeOrderedGen) NewID(_ domain.TemporalFact, now time.Time) string {
	g.mu.Lock()
	ms := now.UnixMilli()
	if ms <= g.last {
		ms = g.last
		g.seq++
	} else {
		g.last = ms
		g.seq = 0
	}
	seq := g.seq
	g.mu.Unlock()

	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	return fmt.Sprintf("fct_%013d_%04x_%s", ms, seq, hex.EncodeToString(rnd[:]))
}

// SequentialIDGenerator returns a deterministic generator for tests.
// IDs follow the pattern prefix + ascending zero-padded counter.
func SequentialIDGenerator(prefix string) port.IDGenerator {
	return &sequentialGen{prefix: prefix}
}

type sequentialGen struct {
	mu     sync.Mutex
	prefix string
	n      int
}

func (g *sequentialGen) NewID(domain.TemporalFact, time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("%s%06d", g.prefix, g.n)
}
