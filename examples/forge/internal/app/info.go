package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/sdkx/deploy"
	flowcraftmemory "github.com/GizClaw/flowcraft/memory/config"
	memoryhook "github.com/GizClaw/flowcraft/sdkx/memory/hook"
	"gopkg.in/yaml.v3"
)

// inspectDocument reads metadata out of the native documents without
// building anything.
func inspectDocument(workspaceDir string, doc deploy.Document) (Info, error) {
	info := Info{ContextID: "__default__"}
	for id, entry := range doc.Agents {
		info.AgentID = id
		info.AgentName = entry.Card.Name
		if info.AgentName == "" {
			info.AgentName = id
		}
		break
	}
	if rawMemory, err := os.ReadFile(filepath.Join(workspaceDir, "memory.yaml")); err == nil {
		var settings flowcraftmemory.Settings
		if err := yaml.Unmarshal(rawMemory, &settings); err == nil {
			info.MemoryEnabled = true
			info.GenerateModel = settings.Generate.Provider + "/" + settings.Generate.Name
			if len(settings.Scopes) > 0 {
				info.MemoryScope = Scope{
					RuntimeID: settings.Scopes[0].RuntimeID,
					UserID:    settings.Scopes[0].UserID,
					AgentID:   settings.Scopes[0].AgentID,
				}
			}
		}
	}
	for _, entry := range doc.Agents {
		for _, preparer := range entry.Prepare {
			if preparer.Type != memoryhook.ContextType {
				continue
			}
			var settings struct {
				Budget struct {
					MaxItems int `yaml:"max_items"`
				} `yaml:"budget"`
			}
			if preparer.Settings != nil {
				if node := preparer.Settings.Node(); node != nil {
					_ = node.Decode(&settings)
				}
			}
			info.MemoryTopK = settings.Budget.MaxItems
			break
		}
		if info.MemoryTopK == 0 {
			info.MemoryTopK = 5
		}
	}
	return info, nil
}

func requireProviderCredential(workspaceDir string) error {
	raw, err := os.ReadFile(filepath.Join(workspaceDir, "inference.yaml"))
	if err != nil {
		return fmt.Errorf("read inference config: %w", err)
	}
	var doc struct {
		Providers []struct {
			Profiles []struct {
				Secrets map[string]struct {
					Resolver string `yaml:"resolver"`
					Key      string `yaml:"key"`
				} `yaml:"secrets"`
			} `yaml:"profiles"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode inference config: %w", err)
	}
	var keys []string
	for _, provider := range doc.Providers {
		for _, profile := range provider.Profiles {
			for _, secret := range profile.Secrets {
				if secret.Resolver == "env" && secret.Key != "" {
					keys = append(keys, secret.Key)
					if _, ok := os.LookupEnv(secret.Key); ok {
						return nil
					}
				}
			}
		}
	}
	return fmt.Errorf("no provider credentials available; set one of %s in .env", strings.Join(keys, ", "))
}
