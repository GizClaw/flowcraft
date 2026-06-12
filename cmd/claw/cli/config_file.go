package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/GizClaw/flowcraft/sdkx/claw"
)

const configFileName = "config.yaml"

// configFile is the complete workspace configuration stored on disk.
// It intentionally mirrors sdkx/claw.Config so the CLI can validate and
// normalize examples before writing them into a workspace.
type configFile struct {
	Workspace    claw.WorkspaceConfig     `json:"workspace,omitempty" yaml:"workspace,omitempty"`
	Conversation claw.ConversationConfig  `json:"conversation,omitempty" yaml:"conversation,omitempty"`
	Settings     claw.ModelSettingsConfig `json:"settings,omitempty" yaml:"settings,omitempty"`
	Models       claw.ModelsConfig        `json:"models,omitempty" yaml:"models,omitempty"`
	History      claw.HistoryConfig       `json:"history,omitempty" yaml:"history,omitempty"`
	Memory       claw.MemoryConfig        `json:"memory,omitempty" yaml:"memory,omitempty"`
	Agent        claw.AgentConfig         `json:"agent,omitempty" yaml:"agent,omitempty"`
}

func WriteConfig(templateFS fs.FS, configSource, workspaceDir string) error {
	configSource = strings.TrimSpace(configSource)
	if configSource == "" {
		return fmt.Errorf("config source is required")
	}
	_, raw, err := readConfigSource(templateFS, configSource)
	if err != nil {
		return err
	}
	cfg, err := decodeConfigFile(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", configSource, err)
	}
	_ = cfg
	dst := filepath.Join(workspaceDir, configFileName)
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
	return f.Close()
}

func decodeConfigFile(raw []byte) (claw.Config, error) {
	var generic any
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		return claw.Config{}, err
	}
	normalized := normalizeYAMLValue(generic)
	jsonRaw, err := json.Marshal(normalized)
	if err != nil {
		return claw.Config{}, err
	}
	var file configFile
	if err := json.Unmarshal(jsonRaw, &file); err != nil {
		return claw.Config{}, err
	}
	return claw.Config{
		Workspace:    file.Workspace,
		Conversation: file.Conversation,
		Settings:     file.Settings,
		Models:       file.Models,
		History:      file.History,
		Memory:       file.Memory,
		Agent:        file.Agent,
	}, nil
}

func normalizeYAMLValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = normalizeYAMLValue(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[fmt.Sprint(k)] = normalizeYAMLValue(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = normalizeYAMLValue(item)
		}
		return out
	default:
		return v
	}
}

func readConfigSource(templateFS fs.FS, source string) (string, []byte, error) {
	if embedded, ok := embeddedExamplePath(source); ok {
		path, raw, err := readEmbeddedExample(templateFS, embedded)
		if err != nil {
			return "", nil, err
		}
		return path, raw, nil
	}
	if path, raw, ok, err := readClawHomeConfigSource(source); ok {
		return path, raw, err
	}
	if looksLikeLocalPath(source) {
		raw, err := os.ReadFile(source)
		if err != nil {
			return "", nil, err
		}
		return source, raw, nil
	}
	embedded, err := findConfigTemplate(templateFS, source)
	if err == nil {
		raw, err := fs.ReadFile(templateFS, embedded)
		if err != nil {
			return "", nil, err
		}
		return embedded, raw, nil
	}
	raw, fileErr := os.ReadFile(source)
	if fileErr == nil {
		return source, raw, nil
	}
	return "", nil, fmt.Errorf("config %q: embedded lookup: %w; local file: %v", source, err, fileErr)
}

func readClawHomeConfigSource(source string) (string, []byte, bool, error) {
	root, err := clawConfigDir()
	if err != nil {
		return "", nil, true, err
	}
	for _, rel := range clawHomeConfigCandidates(source) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err == nil {
			return path, raw, true, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return path, nil, true, err
		}
	}
	return "", nil, false, nil
}

func clawHomeConfigCandidates(source string) []string {
	source = strings.TrimPrefix(strings.TrimSpace(source), "./")
	if source == "" || !fs.ValidPath(source) {
		return nil
	}
	exts := []string{""}
	if filepath.Ext(source) == "" {
		exts = []string{".yaml", ".yml", ".json"}
	}
	var bases []string
	switch {
	case strings.HasPrefix(source, "raid/"):
		bases = append(bases, "configs/raid/"+strings.TrimPrefix(source, "raid/"))
	case strings.HasPrefix(source, "raids/"):
		bases = append(bases, "configs/raid/"+strings.TrimPrefix(source, "raids/"))
	case strings.HasPrefix(source, "persona/"):
		bases = append(bases, "configs/persona/"+strings.TrimPrefix(source, "persona/"))
	case strings.HasPrefix(source, "personas/"):
		bases = append(bases, "configs/persona/"+strings.TrimPrefix(source, "personas/"))
	case !strings.Contains(source, "/"):
		bases = append(bases, "configs/raid/"+source, "configs/persona/"+source)
	default:
		return nil
	}
	out := make([]string, 0, len(bases)*len(exts))
	for _, base := range bases {
		for _, ext := range exts {
			out = append(out, base+ext)
		}
	}
	return out
}

func readEmbeddedExample(templateFS fs.FS, path string) (string, []byte, error) {
	raw, err := fs.ReadFile(templateFS, path)
	if err == nil {
		return path, raw, nil
	}
	if filepath.Ext(path) != "" {
		return "", nil, err
	}
	for _, suffix := range []string{".yaml", ".yml", ".json"} {
		candidate := path + suffix
		raw, candidateErr := fs.ReadFile(templateFS, candidate)
		if candidateErr == nil {
			return candidate, raw, nil
		}
		if !errors.Is(candidateErr, fs.ErrNotExist) {
			return "", nil, candidateErr
		}
	}
	return "", nil, err
}

func embeddedExamplePath(source string) (string, bool) {
	source = strings.TrimSpace(source)
	if source == "" || !fs.ValidPath(source) {
		return "", false
	}
	source = strings.TrimPrefix(source, "./")
	if strings.HasPrefix(source, "examples/raid/") {
		source = "examples/raids/" + strings.TrimPrefix(source, "examples/raid/")
	}
	if strings.HasPrefix(source, "examples/persona/") {
		source = "examples/personas/" + strings.TrimPrefix(source, "examples/persona/")
	}
	if strings.HasPrefix(source, "examples/") {
		return source, true
	}
	return "", false
}

func looksLikeLocalPath(source string) bool {
	return strings.HasPrefix(source, ".") ||
		strings.HasPrefix(source, "/") ||
		strings.Contains(source, string(os.PathSeparator)) ||
		filepath.Ext(source) != ""
}

func findConfigTemplate(templateFS fs.FS, configName string) (string, error) {
	configName = strings.TrimSpace(configName)
	if configName == "" || configName == "." || !fs.ValidPath(configName) || strings.Contains(configName, "/") {
		return "", fmt.Errorf("invalid config %q", configName)
	}
	for _, dir := range []string{"examples/raids", "examples/personas"} {
		for _, ext := range []string{".yaml", ".yml", ".json"} {
			path := dir + "/" + configName + ext
			info, err := fs.Stat(templateFS, path)
			if err == nil && !info.IsDir() {
				return path, nil
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("config %q: %w", configName, fs.ErrNotExist)
}
