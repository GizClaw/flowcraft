package permission

// Policy is the permission decision for one turn: what it may write, and what
// it may reach over the network. It is a value type; treat it as immutable.
type Policy struct {
	// Writes lists the absolute paths a turn may write.
	Writes Writes
	// Net holds the outbound network posture.
	Net NetPolicy
}

// IsZero reports whether the policy grants nothing and restricts nothing,
// which is what an unconfigured host produces.
func (p Policy) IsZero() bool {
	return p.Writes.IsZero() && p.Net.IsZero()
}

// Validate checks the invariants every sandbox projection relies on.
func (p Policy) Validate() error {
	if err := p.Writes.Validate(); err != nil {
		return err
	}
	return p.Net.Validate()
}
