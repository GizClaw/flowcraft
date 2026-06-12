package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const tuiWorkspaceMetaFile = "claw.workspace.json"

type tuiWorkspaceMeta struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	ConfigName   string `json:"config_name"`
	ConfigSource string `json:"config_source"`
	CreatedAt    string `json:"created_at"`
	LastOpenedAt string `json:"last_opened_at"`
}

type tuiRaidOption struct {
	Name   string
	Source string
}

type tuiWorkspaceOption struct {
	ID           string
	Name         string
	Path         string
	ConfigName   string
	ConfigSource string
	LastOpenedAt string
}

func tuiWorkspacesDir() (string, error) {
	root, err := clawConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "workspaces"), nil
}

func listTUIRaidOptions() ([]tuiRaidOption, error) {
	names, err := listRaids()
	if err != nil {
		return nil, err
	}
	out := make([]tuiRaidOption, 0, len(names))
	for _, name := range names {
		source, _, err := readConfigSource(templateFS, name)
		if err != nil {
			return nil, err
		}
		out = append(out, tuiRaidOption{Name: name, Source: source})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func createOrOpenTUIWorkspaceFromRaid(raidName string) (tuiWorkspaceOption, error) {
	raidName = strings.TrimSpace(raidName)
	if raidName == "" {
		return tuiWorkspaceOption{}, fmt.Errorf("raid name is required")
	}
	source, raw, err := readConfigSource(templateFS, raidName)
	if err != nil {
		return tuiWorkspaceOption{}, err
	}
	if _, err := decodeConfigFile(raw); err != nil {
		return tuiWorkspaceOption{}, fmt.Errorf("%s: %w", source, err)
	}
	workspacesDir, err := tuiWorkspacesDir()
	if err != nil {
		return tuiWorkspaceOption{}, err
	}
	id := tuiWorkspaceID("raid", source)
	workspaceDir := filepath.Join(workspacesDir, id)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return tuiWorkspaceOption{}, err
	}
	configPath := filepath.Join(workspaceDir, configFileName)
	if err := writeFileIfMissing(configPath, raw); err != nil {
		return tuiWorkspaceOption{}, err
	}
	meta, err := readTUIWorkspaceMeta(workspaceDir)
	if err != nil {
		return tuiWorkspaceOption{}, err
	}
	now := time.Now().Format(time.RFC3339)
	if meta.ID == "" {
		meta = tuiWorkspaceMeta{
			ID:           id,
			Kind:         "raid",
			ConfigName:   raidName,
			ConfigSource: source,
			CreatedAt:    now,
		}
	}
	meta.LastOpenedAt = now
	if err := writeTUIWorkspaceMeta(workspaceDir, meta); err != nil {
		return tuiWorkspaceOption{}, err
	}
	return tuiWorkspaceOption{
		ID:           meta.ID,
		Name:         workspaceDisplayName(meta, workspaceDir),
		Path:         workspaceDir,
		ConfigName:   meta.ConfigName,
		ConfigSource: meta.ConfigSource,
		LastOpenedAt: meta.LastOpenedAt,
	}, nil
}

func listTUIWorkspaceOptions() ([]tuiWorkspaceOption, error) {
	dir, err := tuiWorkspacesDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]tuiWorkspaceOption, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if _, err := os.Stat(filepath.Join(path, configFileName)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		meta, err := readTUIWorkspaceMeta(path)
		if err != nil {
			return nil, err
		}
		if meta.ID == "" {
			meta.ID = entry.Name()
		}
		out = append(out, tuiWorkspaceOption{
			ID:           meta.ID,
			Name:         workspaceDisplayName(meta, path),
			Path:         path,
			ConfigName:   meta.ConfigName,
			ConfigSource: meta.ConfigSource,
			LastOpenedAt: meta.LastOpenedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastOpenedAt != out[j].LastOpenedAt {
			return out[i].LastOpenedAt > out[j].LastOpenedAt
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func touchTUIWorkspace(path string) error {
	meta, err := readTUIWorkspaceMeta(path)
	if err != nil {
		return err
	}
	if meta.ID == "" {
		meta.ID = filepath.Base(path)
	}
	now := time.Now().Format(time.RFC3339)
	if meta.CreatedAt == "" {
		meta.CreatedAt = now
	}
	meta.LastOpenedAt = now
	return writeTUIWorkspaceMeta(path, meta)
}

func readTUIWorkspaceMeta(workspaceDir string) (tuiWorkspaceMeta, error) {
	raw, err := os.ReadFile(filepath.Join(workspaceDir, tuiWorkspaceMetaFile))
	if err != nil {
		if os.IsNotExist(err) {
			return tuiWorkspaceMeta{}, nil
		}
		return tuiWorkspaceMeta{}, err
	}
	var meta tuiWorkspaceMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return tuiWorkspaceMeta{}, err
	}
	return meta, nil
}

func writeTUIWorkspaceMeta(workspaceDir string, meta tuiWorkspaceMeta) error {
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(filepath.Join(workspaceDir, tuiWorkspaceMetaFile), raw, 0o644)
}

func workspaceDisplayName(meta tuiWorkspaceMeta, workspaceDir string) string {
	if strings.TrimSpace(meta.ConfigName) != "" {
		return meta.ConfigName
	}
	return filepath.Base(workspaceDir)
}

func tuiWorkspaceID(kind, source string) string {
	source = strings.TrimSpace(source)
	if abs, err := filepath.Abs(source); err == nil {
		source = abs
	}
	sum := sha256.Sum256([]byte(kind + "\n" + source))
	return hex.EncodeToString(sum[:])[:16]
}
