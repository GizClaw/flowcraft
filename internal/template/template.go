// Package template provides the graph template system for creating
// predefined workflow topologies.
package template

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/internal/model"
)

// SelectOption is a key-value option for template parameters.
type SelectOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// TemplateParameter describes a configurable parameter in a graph template.
type TemplateParameter struct {
	Name         string         `json:"name"`
	Label        string         `json:"label"`
	Type         string         `json:"type"`
	DefaultValue any            `json:"default_value,omitempty"`
	Required     bool           `json:"required,omitempty"`
	Options      []SelectOption `json:"options,omitempty"`
	Placeholder  string         `json:"placeholder,omitempty"`
}

// GraphTemplate is a reusable graph topology blueprint.
type GraphTemplate struct {
	Name        string              `json:"name"`
	Label       string              `json:"label"`
	Description string              `json:"description"`
	Category    string              `json:"category"`
	Parameters  []TemplateParameter `json:"parameters,omitempty"`
	GraphDef    any                 `json:"graph_def"`
	IsBuiltin   bool                `json:"is_builtin,omitempty"`
}

// templateStore is the subset of model.Store used by the template registry.
type templateStore interface {
	ListTemplates(ctx context.Context) ([]*model.Template, error)
	SaveTemplate(ctx context.Context, t *model.Template) error
	DeleteTemplate(ctx context.Context, name string) error
}

// Registry manages graph templates with an in-memory cache backed by
// optional SQLite persistence via model.Store.
type Registry struct {
	mu        sync.RWMutex
	templates map[string]GraphTemplate
	store     templateStore
}

// NewRegistry creates a new template registry.
func NewRegistry() *Registry {
	return &Registry{templates: make(map[string]GraphTemplate)}
}

// SetStore attaches a persistent store. When set, user-created templates
// are saved to and loaded from the database. Accepts model.Store or any
// implementation of the ListTemplates/SaveTemplate/DeleteTemplate subset.
func (r *Registry) SetStore(s templateStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = s
}

// Register adds a template to the in-memory cache.
func (r *Registry) Register(t GraphTemplate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.templates[t.Name] = t
}

// Save adds a user template to both the in-memory cache and the persistent store.
func (r *Registry) Save(ctx context.Context, t GraphTemplate) error {
	r.mu.Lock()
	r.templates[t.Name] = t
	s := r.store
	r.mu.Unlock()

	if s != nil {
		mt, err := toModel(t)
		if err != nil {
			return err
		}
		return s.SaveTemplate(ctx, mt)
	}
	return nil
}

// Delete removes a user template from the cache and the store.
func (r *Registry) Delete(ctx context.Context, name string) error {
	r.mu.Lock()
	t, ok := r.templates[name]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("template %q not found", name)
	}
	if t.IsBuiltin {
		r.mu.Unlock()
		return fmt.Errorf("cannot delete built-in template %q", name)
	}
	delete(r.templates, name)
	s := r.store
	r.mu.Unlock()

	if s != nil {
		return s.DeleteTemplate(ctx, name)
	}
	return nil
}

// Get returns a template by name.
func (r *Registry) Get(name string) (GraphTemplate, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.templates[name]
	return t, ok
}

// All returns all registered templates.
func (r *Registry) All() []GraphTemplate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]GraphTemplate, 0, len(r.templates))
	for _, t := range r.templates {
		out = append(out, t)
	}
	return out
}

// RegisterBuiltins registers all built-in graph templates.
func (r *Registry) RegisterBuiltins() {
	for _, t := range builtinTemplates {
		t.IsBuiltin = true
		r.Register(t)
	}
}

// LoadFromStore loads user-created templates from the database and syncs
// built-in templates to the store. Should be called once at startup
// after SetStore and RegisterBuiltins.
func (r *Registry) LoadFromStore(ctx context.Context) error {
	r.mu.RLock()
	s := r.store
	r.mu.RUnlock()
	if s == nil {
		return nil
	}

	// Sync built-in templates to the store so they're visible in queries.
	r.mu.RLock()
	builtins := make([]GraphTemplate, 0)
	for _, t := range r.templates {
		if t.IsBuiltin {
			builtins = append(builtins, t)
		}
	}
	r.mu.RUnlock()
	for _, t := range builtins {
		mt, err := toModel(t)
		if err != nil {
			continue
		}
		mt.IsBuiltin = true
		_ = s.SaveTemplate(ctx, mt)
	}

	// Load user templates from the store into the in-memory cache.
	stored, err := s.ListTemplates(ctx)
	if err != nil {
		return fmt.Errorf("template: load from store: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, mt := range stored {
		if mt.IsBuiltin {
			continue
		}
		gt, err := fromModel(mt)
		if err != nil {
			continue
		}
		r.templates[gt.Name] = gt
	}
	return nil
}

func toModel(t GraphTemplate) (*model.Template, error) {
	params, err := json.Marshal(t.Parameters)
	if err != nil {
		return nil, fmt.Errorf("template: marshal parameters: %w", err)
	}
	graphDef, err := json.Marshal(t.GraphDef)
	if err != nil {
		return nil, fmt.Errorf("template: marshal graph_def: %w", err)
	}
	return &model.Template{
		Name:        t.Name,
		Label:       t.Label,
		Description: t.Description,
		Category:    t.Category,
		Parameters:  string(params),
		GraphDef:    string(graphDef),
		IsBuiltin:   t.IsBuiltin,
	}, nil
}

func fromModel(mt *model.Template) (GraphTemplate, error) {
	var params []TemplateParameter
	if mt.Parameters != "" && mt.Parameters != "[]" {
		if err := json.Unmarshal([]byte(mt.Parameters), &params); err != nil {
			return GraphTemplate{}, fmt.Errorf("template: unmarshal parameters: %w", err)
		}
	}
	var graphDef any
	if err := json.Unmarshal([]byte(mt.GraphDef), &graphDef); err != nil {
		return GraphTemplate{}, fmt.Errorf("template: unmarshal graph_def: %w", err)
	}
	return GraphTemplate{
		Name:        mt.Name,
		Label:       mt.Label,
		Description: mt.Description,
		Category:    mt.Category,
		Parameters:  params,
		GraphDef:    graphDef,
		IsBuiltin:   mt.IsBuiltin,
	}, nil
}

// Instantiate creates a concrete graph definition from a template and parameters.
// It serializes the GraphDef to JSON, replaces {{.param_name}} placeholders,
// and returns the result as a map.
func Instantiate(t GraphTemplate, params map[string]any) (map[string]any, error) {
	if params == nil {
		params = make(map[string]any)
	}
	for _, p := range t.Parameters {
		if p.Required {
			if _, ok := params[p.Name]; !ok {
				if p.DefaultValue != nil {
					params[p.Name] = p.DefaultValue
				} else {
					return nil, fmt.Errorf("template %q: missing required parameter %q", t.Name, p.Name)
				}
			}
		}
	}

	for _, p := range t.Parameters {
		if _, ok := params[p.Name]; !ok && p.DefaultValue != nil {
			params[p.Name] = p.DefaultValue
		}
	}

	data, err := json.Marshal(t.GraphDef)
	if err != nil {
		return nil, fmt.Errorf("template %q: marshal graph_def: %w", t.Name, err)
	}

	s := string(data)
	for k, v := range params {
		placeholder := "{{." + k + "}}"
		s = strings.ReplaceAll(s, placeholder, fmt.Sprint(v))
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		return nil, fmt.Errorf("template %q: unmarshal after substitution: %w", t.Name, err)
	}

	return result, nil
}
