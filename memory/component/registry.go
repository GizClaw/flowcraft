package component

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	// ErrFactoryConflict reports a duplicate name in one capability slot.
	ErrFactoryConflict = errors.New("memory component: factory already registered")
	// ErrFactoryNotFound reports a missing name in one capability slot.
	ErrFactoryNotFound = errors.New("memory component: factory not found")
)

var componentName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Spec is the compatibility name for an unconfigured DeriverSpec. It has no
// arbitrary map configuration path.
type Spec = DeriverSpec

// ValidateName checks the shared factory namespace syntax. Namespaces remain
// separate by capability, so the same valid name may be used in every slot.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("memory component: name is required")
	}
	if name != strings.TrimSpace(name) || len(name) > 128 || !componentName.MatchString(name) {
		return fmt.Errorf("memory component: invalid name %q", name)
	}
	return nil
}

type (
	// DeriverFactory constructs a Deriver from an owned Spec.
	DeriverFactory func(Spec) (Deriver, error)
	// IndexerFactory constructs an Indexer from an owned Spec.
	IndexerFactory func(Spec) (Indexer, error)
	// SearcherFactory constructs a Searcher from an owned Spec.
	SearcherFactory func(Spec) (Searcher, error)
	// PackerFactory constructs a Packer from an owned Spec.
	PackerFactory func(Spec) (Packer, error)
)

// Ports declares the artifact contract of a typed derivation factory.
type Ports struct {
	Inputs  []ArtifactKind `json:"inputs"`
	Outputs []ArtifactKind `json:"outputs"`
}

// DeriverSpec is an opaque, typed factory selection. Construct it with
// NewDeriverSpec so callers cannot supply an untyped configuration map.
type DeriverSpec struct {
	Name       string
	config     any
	configType reflect.Type
}

// NewDeriverSpec binds a concrete Go configuration type to a factory name.
func NewDeriverSpec[C any](name string, config C) DeriverSpec {
	return DeriverSpec{Name: name, config: config, configType: reflect.TypeFor[C]()}
}

// NewUnconfiguredDeriverSpec selects a legacy, configuration-free factory.
func NewUnconfiguredDeriverSpec(name string) DeriverSpec {
	return DeriverSpec{Name: name}
}

// FactoryName returns the selected factory name.
func (spec DeriverSpec) FactoryName() string { return spec.Name }

// CanonicalConfig returns stable JSON for policy hashing.
func (spec DeriverSpec) CanonicalConfig() ([]byte, error) {
	if spec.configType == nil {
		return []byte("null"), nil
	}
	data, err := json.Marshal(spec.config)
	if err != nil {
		return nil, fmt.Errorf("memory component: config for %q: %w", spec.Name, err)
	}
	return data, nil
}

// FactoryMetadata describes one registered typed algorithm.
type FactoryMetadata struct {
	Name    string
	Version string
	Ports   Ports
}

type typedDeriverFactory struct {
	metadata   FactoryMetadata
	configType reflect.Type
	build      func(any) (Deriver, error)
}

// Registry stores factories in four independent, concurrently safe slots.
type Registry struct {
	mu        sync.RWMutex
	derivers  map[string]DeriverFactory
	typed     map[string]typedDeriverFactory
	indexers  map[string]IndexerFactory
	searchers map[string]SearcherFactory
	packers   map[string]PackerFactory
}

// NewRegistry returns an empty component registry.
func NewRegistry() *Registry {
	return &Registry{
		derivers:  make(map[string]DeriverFactory),
		typed:     make(map[string]typedDeriverFactory),
		indexers:  make(map[string]IndexerFactory),
		searchers: make(map[string]SearcherFactory),
		packers:   make(map[string]PackerFactory),
	}
}

// Clone returns an independently mutable catalog containing the same factory
// functions and immutable metadata.
func (registry *Registry) Clone() *Registry {
	if registry == nil {
		return NewRegistry()
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	cloned := NewRegistry()
	for name, factory := range registry.derivers {
		cloned.derivers[name] = factory
	}
	for name, factory := range registry.typed {
		factory.metadata.Ports = clonePorts(factory.metadata.Ports)
		cloned.typed[name] = factory
	}
	for name, factory := range registry.indexers {
		cloned.indexers[name] = factory
	}
	for name, factory := range registry.searchers {
		cloned.searchers[name] = factory
	}
	for name, factory := range registry.packers {
		cloned.packers[name] = factory
	}
	return cloned
}

// TypedDeriverMetadata returns typed algorithms sorted by name.
func (registry *Registry) TypedDeriverMetadata() []FactoryMetadata {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	result := make([]FactoryMetadata, 0, len(registry.typed))
	for _, factory := range registry.typed {
		metadata := factory.metadata
		metadata.Ports = clonePorts(metadata.Ports)
		result = append(result, metadata)
	}
	registry.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// RegisterDeriver registers one name in the Deriver slot.
func (registry *Registry) RegisterDeriver(name string, factory DeriverFactory) error {
	if registry == nil {
		return errors.New("memory component: registry is required")
	}
	if factory == nil {
		return errors.New("memory component: deriver factory is required")
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.derivers[name]; exists {
		return fmt.Errorf("%w: %q", ErrFactoryConflict, name)
	}
	if _, exists := registry.typed[name]; exists {
		return fmt.Errorf("%w: %q", ErrFactoryConflict, name)
	}
	registry.derivers[name] = factory
	return nil
}

// RegisterTypedDeriver registers a factory whose configuration type and
// artifact ports are validated before a DAG can be built.
func RegisterTypedDeriver[C any](registry *Registry, name, version string, ports Ports, factory func(C) (Deriver, error)) error {
	if registry == nil {
		return errors.New("memory component: registry is required")
	}
	if factory == nil {
		return errors.New("memory component: typed deriver factory is required")
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	if strings.TrimSpace(version) == "" {
		return errors.New("memory component: algorithm version is required")
	}
	if len(ports.Inputs) == 0 || len(ports.Outputs) == 0 {
		return errors.New("memory component: typed deriver input and output ports are required")
	}
	configType := reflect.TypeFor[C]()
	if configType.Kind() != reflect.Struct {
		return fmt.Errorf("memory component: typed deriver config must be a concrete struct, got %v", configType)
	}
	entry := typedDeriverFactory{
		metadata:   FactoryMetadata{Name: name, Version: version, Ports: clonePorts(ports)},
		configType: configType,
		build: func(value any) (Deriver, error) {
			return factory(value.(C))
		},
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.derivers[name]; exists {
		return fmt.Errorf("%w: %q", ErrFactoryConflict, name)
	}
	if _, exists := registry.typed[name]; exists {
		return fmt.Errorf("%w: %q", ErrFactoryConflict, name)
	}
	registry.typed[name] = entry
	return nil
}

// RegisterIndexer registers one name in the Indexer slot.
func (registry *Registry) RegisterIndexer(name string, factory IndexerFactory) error {
	if registry == nil {
		return errors.New("memory component: registry is required")
	}
	if factory == nil {
		return errors.New("memory component: indexer factory is required")
	}
	return register(registry, name, registry.indexers, factory)
}

// RegisterSearcher registers one name in the Searcher slot.
func (registry *Registry) RegisterSearcher(name string, factory SearcherFactory) error {
	if registry == nil {
		return errors.New("memory component: registry is required")
	}
	if factory == nil {
		return errors.New("memory component: searcher factory is required")
	}
	return register(registry, name, registry.searchers, factory)
}

// RegisterPacker registers one name in the Packer slot.
func (registry *Registry) RegisterPacker(name string, factory PackerFactory) error {
	if registry == nil {
		return errors.New("memory component: registry is required")
	}
	if factory == nil {
		return errors.New("memory component: packer factory is required")
	}
	return register(registry, name, registry.packers, factory)
}

func register[T any](registry *Registry, name string, slot map[string]T, factory T) error {
	if registry == nil {
		return errors.New("memory component: registry is required")
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := slot[name]; exists {
		return fmt.Errorf("%w: %q", ErrFactoryConflict, name)
	}
	slot[name] = factory
	return nil
}

// ResolveDeriver constructs a Deriver using an independently owned Spec.
func (registry *Registry) ResolveDeriver(spec Spec) (Deriver, error) {
	if registry == nil {
		return nil, errors.New("memory component: registry is required")
	}
	factory, err := resolve(registry, spec.Name, registry.derivers)
	if err != nil {
		return nil, fmt.Errorf("memory component: resolve deriver: %w", err)
	}
	value, err := factory(spec)
	if err != nil {
		return nil, fmt.Errorf("memory component: create deriver %q: %w", spec.Name, err)
	}
	if value == nil {
		return nil, fmt.Errorf("memory component: create deriver %q: factory returned nil", spec.Name)
	}
	return value, nil
}

// ResolveTypedDeriver validates the exact config type and constructs a typed
// deriver. All errors are returned during DAG build, before execution starts.
func (registry *Registry) ResolveTypedDeriver(spec DeriverSpec) (Deriver, FactoryMetadata, error) {
	if registry == nil {
		return nil, FactoryMetadata{}, errors.New("memory component: registry is required")
	}
	if err := ValidateName(spec.Name); err != nil {
		return nil, FactoryMetadata{}, err
	}
	registry.mu.RLock()
	entry, exists := registry.typed[spec.Name]
	registry.mu.RUnlock()
	if !exists {
		return nil, FactoryMetadata{}, fmt.Errorf("memory component: resolve typed deriver: %w: %q", ErrFactoryNotFound, spec.Name)
	}
	if spec.configType == nil && entry.configType == reflect.TypeFor[struct{}]() {
		spec.config = struct{}{}
		spec.configType = entry.configType
	}
	if spec.configType != entry.configType {
		return nil, FactoryMetadata{}, fmt.Errorf(
			"memory component: create deriver %q: config type %v does not match registered %v",
			spec.Name, spec.configType, entry.configType,
		)
	}
	config, err := cloneTypedConfig(spec.config, entry.configType)
	if err != nil {
		return nil, FactoryMetadata{}, fmt.Errorf("memory component: create deriver %q: %w", spec.Name, err)
	}
	value, err := entry.build(config)
	if err != nil {
		return nil, FactoryMetadata{}, fmt.Errorf("memory component: create deriver %q: %w", spec.Name, err)
	}
	if value == nil {
		return nil, FactoryMetadata{}, fmt.Errorf("memory component: create deriver %q: factory returned nil", spec.Name)
	}
	return value, FactoryMetadata{
		Name: entry.metadata.Name, Version: entry.metadata.Version, Ports: clonePorts(entry.metadata.Ports),
	}, nil
}

func cloneTypedConfig(value any, typ reflect.Type) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode typed config: %w", err)
	}
	target := reflect.New(typ)
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target.Interface()); err != nil {
		return nil, fmt.Errorf("clone typed config: %w", err)
	}
	return target.Elem().Interface(), nil
}

func clonePorts(ports Ports) Ports {
	return Ports{
		Inputs:  append([]ArtifactKind(nil), ports.Inputs...),
		Outputs: append([]ArtifactKind(nil), ports.Outputs...),
	}
}

// ResolveIndexer constructs an Indexer using an independently owned Spec.
func (registry *Registry) ResolveIndexer(spec Spec) (Indexer, error) {
	if registry == nil {
		return nil, errors.New("memory component: registry is required")
	}
	factory, err := resolve(registry, spec.Name, registry.indexers)
	if err != nil {
		return nil, fmt.Errorf("memory component: resolve indexer: %w", err)
	}
	value, err := factory(spec)
	if err != nil {
		return nil, fmt.Errorf("memory component: create indexer %q: %w", spec.Name, err)
	}
	if value == nil {
		return nil, fmt.Errorf("memory component: create indexer %q: factory returned nil", spec.Name)
	}
	return value, nil
}

// ResolveSearcher constructs a Searcher using an independently owned Spec.
func (registry *Registry) ResolveSearcher(spec Spec) (Searcher, error) {
	if registry == nil {
		return nil, errors.New("memory component: registry is required")
	}
	factory, err := resolve(registry, spec.Name, registry.searchers)
	if err != nil {
		return nil, fmt.Errorf("memory component: resolve searcher: %w", err)
	}
	value, err := factory(spec)
	if err != nil {
		return nil, fmt.Errorf("memory component: create searcher %q: %w", spec.Name, err)
	}
	if value == nil {
		return nil, fmt.Errorf("memory component: create searcher %q: factory returned nil", spec.Name)
	}
	return value, nil
}

// ResolvePacker constructs a Packer using an independently owned Spec.
func (registry *Registry) ResolvePacker(spec Spec) (Packer, error) {
	if registry == nil {
		return nil, errors.New("memory component: registry is required")
	}
	factory, err := resolve(registry, spec.Name, registry.packers)
	if err != nil {
		return nil, fmt.Errorf("memory component: resolve packer: %w", err)
	}
	value, err := factory(spec)
	if err != nil {
		return nil, fmt.Errorf("memory component: create packer %q: %w", spec.Name, err)
	}
	if value == nil {
		return nil, fmt.Errorf("memory component: create packer %q: factory returned nil", spec.Name)
	}
	return value, nil
}

func resolve[T any](registry *Registry, name string, slot map[string]T) (T, error) {
	var zero T
	if registry == nil {
		return zero, errors.New("memory component: registry is required")
	}
	if err := ValidateName(name); err != nil {
		return zero, err
	}
	registry.mu.RLock()
	factory, exists := slot[name]
	registry.mu.RUnlock()
	if !exists {
		return zero, fmt.Errorf("%w: %q", ErrFactoryNotFound, name)
	}
	return factory, nil
}
