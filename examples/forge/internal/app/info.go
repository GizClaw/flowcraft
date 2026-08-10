package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	configutils "github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
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
