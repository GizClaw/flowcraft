package script

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/goccy/go-json"
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
	// Allow names the host tools this script may call. An entry is
	// "tool:<name>". Empty means the script can call nothing, which is the
	// default: a rule reads state, and reaching outside it is a decision a
	// document has to state.
	Allow []string `json:"allow"`
	// MaxOutputBytes caps the total encoded size of one invocation's writes.
	// Zero means unbounded. Over the cap is an error, never a truncation: a rule
	// that wrote half a state would carry on with half a fact.
	MaxOutputBytes int `json:"max_output_bytes"`
	// Budget bounds one invocation.
	Budget Budget `json:"budget"`
	// Version says which source a resumed run may use. "latest" (the default)
	// takes whatever the source holds now; "pinned" refuses to continue under a
	// version other than the one the task recorded, which is what makes a fix
	// land only on new games.
	Version string `json:"version"`
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
	// Tools means a script may call host tools through [Host.Tool].
	Tools bool
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
	if err := checkVersion(cfg.Version); err != nil {
		return Config{}, err
	}
	if err := uniqueKeys("allow", cfg.Allow); err != nil {
		return Config{}, err
	}
	if _, err := toolNames(cfg.Allow); err != nil {
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
	if len(cfg.Allow) > 0 && !caps.Tools {
		return errdefs.Validationf(
			"script: config.allow needs a backend that can call tools; %q does not declare it",
			backend)
	}
	if cfg.Budget.MemoryMiB > 0 && !caps.MemoryBounded {
		return errdefs.Validationf(
			"script: config.budget.memory_mib needs a backend that bounds memory; %q does not declare it",
			backend)
	}
	return nil
}

// toolNames parses config.allow into the set of tool names it permits.
//
// The prefix is required rather than implied: a capability list that grows other
// kinds later (a model call, a file read) must not be distinguishable from a tool
// name by position alone.
func toolNames(allow []string) (map[string]struct{}, error) {
	if len(allow) == 0 {
		return nil, nil
	}
	names := make(map[string]struct{}, len(allow))
	for _, entry := range allow {
		scheme, name, qualified := strings.Cut(entry, ":")
		if !qualified || name == "" {
			return nil, errdefs.Validationf(
				"script: config.allow entry %q must be \"tool:<name>\"", entry)
		}
		if scheme != "tool" {
			return nil, errdefs.Validationf(
				"script: config.allow entry %q names an unknown capability; the only kind is \"tool\"",
				entry)
		}
		names[name] = struct{}{}
	}
	return names, nil
}

// The version policies a config may name.
const (
	// VersionLatest takes the current source on every run.
	VersionLatest = "latest"
	// VersionPinned keeps a resumed task on the version it recorded.
	VersionPinned = "pinned"
)

func checkVersion(value string) error {
	switch value {
	case "", VersionLatest, VersionPinned:
		return nil
	}
	return errdefs.Validationf(
		"script: config.version %q is not supported; use %q or %q",
		value, VersionLatest, VersionPinned)
}
