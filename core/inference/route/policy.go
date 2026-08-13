package route

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
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
	Quality     *float64 `json:"quality,omitempty"`
	Economy     *float64 `json:"economy,omitempty"`
	Speed       *float64 `json:"speed,omitempty"`
	Reliability *float64 `json:"reliability,omitempty"`
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
	Model inference.ModelRef `json:"model"`
	Score ModelScore         `json:"score,omitzero"`
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
	Tier    Tier     `json:"tier"`
	Targets []Target `json:"targets"`
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
	Generate       []Pool                `json:"generate,omitempty"`
	Embed          []Pool                `json:"embed,omitempty"`
	Retry          *RetryConfig          `json:"retry,omitempty"`
	CircuitBreaker *CircuitBreakerConfig `json:"circuit_breaker,omitempty"`
}

func (p Policy) Clone() Policy {
	cloned := Policy{
		Generate: clonePools(p.Generate),
		Embed:    clonePools(p.Embed),
	}
	if p.Retry != nil {
		retry := p.Retry.Clone()
		cloned.Retry = &retry
	}
	if p.CircuitBreaker != nil {
		breaker := *p.CircuitBreaker
		cloned.CircuitBreaker = &breaker
	}
	return cloned
}

func (p Policy) Validate() error {
	total := len(p.Generate) + len(p.Embed)
	if total == 0 {
		return fmt.Errorf("route policy has no pools")
	}
	operations := []struct {
		operation inference.Operation
		pools     []Pool
	}{
		{operation: inference.OperationGenerate, pools: p.Generate},
		{operation: inference.OperationEmbed, pools: p.Embed},
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
	if p.Retry != nil {
		if err := p.Retry.validate(hasPools(p)); err != nil {
			return err
		}
	}
	if p.CircuitBreaker != nil {
		if err := p.CircuitBreaker.validate(); err != nil {
			return fmt.Errorf("circuit breaker: %w", err)
		}
	}
	return nil
}

func hasPools(policy Policy) func(inference.Operation) bool {
	return func(operation inference.Operation) bool {
		switch operation {
		case inference.OperationGenerate:
			return len(policy.Generate) > 0
		case inference.OperationEmbed:
			return len(policy.Embed) > 0
		default:
			return false
		}
	}
}

// RetryConfig is the JSON-only deployment form of RetryPolicies.
type RetryConfig struct {
	Generate *RetryPolicyConfig `json:"generate,omitempty"`
	Embed    *RetryPolicyConfig `json:"embed,omitempty"`
}

// UnmarshalJSON enforces the JSON-only DTO boundary strictly: unknown fields
// inside the retry section are rejected.
func (c *RetryConfig) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	type alias RetryConfig
	var raw alias
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("decode retry config: %w", err)
	}
	*c = RetryConfig(raw)
	return nil
}

func (c RetryConfig) Clone() RetryConfig {
	return RetryConfig{
		Generate: c.Generate.Clone(),
		Embed:    c.Embed.Clone(),
	}
}

func (c RetryConfig) validate(hasPools func(inference.Operation) bool) error {
	entries := []struct {
		operation inference.Operation
		config    *RetryPolicyConfig
	}{
		{inference.OperationGenerate, c.Generate},
		{inference.OperationEmbed, c.Embed},
	}
	for _, entry := range entries {
		if entry.config == nil {
			continue
		}
		if !hasPools(entry.operation) {
			return fmt.Errorf(
				"%s retry policy requires %s pools",
				entry.operation,
				entry.operation,
			)
		}
		if err := entry.config.validate(); err != nil {
			return fmt.Errorf("%s retry policy: %w", entry.operation, err)
		}
	}
	return nil
}

func (c RetryConfig) policies() (RetryPolicies, error) {
	var out RetryPolicies
	var err error
	if out.Generate, err = c.Generate.policy(); err != nil {
		return RetryPolicies{}, fmt.Errorf("generate retry policy: %w", err)
	}
	if out.Embed, err = c.Embed.policy(); err != nil {
		return RetryPolicies{}, fmt.Errorf("embed retry policy: %w", err)
	}
	return out, nil
}

// RetryableClass is one built-in error class a deployment may allow for
// same-target retry.
type RetryableClass string

const (
	RetryableRateLimit   RetryableClass = "rate_limit"
	RetryableTimeout     RetryableClass = "timeout"
	RetryableUnavailable RetryableClass = "unavailable"
)

// RetryPolicyConfig is the JSON-only deployment form of RetryPolicy.
type RetryPolicyConfig struct {
	MaxAttempts              int              `json:"max_attempts,omitempty"`
	MaxTotalAttempts         int              `json:"max_total_attempts,omitempty"`
	Backoff                  *Backoff         `json:"backoff,omitempty"`
	Retryable                []RetryableClass `json:"retryable,omitempty"`
	FallbackOnRetryExhausted bool             `json:"fallback_on_retry_exhausted,omitempty"`
}

// UnmarshalJSON enforces the JSON-only DTO boundary strictly.
func (c *RetryPolicyConfig) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	type alias RetryPolicyConfig
	var raw alias
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("decode retry policy: %w", err)
	}
	*c = RetryPolicyConfig(raw)
	return nil
}

func (c *RetryPolicyConfig) Clone() *RetryPolicyConfig {
	if c == nil {
		return nil
	}
	cloned := *c
	if c.Backoff != nil {
		backoff := *c.Backoff
		cloned.Backoff = &backoff
	}
	cloned.Retryable = append([]RetryableClass(nil), c.Retryable...)
	return &cloned
}

func (c *RetryPolicyConfig) validate() error {
	if c == nil {
		return nil
	}
	if c.MaxAttempts < 0 {
		return fmt.Errorf("max_attempts must not be negative")
	}
	if c.MaxTotalAttempts < 0 {
		return fmt.Errorf("max_total_attempts must not be negative")
	}
	if c.Backoff != nil {
		if err := c.Backoff.validate(); err != nil {
			return fmt.Errorf("backoff: %w", err)
		}
	}
	seen := make(map[RetryableClass]struct{}, len(c.Retryable))
	for _, class := range c.Retryable {
		switch class {
		case RetryableRateLimit, RetryableTimeout, RetryableUnavailable:
		default:
			return fmt.Errorf("unknown retryable class %q", class)
		}
		if _, ok := seen[class]; ok {
			return fmt.Errorf("duplicate retryable class %q", class)
		}
		seen[class] = struct{}{}
	}
	return nil
}

func (c *RetryPolicyConfig) policy() (*RetryPolicy, error) {
	if c == nil {
		return nil, nil
	}
	var retryable func(context.Context, RetryDecision) bool
	if c.Retryable == nil {
		retryable = DefaultRetryable
	} else {
		classes := make(map[RetryableClass]struct{}, len(c.Retryable))
		for _, class := range c.Retryable {
			classes[class] = struct{}{}
		}
		retryable = func(_ context.Context, decision RetryDecision) bool {
			if decision.ObservableOutput ||
				decision.ErrorKind != inference.ProviderFailure {
				return false
			}
			if _, ok := classes[RetryableRateLimit]; ok &&
				errdefs.IsRateLimit(decision.Err) {
				return true
			}
			if _, ok := classes[RetryableTimeout]; ok &&
				errdefs.IsTimeout(decision.Err) {
				return true
			}
			if _, ok := classes[RetryableUnavailable]; ok &&
				errdefs.IsNotAvailable(decision.Err) {
				return true
			}
			return false
		}
	}
	var backoff Backoff
	if c.Backoff != nil {
		backoff = *c.Backoff
	}
	return &RetryPolicy{
		MaxAttempts:              c.MaxAttempts,
		MaxTotalAttempts:         c.MaxTotalAttempts,
		Backoff:                  backoff,
		Retryable:                retryable,
		FallbackOnRetryExhausted: c.FallbackOnRetryExhausted,
	}, nil
}

// Options converts the policy's retry and circuit-breaker sections into
// Router construction options.
func (p Policy) Options() ([]Option, error) {
	var options []Option
	if p.Retry != nil {
		policies, err := p.Retry.policies()
		if err != nil {
			return nil, err
		}
		options = append(options, WithRetryPolicies(policies))
	}
	if p.CircuitBreaker != nil {
		options = append(options, WithCircuitBreaker(*p.CircuitBreaker))
	}
	return options, nil
}

// ValidateFor checks every configured target against an immutable inference
// Runtime without opening drivers or invoking compilers.
func (p Policy) ValidateFor(assembly *inference.Assembly) error {
	if assembly == nil {
		return fmt.Errorf("inference assembly is required")
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
	}
	for _, entry := range operations {
		for _, pool := range entry.pools {
			for _, target := range pool.Targets {
				descriptor, err := assembly.InspectModel(target.Model)
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
