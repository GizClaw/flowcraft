package sandbox

// LocalPolicy describes the daemon-owned policy used by ComposeLocal.
// Zero values are conservative:
//
//   - AllowedCommands nil means no command-name gate is installed.
//     A non-nil empty slice installs a gate that blocks every command.
//   - Predicates nil means no approval tripwire is installed.
//   - Approval may be nil; if a predicate matches, WithApproval then
//     fails closed with PolicyDenied.
type LocalPolicy struct {
	Defaults        ExecOptions
	AllowedCommands []string
	Approval        ApprovalFunc
	Predicates      []Predicate
}

// DefaultLocalPolicy returns the blast-radius defaults for a backend
// rooted at root:
//
//   - ask for approval when WorkDir resolves outside root;
//   - ask for approval for any non-default network posture;
//   - optionally ask for approval when the command base name matches a
//     caller-supplied sensitive pattern.
//
// It deliberately does not guess environment allow-lists, resource
// budgets, or a command allow-list: those values are deployment
// specific and belong in the returned policy's Defaults /
// AllowedCommands fields. Backend filesystem enforcement remains the
// final wall; approval gates an attempt but never widens that wall.
func DefaultLocalPolicy(root string, approval ApprovalFunc, sensitiveCommands ...string) LocalPolicy {
	predicates := []Predicate{
		WorkDirOutsideRoot(root),
		NetNonDefault(),
	}
	if len(sensitiveCommands) > 0 {
		predicates = append(predicates, CommandPatterns(sensitiveCommands...))
	}
	return LocalPolicy{
		Approval:   approval,
		Predicates: predicates,
	}
}

// ComposeLocal builds the recommended local-agent runner chain:
//
//	WithDefaults(
//	    WithApproval(
//	        AllowCommands(backend),
//	    ),
//	)
//
// Decorators whose config is absent are omitted. WithDefaults is
// deliberately outermost: it merges daemon-owned policy before
// WithApproval inspects the call, so the approver sees the effective
// Env / Net / Resources posture rather than the caller's raw request.
// AllowCommands sits closest to the backend and remains an independent
// hard gate: approval cannot bypass a command allow-list.
//
// ComposeLocal adds no writable paths and never modifies backend
// enforcement. Seatbelt callers should use seatbelt.WithWritablePaths
// when constructing the backend for dedicated temp/cache directories.
func ComposeLocal(backend Runner, policy LocalPolicy) Runner {
	runner := backend
	if policy.AllowedCommands != nil {
		runner = AllowCommands(runner, policy.AllowedCommands)
	}
	if len(policy.Predicates) > 0 {
		runner = WithApproval(
			runner,
			policy.Approval,
			policy.Predicates...,
		)
	}
	return WithDefaults(runner, policy.Defaults)
}
