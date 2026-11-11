package script

import (
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Config is one script node's written configuration.
//
// It names a backend and a source, and it declares the state the script may
// touch. Everything else is budget.
type Config struct {
	// Backend is the execution engine, resolved against the table the host
	// passed to [New]. An unregistered name is a build error listing what is
	// registered, the same rule an unknown driver or node kind follows.
	Backend string `json:"backend"`
	// Path is the source reference handed to the [Source]. A file source reads
	// it, and a relative path is resolved against the source's base directory,
	// not the process working directory.
	Path string `json:"path"`
	// Reads names the state keys projected into the script. A key the graph does
	// not declare is an error: a rule that silently reads zero values is worse
	// than one that fails.
	Reads []string `json:"reads"`
	// Writes names the state keys the script may return. A write outside this
	// list is an error, not a silently dropped value.
	Writes []string `json:"writes"`
	// MaxOutputBytes caps the total encoded size of one invocation's writes.
	// Zero means unbounded. Over the cap is an error, never a truncation: a rule
	// that wrote half a state would carry on with half a fact.
	MaxOutputBytes int `json:"max_output_bytes"`
	// Budget bounds one invocation.
	Budget Budget `json:"budget"`
}

// Budget is what one invocation may consume.
//
// The currencies differ by backend, and a backend that cannot enforce a currency
// a config asks for fails construction instead of ignoring the request.
type Budget struct {
	// MaxSteps caps the interpreter work one invocation may do. It is the
	// reproducible bound: the same source and state take the same number of
	// steps everywhere. Zero means the backend's default.
	MaxSteps uint64 `json:"max_steps"`
	// MemoryMiB caps the memory the script may allocate. Zero means undeclared.
	// Only a backend that isolates the script process can honour it.
	MemoryMiB int `json:"memory_mib"`
}

// Capabilities is what a backend can promise.
//
// It is a declaration, not a description: the node refuses a configuration the
// backend cannot honour, so a document can never ask for a bound it silently does
// not get.
type Capabilities struct {
	// Deterministic means the same source and state produce the same result:
	// no clock, no randomness, no IO.
	Deterministic bool
	// Terminating means every source ends, given a step budget.
	Terminating bool
	// MemoryBounded means Budget.MemoryMiB is enforced.
	MemoryBounded bool
	// Isolated means the script runs outside the host process.
	Isolated bool
}

// decodeConfig parses and validates one node's config. It is called at Build and
// again on every invocation (the engine caches the decoded value, so the second
// call usually does not happen).
func decodeConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if err := graph.DecodeConfig(raw, &cfg); err != nil {
		return Config{}, errdefs.Validationf("script: config: %w", err)
	}
	if strings.TrimSpace(cfg.Backend) == "" {
		return Config{}, errdefs.Validationf("script: config.backend is required")
	}
	if strings.TrimSpace(cfg.Path) == "" {
		return Config{}, errdefs.Validationf("script: config.path is required")
	}
	if err := uniqueKeys("reads", cfg.Reads); err != nil {
		return Config{}, err
	}
	if err := uniqueKeys("writes", cfg.Writes); err != nil {
		return Config{}, err
	}
	if cfg.MaxOutputBytes < 0 {
		return Config{}, errdefs.Validationf(
			"script: config.max_output_bytes must not be negative")
	}
	if cfg.Budget.MemoryMiB < 0 {
		return Config{}, errdefs.Validationf(
			"script: config.budget.memory_mib must not be negative")
	}
	return cfg, nil
}

// uniqueKeys rejects a duplicate in one declared key list: reading the same key
// twice is a document bug, and writing it twice would make the projection order
// decide the result.
func uniqueKeys(field string, keys []string) error {
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			return errdefs.Validationf("script: config.%s holds an empty key", field)
		}
		if _, duplicate := seen[key]; duplicate {
			return errdefs.Validationf("script: config.%s declares %q twice", field, key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// checkCapabilities refuses a configuration the backend cannot honour.
//
// It runs at decode time, so a document asking for a step budget on a backend
// without a termination guarantee fails when the graph is built rather than when
// a run finally hangs.
func checkCapabilities(cfg Config, backend string, caps Capabilities) error {
	if cfg.Budget.MaxSteps > 0 && !caps.Terminating {
		return errdefs.Validationf(
			"script: config.budget.max_steps needs a backend that terminates; %q does not declare it",
			backend)
	}
	if cfg.Budget.MemoryMiB > 0 && !caps.MemoryBounded {
		return errdefs.Validationf(
			"script: config.budget.memory_mib needs a backend that bounds memory; %q does not declare it",
			backend)
	}
	return nil
}
