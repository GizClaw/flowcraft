package recall

import "errors"

// Errors exposed to callers (, §7, §8).
var (
	ErrMissingUserID    = errors.New("ltm: scope.user_id is required")
	ErrMissingRuntimeID = errors.New("ltm: scope.runtime_id is required")
	ErrJournalRequired  = errors.New("ltm: history/rollback require a journal-wrapped Index")
	ErrJobNotFound      = errors.New("ltm: job not found")
	ErrAwaitTimeout     = errors.New("ltm: AwaitJob timed out")
)
