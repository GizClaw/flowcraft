package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	coresandbox "github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdkx/sandbox/nsjail"
	"github.com/GizClaw/flowcraft/sdkx/sandbox/seatbelt"
)

// Built-in backend names.
const (
	BackendLocal    = "local"
	BackendSeatbelt = "seatbelt"
	BackendNSJail   = "nsjail"
)

func (b *Builder) registerBuiltins() {
	b.factories[BackendLocal] = buildLocal
	b.factories[BackendSeatbelt] = buildSeatbelt
	b.factories[BackendNSJail] = buildNSJail
}

type localSettings struct {
	DefaultMaxOutputBytes *int64 `yaml:"default_max_output_bytes,omitempty"`
}

func buildLocal(_ context.Context, input FactoryInput) (coresandbox.Runner, error) {
	settings, err := DecodeSettings[localSettings](input.Settings)
	if err != nil {
		return nil, decodeSettingsError(BackendLocal, err)
	}
	var options []coresandbox.Option
	if settings.DefaultMaxOutputBytes != nil {
		if *settings.DefaultMaxOutputBytes < 0 {
			return nil, errdefs.Validationf(
				"%s settings.default_max_output_bytes must be non-negative",
				BackendLocal)
		}
		options = append(options,
			coresandbox.WithMaxOutputBytes(*settings.DefaultMaxOutputBytes))
	}
	return coresandbox.NewLocalRunner(input.Root, options...), nil
}

type seatbeltSettings struct {
	Binary        string   `yaml:"binary,omitempty"`
	WritablePaths []string `yaml:"writable_paths,omitempty"`
}

func buildSeatbelt(_ context.Context, input FactoryInput) (coresandbox.Runner, error) {
	settings, err := DecodeSettings[seatbeltSettings](input.Settings)
	if err != nil {
		return nil, decodeSettingsError(BackendSeatbelt, err)
	}
	var options []seatbelt.RunnerOption
	if settings.Binary != "" {
		binary, err := resolveSettingPath(input.Root, settings.Binary)
		if err != nil {
			return nil, fmt.Errorf("seatbelt settings.binary: %w", err)
		}
		options = append(options, seatbelt.WithBinary(binary))
	}
	writable, err := resolveSettingPaths(input.Root, settings.WritablePaths)
	if err != nil {
		return nil, fmt.Errorf("seatbelt settings.writable_paths: %w", err)
	}
	if writable != nil {
		options = append(options, seatbelt.WithWritablePaths(writable...))
	}
	return seatbelt.New(input.Root, options...)
}

type nsjailSettings struct {
	Binary        string   `yaml:"binary,omitempty"`
	WritablePaths []string `yaml:"writable_paths,omitempty"`
	ExtraFlags    []string `yaml:"extra_flags,omitempty"`
}

func buildNSJail(_ context.Context, input FactoryInput) (coresandbox.Runner, error) {
	settings, err := DecodeSettings[nsjailSettings](input.Settings)
	if err != nil {
		return nil, decodeSettingsError(BackendNSJail, err)
	}
	var options []nsjail.RunnerOption
	if settings.Binary != "" {
		binary, err := resolveSettingPath(input.Root, settings.Binary)
		if err != nil {
			return nil, fmt.Errorf("nsjail settings.binary: %w", err)
		}
		options = append(options, nsjail.WithBinary(binary))
	}
	writable, err := resolveSettingPaths(input.Root, settings.WritablePaths)
	if err != nil {
		return nil, fmt.Errorf("nsjail settings.writable_paths: %w", err)
	}
	if writable != nil {
		options = append(options, nsjail.WithWritablePaths(writable...))
	}
	if settings.ExtraFlags != nil {
		options = append(options, nsjail.WithExtraFlags(settings.ExtraFlags...))
	}
	return nsjail.New(input.Root, options...)
}

func decodeSettingsError(backend string, err error) error {
	return errdefs.Validationf("decode %s settings: %v", backend, err)
}

func resolveSettingPaths(root string, paths []string) ([]string, error) {
	if paths == nil {
		return nil, nil
	}
	resolved := make([]string, len(paths))
	for i, path := range paths {
		if path == "" {
			return nil, errdefs.Validationf("path[%d] is empty", i)
		}
		value, err := resolveSettingPath(root, path)
		if err != nil {
			return nil, errdefs.Validationf("path[%d] %q: %v", i, path, err)
		}
		resolved[i] = value
	}
	return resolved, nil
}

// resolveSettingPath makes every relative built-in settings path relative to
// the referenced workspace root and rejects relative traversal outside it.
func resolveSettingPath(root, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	value := filepath.Clean(filepath.Join(root, path))
	cleanRoot := filepath.Clean(root)
	relative, err := filepath.Rel(cleanRoot, value)
	if err != nil {
		return "", errdefs.Validationf(
			"resolve relative path %q: %v", path, err)
	}
	if relative == ".." ||
		(len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator)) {
		return "", errdefs.Validationf(
			"relative path %q escapes workspace root", path)
	}
	realRoot, err := evalExistingPrefix(cleanRoot)
	if err != nil {
		return "", errdefs.Validationf(
			"resolve workspace root %q: %v", cleanRoot, err)
	}
	realValue, err := evalExistingPrefix(value)
	if err != nil {
		return "", errdefs.Validationf(
			"resolve relative path %q: %v", path, err)
	}
	relative, err = filepath.Rel(realRoot, realValue)
	if err != nil || relative == ".." ||
		(len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator)) {
		return "", errdefs.Validationf(
			"relative path %q escapes workspace root through a symlink", path)
	}
	return realValue, nil
}

func evalExistingPrefix(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	realParent, err := evalExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(path)), nil
}
