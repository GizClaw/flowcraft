package graph

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// FallbackHandler handles nodes whose type name is not registered.
// It receives the type name and the raw config, letting a host keep
// unknown graphs runnable (proxying, stubbing, remote execution)
// instead of failing at [Build].
type FallbackHandler func(ctx ExecutionContext, board *agent.Board, typeName string, raw json.RawMessage) error

// Registry maps node type names to their behaviour.
//
// It is plain in-memory wiring — no global state, no side effects.
// Registrations happen at assembly time; a Registry is safe for
// concurrent reads once handed to [Build].
type Registry struct {
	types    map[string]*erasedType
	fallback FallbackHandler
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{types: make(map[string]*erasedType)}
}

// RegisterType binds typeName to nt.
//
// The node type's meta is validated eagerly: role kinds must be known,
// each role must set exactly one of Name/ConfigKey, and a ConfigKey
// must name a real field of the config struct C.
func RegisterType[C any](r *Registry, typeName string, nt NodeType[C]) error {
	if typeName == "" {
		return errdefs.Validationf("graph: node type name is required")
	}
	if nt.Handler == nil {
		return errdefs.Validationf("graph: node type %q: handler is required", typeName)
	}
	if _, dup := r.types[typeName]; dup {
		return errdefs.Conflictf("graph: node type %q already registered", typeName)
	}
	decode := nt.Decode
	if decode == nil {
		decode = DecodeConfig[C]
	}
	et := &erasedType{
		meta:         nt.Meta,
		configFields: configFieldNames[C](),
		decode:       func(raw json.RawMessage) (any, error) { return decode(raw) },
		invoke: func(ec ExecutionContext, b *agent.Board, cfg any) error {
			return nt.Handler(ec, b, cfg.(C))
		},
	}
	if err := et.validate(typeName); err != nil {
		return err
	}
	r.types[typeName] = et
	return nil
}

// RegisterFallback installs the handler for unregistered node types.
// Without a fallback, [Build] rejects definitions referencing unknown
// types.
func (r *Registry) RegisterFallback(fn FallbackHandler) {
	r.fallback = fn
}

// Has reports whether typeName is registered.
func (r *Registry) Has(typeName string) bool {
	_, ok := r.types[typeName]
	return ok
}

// MetaOf returns the static descriptor of a registered type.
func (r *Registry) MetaOf(typeName string) (Meta, bool) {
	et, ok := r.types[typeName]
	if !ok {
		return Meta{}, false
	}
	return et.meta, true
}

// TypeNames returns the registered type names, sorted.
func (r *Registry) TypeNames() []string {
	out := make([]string, 0, len(r.types))
	for name := range r.types {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// erasedType is the type-erased internal form of a [NodeType]: the
// generics exist only at registration; the kernel invokes everything
// through this uniform shape.
type erasedType struct {
	meta Meta
	// configFields is the set of known JSON field names of the config
	// struct, or nil for non-struct configs (field checks disabled).
	configFields map[string]bool
	decode       func(json.RawMessage) (any, error)
	invoke       func(ExecutionContext, *agent.Board, any) error
}

// validate checks the registered meta for internal consistency.
func (et *erasedType) validate(typeName string) error {
	for _, roles := range [][]Role{et.meta.Reads, et.meta.Writes} {
		for _, role := range roles {
			switch role.Kind {
			case RoleVar, RoleMessages:
			default:
				return errdefs.Validationf(
					"graph: node type %q: unknown role kind %q", typeName, role.Kind,
				)
			}
			if (role.Name == "") == (role.ConfigKey == "") {
				return errdefs.Validationf(
					"graph: node type %q: role must set exactly one of Name/ConfigKey", typeName,
				)
			}
			if role.ConfigKey != "" && et.configFields != nil && !et.configFields[role.ConfigKey] {
				return errdefs.Validationf(
					"graph: node type %q: role ConfigKey %q is not a field of the config struct",
					typeName, role.ConfigKey,
				)
			}
		}
	}
	return nil
}

// configFieldNames returns the JSON field names of C when C is a
// struct — used for Build-time unknown-field rejection and ConfigKey
// validation. It returns nil for non-struct configs, disabling both
// checks.
func configFieldNames[C any]() map[string]bool {
	t := reflect.TypeFor[C]()
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	names := make(map[string]bool)
	collectJSONFieldNames(t, names)
	return names
}

func collectJSONFieldNames(t reflect.Type, out map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue // unexported
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-":
			continue
		case name == "" && f.Anonymous:
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectJSONFieldNames(ft, out)
			}
		case name == "":
			out[f.Name] = true
		default:
			out[name] = true
		}
	}
}
