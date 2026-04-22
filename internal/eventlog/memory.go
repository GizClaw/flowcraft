package eventlog

import "context"

// MemoryAppender is a trivial Appender for tests (R1); R2 replaces with SQLite-backed log.
type MemoryAppender struct {
	Seq  int64
	Last []Envelope
	Hook func(Envelope)
}

func (m *MemoryAppender) Append(_ context.Context, env Envelope) (int64, error) {
	m.Seq++
	env.Seq = m.Seq
	if m.Hook != nil {
		m.Hook(env)
	}
	m.Last = append(m.Last, env)
	return m.Seq, nil
}
