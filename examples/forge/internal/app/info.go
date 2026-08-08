package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	flowcraftmemory "github.com/GizClaw/flowcraft/memory/config"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	configutils "github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	memoryhook "github.com/GizClaw/flowcraft/sdkx/memory/hook"
)

// inspectDocument reads metadata out of the native documents without
// building anything.
func inspectDocument(workspaceDir string, doc deploy.Document) (Info, error) {
	info := Info{ContextID: "__default__"}
	if rawSpeakers, err := os.ReadFile(filepath.Join(workspaceDir, "speakers.yaml")); err == nil {
		if speakers, err := decodeSpeakers(rawSpeakers); err == nil {
			info.Speakers = speakers
		}
	}
	for id, entry := range doc.Agents {
		info.AgentID = id
		info.AgentName = entry.Card.Name
		if info.AgentName == "" {
			info.AgentName = id
		}
		break
	}
	if rawMemory, err := os.ReadFile(filepath.Join(workspaceDir, "memory.yaml")); err == nil {
		if settings, err := decodeMemorySettings(rawMemory); err == nil {
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
					MaxItems int `json:"max_items"`
				} `json:"budget"`
			}
			if preparer.Settings != nil {
				settings, _ = sdkconfig.DecodeSettings[struct {
					Budget struct {
						MaxItems int `json:"max_items"`
					} `json:"budget"`
				}](preparer.Settings)
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
	doc, err := decodeInferenceCredentials(raw)
	if err != nil {
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

// decodeSpeakers reads the YAML authoring file with JSON semantics:
// utils converts the document at the boundary, and the result is a
// plain string map.
func decodeSpeakers(raw []byte) (map[string]string, error) {
	jsonData, err := configutils.ToJSON(raw)
	if err != nil {
		return nil, err
	}
	var speakers map[string]string
	if err := json.Unmarshal(jsonData, &speakers); err != nil {
		return nil, err
	}
	return speakers, nil
}

// decodeMemorySettings decodes memory.yaml through the JSON wire
// protocol. The settings type is JSON-tagged, so the YAML document
// must pass through utils.ToJSON before encoding/json sees it; this
// keeps keys like runtime_id matching the wire protocol.
func decodeMemorySettings(raw []byte) (flowcraftmemory.Settings, error) {
	jsonData, err := configutils.ToJSON(raw)
	if err != nil {
		return flowcraftmemory.Settings{}, err
	}
	var settings flowcraftmemory.Settings
	if err := json.Unmarshal(jsonData, &settings); err != nil {
		return flowcraftmemory.Settings{}, err
	}
	return settings, nil
}

type inferenceCredentials struct {
	Providers []struct {
		Profiles []struct {
			Secrets map[string]struct {
				Resolver string `json:"resolver"`
				Key      string `json:"key"`
			} `json:"secrets"`
		} `json:"profiles"`
	} `json:"providers"`
}

// decodeInferenceCredentials converts the YAML authoring document to
// JSON and decodes only the secret references the preflight needs.
// json.Unmarshal (not utils.Decode) is used on purpose: version, id,
// driver, and provider-specific fields are valid in inference.yaml but
// irrelevant here, and the preflight should stay best-effort.
func decodeInferenceCredentials(raw []byte) (inferenceCredentials, error) {
	jsonData, err := configutils.ToJSON(raw)
	if err != nil {
		return inferenceCredentials{}, err
	}
	var doc inferenceCredentials
	if err := json.Unmarshal(jsonData, &doc); err != nil {
		return inferenceCredentials{}, err
	}
	return doc, nil
}
