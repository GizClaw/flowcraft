package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func ensureClawConfigDir() error {
	root, err := clawConfigDir()
	if err != nil {
		return err
	}
	return syncEmbeddedConfigs(templateFS, root)
}

func clawConfigDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv("CLAW_CONFIG_DIR")); override != "" {
		return filepath.Clean(override), nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(configDir, "claw"), nil
}

func syncEmbeddedConfigs(src fs.FS, clawRoot string) error {
	return fs.WalkDir(src, "examples", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
		default:
			return nil
		}
		raw, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		rel, ok := clawConfigRelPath(path)
		if !ok {
			return nil
		}
		dst := filepath.Join(clawRoot, filepath.FromSlash(rel))
		return writeFileIfMissing(dst, raw)
	})
}

func clawConfigRelPath(embeddedPath string) (string, bool) {
	switch {
	case strings.HasPrefix(embeddedPath, "examples/raids/"):
		return "configs/raid/" + strings.TrimPrefix(embeddedPath, "examples/raids/"), true
	case strings.HasPrefix(embeddedPath, "examples/personas/"):
		return "configs/persona/" + strings.TrimPrefix(embeddedPath, "examples/personas/"), true
	case strings.HasPrefix(embeddedPath, "examples/test/"):
		return "configs/test/" + strings.TrimPrefix(embeddedPath, "examples/test/"), true
	default:
		return "", false
	}
}

func writeFileIfMissing(path string, data []byte) error {
	_, err := os.Stat(path)
	if err == nil {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
