package permission

// Policy is the permission decision for one turn: what it may write, and what
// it may reach over the network. It is a value type; treat it as immutable.
type Policy struct {
	// Files holds the file-access entries.
	Files Files
	// Net holds the outbound network posture.
	Net NetPolicy
}

// IsZero reports whether the policy grants nothing and restricts nothing,
// which is what an unconfigured host produces.
func (p Policy) IsZero() bool {
	return len(p.Files.Entries) == 0 && p.Net.IsZero()
}

// Validate checks the invariants every interpreter relies on: absolute,
// glob-free file paths, and a network policy one backend can enforce.
func (p Policy) Validate() error {
	if err := p.Files.validate(); err != nil {
		return err
	}
	return p.Net.Validate()
}
