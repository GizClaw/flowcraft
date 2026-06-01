package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/claw"
	"gopkg.in/yaml.v3"
)

const APIVersion = "claw.flowcraft.io/v1alpha1"

type envelope struct {
	APIVersion string         `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`
	Kind       string         `json:"kind,omitempty" yaml:"kind,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Spec       yaml.Node      `json:"spec,omitempty" yaml:"spec,omitempty"`
}

// Load reads command-owned config files and expands environment variables.
func Load(ctx context.Context, ws sdkworkspace.Workspace, root string) (claw.Config, error) {
	cfg := claw.DefaultConfig()
	if ws == nil {
		ExpandEnv(&cfg)
		return cfg, nil
	}
	root = cleanRoot(root)
	if ok, err := mergeConfigFile(ctx, ws, join(root, "claw.yaml"), &cfg); err != nil {
		return claw.Config{}, err
	} else if ok {
		ExpandEnv(&cfg)
		return cfg, nil
	}
	if ok, err := mergeConfigFile(ctx, ws, join(root, "claw.yml"), &cfg); err != nil {
		return claw.Config{}, err
	} else if ok {
		ExpandEnv(&cfg)
		return cfg, nil
	}
	if ok, err := mergeConfigFile(ctx, ws, join(root, "claw.json"), &cfg); err != nil {
		return claw.Config{}, err
	} else if ok {
		ExpandEnv(&cfg)
		return cfg, nil
	}

	files := []struct {
		name string
		out  any
	}{
		{"workspace", &cfg.Workspace},
		{"models", &cfg.Models},
		{"memory", &cfg.Memory},
		{"agent", &cfg.Agent},
	}
	for _, f := range files {
		for _, ext := range []string{".yaml", ".yml", ".json"} {
			if _, err := mergeConfigFile(ctx, ws, join(root, f.name+ext), f.out); err != nil {
				return claw.Config{}, err
			}
		}
	}
	ExpandEnv(&cfg)
	return cfg, nil
}

func WriteExample(exampleFS fs.FS, exampleName, workspaceDir string) error {
	exampleName = strings.TrimSpace(exampleName)
	if exampleName == "" || exampleName == "." || !fs.ValidPath(exampleName) || strings.Contains(exampleName, "/") {
		return fmt.Errorf("invalid example %q", exampleName)
	}
	docs, err := readExampleConfig(exampleFS, exampleName)
	if err != nil {
		return err
	}
	for name, raw := range docs {
		dst := filepath.Join(workspaceDir, "config", name+".json")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists", dst)
			}
			return err
		}
		if _, err := f.Write(raw); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

func readExampleConfig(exampleFS fs.FS, exampleName string) (map[string][]byte, error) {
	out := map[string][]byte{}
	templatePath, err := findExampleTemplate(exampleFS, exampleName)
	if err != nil {
		return nil, err
	}
	raw, err := fs.ReadFile(exampleFS, templatePath)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(templatePath))
	base := strings.TrimSuffix(filepath.Base(templatePath), filepath.Ext(templatePath))
	switch ext {
	case ".json":
		name, encoded, err := encodeJSONTemplate(base, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", templatePath, err)
		}
		out[name] = encoded
	case ".yaml", ".yml":
		docs, err := encodeYAMLTemplate(base, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", templatePath, err)
		}
		for name, encoded := range docs {
			out[name] = encoded
		}
	}
	return out, nil
}

func findExampleTemplate(exampleFS fs.FS, exampleName string) (string, error) {
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		path := "examples/" + exampleName + ext
		info, err := fs.Stat(exampleFS, path)
		if err == nil && !info.IsDir() {
			return path, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("example %q: %w", exampleName, fs.ErrNotExist)
}

func encodeJSONTemplate(base string, raw []byte) (string, []byte, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return "", nil, err
	}
	name := base
	target := unwrapDocumentNode(&node)
	if isEnvelope(target) {
		var env envelope
		if err := target.Decode(&env); err != nil {
			return "", nil, err
		}
		var err error
		name, err = fileNameForKind(env.Kind)
		if err != nil {
			return "", nil, err
		}
		target = unwrapDocumentNode(&env.Spec)
	}
	encoded, err := encodeNodeAsJSON(target)
	return name, encoded, err
}

func encodeYAMLTemplate(base string, raw []byte) (map[string][]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	out := map[string][]byte{}
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		node := unwrapDocumentNode(&doc)
		if isEmptyYAMLNode(node) {
			continue
		}
		name := base
		if isEnvelope(node) {
			var env envelope
			if err := node.Decode(&env); err != nil {
				return nil, err
			}
			if env.APIVersion != "" && env.APIVersion != APIVersion {
				return nil, fmt.Errorf("unsupported apiVersion %q for kind %q", env.APIVersion, env.Kind)
			}
			var err error
			name, err = fileNameForKind(env.Kind)
			if err != nil {
				return nil, err
			}
			node = unwrapDocumentNode(&env.Spec)
		}
		encoded, err := encodeNodeAsJSON(node)
		if err != nil {
			return nil, err
		}
		out[name] = encoded
	}
}

func encodeNodeAsJSON(node *yaml.Node) ([]byte, error) {
	var value any
	if err := node.Decode(&value); err != nil {
		return nil, err
	}
	return json.MarshalIndent(value, "", "  ")
}

func mergeConfigFile(ctx context.Context, ws sdkworkspace.Workspace, path string, out any) (bool, error) {
	raw, err := ws.Read(ctx, path)
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if strings.HasSuffix(path, ".json") {
		if err := json.Unmarshal(raw, out); err != nil {
			return false, fmt.Errorf("claw: decode %s: %w", path, err)
		}
		return true, nil
	}
	if err := decodeYAMLDocuments(raw, out); err != nil {
		return false, fmt.Errorf("claw: decode %s: %w", path, err)
	}
	return true, nil
}

func decodeYAMLDocuments(raw []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		node := unwrapDocumentNode(&doc)
		if isEmptyYAMLNode(node) {
			continue
		}
		if isEnvelope(node) {
			if err := decodeEnvelope(node, out); err != nil {
				return err
			}
			continue
		}
		if err := node.Decode(out); err != nil {
			return err
		}
	}
}

func decodeEnvelope(node *yaml.Node, out any) error {
	var env envelope
	if err := node.Decode(&env); err != nil {
		return err
	}
	if env.APIVersion != "" && env.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q for kind %q", env.APIVersion, env.Kind)
	}
	spec := unwrapDocumentNode(&env.Spec)
	if isEmptyYAMLNode(spec) {
		return fmt.Errorf("kind %q has empty spec", env.Kind)
	}
	switch target := out.(type) {
	case *claw.Config:
		return decodeEnvelopeIntoConfig(normalizeKind(env.Kind), spec, target)
	case *claw.WorkspaceConfig:
		return decodeMatchingKind(normalizeKind(env.Kind), spec, target, "workspaceconfig")
	case *claw.ModelsConfig:
		return decodeMatchingKind(normalizeKind(env.Kind), spec, target, "modelsconfig")
	case *claw.MemoryConfig:
		return decodeMatchingKind(normalizeKind(env.Kind), spec, target, "memoryconfig")
	case *claw.AgentConfig:
		return decodeMatchingKind(normalizeKind(env.Kind), spec, target, "agentconfig")
	default:
		return fmt.Errorf("unsupported config target %T for kind %q", out, env.Kind)
	}
}

func decodeEnvelopeIntoConfig(kind string, spec *yaml.Node, cfg *claw.Config) error {
	switch kind {
	case "claw", "clawconfig", "config":
		return spec.Decode(cfg)
	case "workspace", "workspaceconfig":
		return spec.Decode(&cfg.Workspace)
	case "models", "modelsconfig":
		return spec.Decode(&cfg.Models)
	case "memory", "memoryconfig":
		return spec.Decode(&cfg.Memory)
	case "agent", "agentconfig":
		return spec.Decode(&cfg.Agent)
	default:
		return fmt.Errorf("unsupported config kind %q", kind)
	}
}

func decodeMatchingKind[T any](kind string, spec *yaml.Node, target *T, allowed ...string) error {
	for _, v := range allowed {
		if kind == v {
			return spec.Decode(target)
		}
	}
	return fmt.Errorf("unsupported config kind %q for target %T", kind, target)
}

func fileNameForKind(kind string) (string, error) {
	switch normalizeKind(kind) {
	case "workspace", "workspaceconfig":
		return "workspace", nil
	case "models", "modelsconfig":
		return "models", nil
	case "memory", "memoryconfig":
		return "memory", nil
	case "agent", "agentconfig":
		return "agent", nil
	default:
		return "", fmt.Errorf("unsupported config kind %q", kind)
	}
}

func normalizeKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	kind = strings.ReplaceAll(kind, "-", "")
	kind = strings.ReplaceAll(kind, "_", "")
	kind = strings.ReplaceAll(kind, " ", "")
	return kind
}

func unwrapDocumentNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return node.Content[0]
	}
	return node
}

func isEmptyYAMLNode(node *yaml.Node) bool {
	return node == nil || node.Kind == 0 || (node.Kind == yaml.ScalarNode && node.Tag == "!!null")
}

func isEnvelope(node *yaml.Node) bool {
	return mappingValue(node, "kind") != nil && mappingValue(node, "spec") != nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	node = unwrapDocumentNode(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		if k.Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func cleanRoot(root string) string {
	root = strings.Trim(strings.TrimSpace(root), "/")
	if root == "" {
		return "config"
	}
	if root == "." {
		return "."
	}
	return root
}

func join(root, name string) string {
	root = cleanRoot(root)
	if root == "." {
		return name
	}
	return root + "/" + name
}

// ExpandEnv expands ${VAR} and $VAR in every string field in cfg.
func ExpandEnv(cfg *claw.Config) {
	if cfg == nil {
		return
	}
	expandEnvValue(reflect.ValueOf(cfg).Elem())
}

func expandEnvValue(v reflect.Value) {
	if !v.IsValid() {
		return
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		expandEnvValue(v.Elem())
		return
	}
	if !v.CanSet() && v.Kind() != reflect.Map && v.Kind() != reflect.Slice && v.Kind() != reflect.Struct && v.Kind() != reflect.Interface {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(os.ExpandEnv(v.String()))
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if field.CanSet() || field.Kind() == reflect.Map || field.Kind() == reflect.Slice || field.Kind() == reflect.Struct || field.Kind() == reflect.Pointer || field.Kind() == reflect.Interface {
				expandEnvValue(field)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			expandEnvValue(v.Index(i))
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return
		}
		for _, key := range v.MapKeys() {
			value := expandEnvMapValue(v.MapIndex(key))
			v.SetMapIndex(key, value)
		}
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		expanded := expandEnvInterface(v.Interface())
		if v.CanSet() {
			v.Set(reflect.ValueOf(expanded))
		}
	}
}

func expandEnvMapValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	if v.Kind() == reflect.Struct || v.Kind() == reflect.Map || v.Kind() == reflect.Slice || v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		cp := reflect.New(v.Type()).Elem()
		cp.Set(v)
		expandEnvValue(cp)
		return cp
	}
	expanded := expandEnvInterface(v.Interface())
	if expanded == nil {
		return reflect.Zero(v.Type())
	}
	ev := reflect.ValueOf(expanded)
	if ev.Type().AssignableTo(v.Type()) {
		return ev
	}
	if ev.Type().ConvertibleTo(v.Type()) {
		return ev.Convert(v.Type())
	}
	return v
}

func expandEnvInterface(v any) any {
	switch x := v.(type) {
	case string:
		return os.ExpandEnv(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = expandEnvInterface(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = expandEnvInterface(item)
		}
		return out
	default:
		return v
	}
}
