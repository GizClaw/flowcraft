package route

import (
	"fmt"
	"math"
	"regexp"
	"slices"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

var tierPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Tier is a deployment-defined routing class. The route package assigns no
// built-in ordering or quality meaning to tier names.
type Tier string

func (t Tier) Validate() error {
	if !tierPattern.MatchString(string(t)) {
		return fmt.Errorf("invalid route tier %q", t)
	}
	return nil
}

// ModelScore contains optional normalized routing signals. Every populated
// value is in [0,1], and higher is better. Scores guide selection only; they
// never claim that a request is executable.
type ModelScore struct {
	Quality     *float64 `json:"quality,omitempty" yaml:"quality,omitempty"`
	Economy     *float64 `json:"economy,omitempty" yaml:"economy,omitempty"`
	Speed       *float64 `json:"speed,omitempty" yaml:"speed,omitempty"`
	Reliability *float64 `json:"reliability,omitempty" yaml:"reliability,omitempty"`
}

// IsZero reports whether no signal is populated. It drives both JSON
// omitzero and YAML omitempty so scoreless targets stay compact.
func (s ModelScore) IsZero() bool {
	return s.Quality == nil && s.Economy == nil &&
		s.Speed == nil && s.Reliability == nil
}

func (s ModelScore) Clone() ModelScore {
	return ModelScore{
		Quality:     clonePointer(s.Quality),
		Economy:     clonePointer(s.Economy),
		Speed:       clonePointer(s.Speed),
		Reliability: clonePointer(s.Reliability),
	}
}

func (s ModelScore) Validate() error {
	values := []struct {
		name  string
		value *float64
	}{
		{name: "quality", value: s.Quality},
		{name: "economy", value: s.Economy},
		{name: "speed", value: s.Speed},
		{name: "reliability", value: s.Reliability},
	}
	for _, item := range values {
		if item.value == nil {
			continue
		}
		if math.IsNaN(*item.value) || math.IsInf(*item.value, 0) ||
			*item.value < 0 || *item.value > 1 {
			return fmt.Errorf("model %s score must be between 0 and 1", item.name)
		}
	}
	return nil
}

// Target is one exact executable model candidate plus route-only signals.
type Target struct {
	Model inference.ModelRef `json:"model" yaml:"model"`
	Score ModelScore         `json:"score,omitzero" yaml:"score,omitempty"`
}

func (t Target) Clone() Target {
	t.Score = t.Score.Clone()
	return t
}

func (t Target) Validate() error {
	if err := t.Model.Validate(); err != nil {
		return err
	}
	return t.Score.Validate()
}

// Pool is the allowlist of exact model targets available to one tier.
type Pool struct {
	Tier    Tier     `json:"tier" yaml:"tier"`
	Targets []Target `json:"targets" yaml:"targets"`
}

func (p Pool) Clone() Pool {
	clone := p
	clone.Targets = make([]Target, len(p.Targets))
	for index, target := range p.Targets {
		clone.Targets[index] = target.Clone()
	}
	return clone
}

func (p Pool) Validate() error {
	if err := p.Tier.Validate(); err != nil {
		return err
	}
	if len(p.Targets) == 0 {
		return fmt.Errorf("route tier %q has no targets", p.Tier)
	}
	seen := make(map[inference.ModelRef]struct{}, len(p.Targets))
	for index, target := range p.Targets {
		if err := target.Validate(); err != nil {
			return fmt.Errorf("route tier %q target %d: %w", p.Tier, index, err)
		}
		if _, ok := seen[target.Model]; ok {
			return fmt.Errorf("route tier %q has duplicate target %+v", p.Tier, target.Model)
		}
		seen[target.Model] = struct{}{}
	}
	return nil
}

// Policy owns operation-specific tier allowlists. It is descriptive route
// configuration, not an inference capability catalog.
type Policy struct {
	Generate      []Pool `json:"generate,omitempty" yaml:"generate,omitempty"`
	Embed         []Pool `json:"embed,omitempty" yaml:"embed,omitempty"`
	Transcription []Pool `json:"transcription,omitempty" yaml:"transcription,omitempty"`
	Realtime      []Pool `json:"realtime,omitempty" yaml:"realtime,omitempty"`
}

func (p Policy) Clone() Policy {
	return Policy{
		Generate:      clonePools(p.Generate),
		Embed:         clonePools(p.Embed),
		Transcription: clonePools(p.Transcription),
		Realtime:      clonePools(p.Realtime),
	}
}

func (p Policy) Validate() error {
	total := len(p.Generate) + len(p.Embed) + len(p.Transcription) + len(p.Realtime)
	if total == 0 {
		return fmt.Errorf("route policy has no pools")
	}
	operations := []struct {
		operation inference.Operation
		pools     []Pool
	}{
		{operation: inference.OperationGenerate, pools: p.Generate},
		{operation: inference.OperationEmbed, pools: p.Embed},
		{operation: inference.OperationTranscription, pools: p.Transcription},
		{operation: inference.OperationRealtime, pools: p.Realtime},
	}
	for _, entry := range operations {
		seen := make(map[Tier]struct{}, len(entry.pools))
		for index, pool := range entry.pools {
			if err := pool.Validate(); err != nil {
				return fmt.Errorf("%s pool %d: %w", entry.operation, index, err)
			}
			if _, ok := seen[pool.Tier]; ok {
				return fmt.Errorf(
					"%s route policy has duplicate tier %q",
					entry.operation,
					pool.Tier,
				)
			}
			seen[pool.Tier] = struct{}{}
		}
	}
	return nil
}

// ValidateFor checks every configured target against an immutable inference
// Runtime without opening drivers or invoking compilers.
func (p Policy) ValidateFor(runtime *inference.Runtime) error {
	if runtime == nil {
		return fmt.Errorf("inference runtime is required")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	operations := []struct {
		operation inference.Operation
		pools     []Pool
	}{
		{operation: inference.OperationGenerate, pools: p.Generate},
		{operation: inference.OperationEmbed, pools: p.Embed},
		{operation: inference.OperationTranscription, pools: p.Transcription},
		{operation: inference.OperationRealtime, pools: p.Realtime},
	}
	for _, entry := range operations {
		for _, pool := range entry.pools {
			for _, target := range pool.Targets {
				descriptor, err := runtime.InspectModel(target.Model)
				if err != nil {
					return fmt.Errorf(
						"%s tier %q target %+v: %w",
						entry.operation,
						pool.Tier,
						target.Model,
						err,
					)
				}
				if descriptor.Lifecycle.Status == inference.ModelStatusRetired {
					return fmt.Errorf(
						"%s tier %q target %q is retired",
						entry.operation,
						pool.Tier,
						target.Model.ID.Name,
					)
				}
				if !supportsOperation(descriptor, entry.operation) {
					return fmt.Errorf(
						"%s tier %q target %q does not expose the operation",
						entry.operation,
						pool.Tier,
						target.Model.ID.Name,
					)
				}
			}
		}
	}
	return nil
}

func supportsOperation(descriptor inference.ModelDescriptor, operation inference.Operation) bool {
	return slices.Contains(descriptor.Operations, operation)
}

func clonePools(pools []Pool) []Pool {
	if pools == nil {
		return nil
	}
	clone := make([]Pool, len(pools))
	for index, pool := range pools {
		clone[index] = pool.Clone()
	}
	return clone
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
