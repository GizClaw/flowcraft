package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/inference"
	yamlv3 "gopkg.in/yaml.v3"
)

// VersionV1 is the only document version the package currently
// supports. The YAML decoder rejects unknown versions.
const VersionV1 = "v1"

// identifierPattern matches the documented name shape for
// implementation ids, store names, and clock impls.
var identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Document is one complete, immutable memory deployment
// snapshot. It is the typed shape the YAML decoder produces and
// that [Builder.NewAssembly] consumes.
type Document struct {
	Version   string                `yaml:"version"`
	Runtime   RuntimeSpec           `yaml:"runtime"`
	Stores    map[string]StoreEntry `yaml:"stores,omitempty"`
	Embedding EmbeddingSpec         `yaml:"embedding,omitempty"`
	Lifecycle LifecycleSpec         `yaml:"lifecycle,omitempty"`
}

// RuntimeSpec captures the runtime-level fields that are not
// per-op. The hard partition must be exactly [runtime_id,
// user_id]; the kernel's [memory.Scope.HardPartitionKey]
// depends on this ordering.
type RuntimeSpec struct {
	// HardPartition names the two fields that make up a
	// Scope's hard partition. The Builder rejects any value
	// that is not exactly [runtime_id, user_id] (in this
	// order) because the kernel's partitioning contract is
	// fixed.
	HardPartition []string `yaml:"hard_partition"`
	// DefaultScope is the Scope the scheduler / hooks fall
	// back to when their own settings do not specify one.
	DefaultScope ScopeSpec `yaml:"default_scope"`
	// Clock names the clock implementation. Currently
	// supported values: "system", "frozen".
	Clock ClockSpec `yaml:"clock"`
}

// ScopeSpec is the YAML shape of [memory.Scope]. Only the
// hard-partition fields are part of the YAML schema; the soft
// fields (AgentID, ConversationID, DatasetID) are request-time
// concerns, not deployment-time.
type ScopeSpec struct {
	RuntimeID string `yaml:"runtime_id"`
	UserID    string `yaml:"user_id,omitempty"`
}

// ClockSpec names a clock implementation. The Builder owns the
// mapping from impl name to a concrete [memory.Clock].
type ClockSpec struct {
	Impl string `yaml:"impl"`
}

// StoreEntry is one slot in the document's stores map. The
// kernel has two logical slots: "messages" (transcript
// persistence: Append + Load + Recall), "documents" (chunk +
// embedding storage: Import + Compact + Archive). The Builder
// wires each slot to the registered StoreFactory whose name
// matches Impl.
type StoreEntry struct {
	Impl     string          `yaml:"impl"`
	Settings json.RawMessage `yaml:"settings,omitempty"`
}

// UnmarshalYAML preserves Settings as JSON while keeping the surrounding YAML
// schema strict. Store implementations therefore receive the same JSON
// contract regardless of whether configuration originated as YAML or JSON.
func (s *StoreEntry) UnmarshalYAML(node *yamlv3.Node) error {
	if node.Kind != yamlv3.MappingNode {
		return fmt.Errorf("memory config: store entry must be a mapping")
	}
	var decoded StoreEntry
	seen := make(map[string]struct{}, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		if key.Kind != yamlv3.ScalarNode || key.Tag != "!!str" {
			return fmt.Errorf("memory config: store entry keys must be strings")
		}
		if _, duplicate := seen[key.Value]; duplicate {
			return fmt.Errorf("memory config: duplicate store entry field %q", key.Value)
		}
		seen[key.Value] = struct{}{}
		switch key.Value {
		case "impl":
			if err := value.Decode(&decoded.Impl); err != nil {
				return err
			}
		case "settings":
			settings, err := yamlNodeJSON(value)
			if err != nil {
				return fmt.Errorf("memory config: store settings: %w", err)
			}
			decoded.Settings = settings
		default:
			return fmt.Errorf("memory config: field %q not found in type config.StoreEntry", key.Value)
		}
	}
	*s = decoded
	return nil
}

func yamlNodeJSON(node *yamlv3.Node) (json.RawMessage, error) {
	if node.Kind == yamlv3.DocumentNode {
		if len(node.Content) != 1 {
			return nil, errors.New("YAML document is empty")
		}
		return yamlNodeJSON(node.Content[0])
	}
	switch node.Kind {
	case yamlv3.MappingNode:
		var output bytes.Buffer
		output.WriteByte('{')
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yamlv3.ScalarNode || key.Tag != "!!str" {
				return nil, errors.New("object keys must be strings")
			}
			if index > 0 {
				output.WriteByte(',')
			}
			encodedKey, _ := json.Marshal(key.Value)
			output.Write(encodedKey)
			output.WriteByte(':')
			encodedValue, err := yamlNodeJSON(node.Content[index+1])
			if err != nil {
				return nil, err
			}
			output.Write(encodedValue)
		}
		output.WriteByte('}')
		return output.Bytes(), nil
	case yamlv3.SequenceNode:
		var output bytes.Buffer
		output.WriteByte('[')
		for index, child := range node.Content {
			if index > 0 {
				output.WriteByte(',')
			}
			encoded, err := yamlNodeJSON(child)
			if err != nil {
				return nil, err
			}
			output.Write(encoded)
		}
		output.WriteByte(']')
		return output.Bytes(), nil
	case yamlv3.ScalarNode:
		switch node.Tag {
		case "!!str":
			return json.Marshal(node.Value)
		case "!!bool":
			var value bool
			if err := node.Decode(&value); err != nil {
				return nil, err
			}
			return json.RawMessage(strconv.FormatBool(value)), nil
		case "!!null":
			return json.RawMessage("null"), nil
		case "!!int", "!!float":
			number := strings.ReplaceAll(node.Value, "_", "")
			if !json.Valid([]byte(number)) {
				return nil, fmt.Errorf("number %q must use JSON number syntax", node.Value)
			}
			return json.RawMessage(number), nil
		default:
			return nil, fmt.Errorf("scalar %q is not JSON-compatible", node.Value)
		}
	case yamlv3.AliasNode:
		return yamlNodeJSON(node.Alias)
	default:
		return nil, errors.New("unsupported YAML node")
	}
}

// EmbeddingSpec configures document embedding through an inference Runtime.
// The all-zero value disables embedding. When enabled, every field is
// required: Dimensions is sent with EmbedRequest and defines the vector schema,
// BatchSize controls import batching, and Timeout bounds inference calls.
type EmbeddingSpec struct {
	Model      inference.ModelRef `yaml:"model,omitempty"`
	Dimensions int                `yaml:"dimensions,omitempty"`
	BatchSize  int                `yaml:"batch_size,omitempty"`
	Timeout    time.Duration      `yaml:"timeout,omitempty"`
}

// Enabled reports whether any embedding setting is configured.
func (e EmbeddingSpec) Enabled() bool {
	return e != (EmbeddingSpec{})
}

// Validate enforces the memory-side embedding policy.
func (e EmbeddingSpec) Validate() error {
	if !e.Enabled() {
		return nil
	}
	if err := e.Model.Validate(); err != nil {
		return fmt.Errorf("memory config: embedding.model: %w", err)
	}
	if e.Dimensions <= 0 {
		return errors.New("memory config: embedding.dimensions must be greater than zero")
	}
	if e.BatchSize <= 0 {
		return errors.New("memory config: embedding.batch_size must be greater than zero")
	}
	if e.Timeout <= 0 {
		return errors.New("memory config: embedding.timeout must be greater than zero")
	}
	return nil
}

// LifecycleSpec captures the scheduler-side config the independent
// scheduler (sdkx/scheduler/memory) reads at runtime. Empty operation
// blocks are disabled. The kernel's [memory.Runtime] does not look at
// these fields.
type LifecycleSpec struct {
	Compact CompactLifecycleSpec `yaml:"compact,omitempty"`
	Archive ArchiveLifecycleSpec `yaml:"archive,omitempty"`
}

// CompactLifecycleSpec schedules compaction and defines its retention window.
type CompactLifecycleSpec struct {
	Cron      string        `yaml:"cron,omitempty"`
	OlderThan time.Duration `yaml:"older_than,omitempty"`
	Keep      int           `yaml:"keep,omitempty"`
}

// ArchiveLifecycleSpec schedules archival and defines its retention window.
type ArchiveLifecycleSpec struct {
	Cron        string        `yaml:"cron,omitempty"`
	OlderThan   time.Duration `yaml:"older_than,omitempty"`
	Destination string        `yaml:"destination,omitempty"`
}

// Validate enforces the documented boundary. A Document is
// rejected when:
//
//   - version is missing or not "v1";
//   - runtime.hard_partition is missing or not exactly
//     [runtime_id, user_id];
//   - default_scope.runtime_id is empty;
//   - clock.impl is missing or unknown;
//   - stores.<name>.impl is missing;
//   - embedding is partially configured or contains invalid values;
//   - a lifecycle block is partially configured or contains invalid values.
//
// Unknown top-level keys are caught earlier by the YAML
// decoder's KnownFields strictness.
func (d Document) Validate() error {
	if d.Version == "" {
		return errors.New("memory config: version is required")
	}
	if d.Version != VersionV1 {
		return fmt.Errorf("memory config: unsupported version %q (want %q)",
			d.Version, VersionV1)
	}
	if err := d.Runtime.Validate(); err != nil {
		return err
	}
	for name, entry := range d.Stores {
		switch name {
		case StoreMessages, StoreDocuments:
		default:
			return fmt.Errorf(
				"memory config: stores[%q] is not a supported slot (want messages or documents)",
				name)
		}
		if err := entry.validate(name); err != nil {
			return err
		}
	}
	if err := d.Embedding.Validate(); err != nil {
		return err
	}
	if err := d.Lifecycle.Validate(); err != nil {
		return err
	}
	return nil
}

// Validate enforces lifecycle block completeness and retention constraints.
func (l LifecycleSpec) Validate() error {
	compactEnabled := l.Compact.Cron != "" || l.Compact.OlderThan != 0 || l.Compact.Keep != 0
	if compactEnabled {
		if strings.TrimSpace(l.Compact.Cron) == "" {
			return errors.New("memory config: lifecycle.compact.cron is required")
		}
		if l.Compact.OlderThan <= 0 {
			return errors.New("memory config: lifecycle.compact.older_than must be greater than zero")
		}
		if l.Compact.Keep < 0 {
			return errors.New("memory config: lifecycle.compact.keep must not be negative")
		}
	}

	archiveEnabled := l.Archive.Cron != "" ||
		l.Archive.OlderThan != 0 || l.Archive.Destination != ""
	if archiveEnabled {
		if strings.TrimSpace(l.Archive.Cron) == "" {
			return errors.New("memory config: lifecycle.archive.cron is required")
		}
		if l.Archive.OlderThan <= 0 {
			return errors.New("memory config: lifecycle.archive.older_than must be greater than zero")
		}
		if strings.TrimSpace(l.Archive.Destination) == "" {
			return errors.New("memory config: lifecycle.archive.destination is required")
		}
	}
	return nil
}

// Validate enforces the RuntimeSpec invariants.
func (r RuntimeSpec) Validate() error {
	if len(r.HardPartition) != 2 ||
		r.HardPartition[0] != "runtime_id" ||
		r.HardPartition[1] != "user_id" {
		return fmt.Errorf(
			"memory config: runtime.hard_partition must be exactly [runtime_id, user_id] (got %v)",
			r.HardPartition)
	}
	if r.DefaultScope.RuntimeID == "" {
		return errors.New("memory config: runtime.default_scope.runtime_id is required")
	}
	if r.Clock.Impl == "" {
		return errors.New("memory config: runtime.clock.impl is required")
	}
	if !identifierPattern.MatchString(r.Clock.Impl) {
		return fmt.Errorf("memory config: runtime.clock.impl %q is not a valid identifier",
			r.Clock.Impl)
	}
	return nil
}

// validate enforces the StoreEntry invariants for one named
// slot.
func (s StoreEntry) validate(name string) error {
	if !identifierPattern.MatchString(name) {
		return fmt.Errorf("memory config: stores[%q] is not a valid identifier", name)
	}
	if s.Impl == "" {
		return fmt.Errorf("memory config: stores[%q].impl is required", name)
	}
	if !identifierPattern.MatchString(s.Impl) {
		return fmt.Errorf("memory config: stores[%q].impl %q is not a valid identifier",
			name, s.Impl)
	}
	return nil
}
