package domain

// Scope identifies the tenant/user partition for canonical memory.
//
// RuntimeID and UserID participate in storage / namespace partitioning;
// AgentID is a soft-isolation dimension surfaced through metadata, not
// through partitioning, so a single agent can union its own facts with
// shared ones during recall.
//
// Federation is read-path only: Save / Forget / revision APIs use the
// primary scope and ignore Federation (write path does not federate).
// Federation lists additional sub-scopes to recall from; only one
// level is expanded — sub-scope Federation fields are ignored.
//
// v1 Partition translation:
//
//	Partitions:[User, Global] on scope {RuntimeID: rt, UserID: alice}
//	≈ Federation: []Scope{{RuntimeID: rt}} on the same primary scope.
type Scope struct {
	RuntimeID string
	AgentID   string
	UserID    string

	// Federation lists extra scopes to include on Recall. nil and
	// empty slice are equivalent (primary scope only).
	Federation []Scope
}
