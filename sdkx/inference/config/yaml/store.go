// Package yaml persists inference configuration snapshots as strict YAML.
package yaml

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/route"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
	yamlv3 "gopkg.in/yaml.v3"
)

type Store struct {
	path string
	lock *sync.RWMutex
}

type fileLocker interface {
	Unlock() error
}

var pathLocks sync.Map

func New(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("YAML inference config path is required")
	}
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf(
			"resolve YAML inference config path: %w",
			err,
		)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		path = resolved
	} else if directory, directoryErr := filepath.EvalSymlinks(
		filepath.Dir(path),
	); directoryErr == nil {
		path = filepath.Join(directory, filepath.Base(path))
	}
	lock, _ := pathLocks.LoadOrStore(path, &sync.RWMutex{})
	return &Store{path: path, lock: lock.(*sync.RWMutex)}, nil
}

func (s *Store) Load(ctx context.Context) (config.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return config.Snapshot{}, err
	}
	s.lock.RLock()
	data, err := os.ReadFile(s.path)
	s.lock.RUnlock()
	if errors.Is(err, os.ErrNotExist) {
		return config.Snapshot{}, config.ErrNotFound
	}
	if err != nil {
		return config.Snapshot{}, fmt.Errorf(
			"read YAML inference config: %w",
			err,
		)
	}
	document, err := decode(data)
	if err != nil {
		return config.Snapshot{}, err
	}
	return config.Snapshot{
		Document: document,
		Revision: revision(data),
	}, nil
}

func (s *Store) Save(
	ctx context.Context,
	expectedRevision string,
	document config.Document,
) (config.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return config.Snapshot{}, err
	}
	if err := document.Validate(); err != nil {
		return config.Snapshot{}, err
	}
	data, err := encode(document)
	if err != nil {
		return config.Snapshot{}, err
	}
	// Create the target directory before entering the critical section so the
	// lock ordering below stays: in-process mutex -> cross-process file lock.
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return config.Snapshot{}, fmt.Errorf(
			"create YAML inference config directory: %w",
			err,
		)
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	fileLock, err := lockFile(ctx, s.path+".lock")
	if err != nil {
		return config.Snapshot{}, err
	}
	defer fileLock.Unlock()
	current, readErr := os.ReadFile(s.path)
	exists := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return config.Snapshot{}, fmt.Errorf(
			"read current YAML inference config: %w",
			readErr,
		)
	}
	currentRevision := ""
	if exists {
		currentRevision = revision(current)
	}
	if expectedRevision != config.AnyRevision &&
		((expectedRevision == "" && exists) ||
			(expectedRevision != "" &&
				expectedRevision != currentRevision)) {
		return config.Snapshot{}, fmt.Errorf(
			"%w: expected %q, current %q",
			config.ErrConflict,
			expectedRevision,
			currentRevision,
		)
	}
	if err := ctx.Err(); err != nil {
		return config.Snapshot{}, err
	}
	temporary, err := os.CreateTemp(
		directory,
		".inference-config-*",
	)
	if err != nil {
		return config.Snapshot{}, fmt.Errorf(
			"create temporary YAML inference config: %w",
			err,
		)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	err = temporary.Chmod(0o600)
	if err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return config.Snapshot{}, fmt.Errorf(
			"write YAML inference config: %w",
			err,
		)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return config.Snapshot{}, fmt.Errorf(
			"replace YAML inference config: %w",
			err,
		)
	}
	if err := syncDir(directory); err != nil {
		return config.Snapshot{}, fmt.Errorf(
			"sync YAML inference config directory: %w",
			err,
		)
	}
	return config.Snapshot{
		Document: document.Clone(),
		Revision: revision(data),
	}, nil
}

type documentWire struct {
	Version   string         `yaml:"version"`
	Providers []providerWire `yaml:"providers"`
	Route     *route.Policy  `yaml:"route,omitempty"`
}

type providerWire struct {
	ID       string        `yaml:"id"`
	Driver   string        `yaml:"driver"`
	Profiles []profileWire `yaml:"profiles,omitempty"`
	Spec     rawSpec       `yaml:"spec,omitempty"`
}

type profileWire struct {
	ID         string                      `yaml:"id,omitempty"`
	Operations []inference.Operation       `yaml:"operations,omitempty"`
	Secrets    map[string]config.SecretRef `yaml:"secrets,omitempty"`
	Spec       rawSpec                     `yaml:"spec,omitempty"`
}

type rawSpec struct {
	value json.RawMessage
}

func (s rawSpec) IsZero() bool {
	return len(s.value) == 0
}

func (s *rawSpec) UnmarshalYAML(node *yamlv3.Node) error {
	data, err := yamlNodeJSON(node)
	if err != nil {
		return err
	}
	s.value = data
	return nil
}

func (s rawSpec) MarshalYAML() (any, error) {
	if len(s.value) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(s.value))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return jsonValueYAMLNode(value)
}

func yamlNodeJSON(node *yamlv3.Node) (json.RawMessage, error) {
	if node.Kind == yamlv3.DocumentNode {
		if len(node.Content) != 1 {
			return nil, fmt.Errorf("spec YAML document is empty")
		}
		return yamlNodeJSON(node.Content[0])
	}
	switch node.Kind {
	case yamlv3.MappingNode:
		var output bytes.Buffer
		output.WriteByte('{')
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yamlv3.ScalarNode ||
				key.Tag != "!!str" {
				return nil, fmt.Errorf("spec object keys must be strings")
			}
			if index > 0 {
				output.WriteByte(',')
			}
			encodedKey, _ := json.Marshal(key.Value)
			output.Write(encodedKey)
			output.WriteByte(':')
			encodedValue, err := yamlNodeJSON(node.Content[index+1])
			if err != nil {
				return nil, err
			}
			output.Write(encodedValue)
		}
		output.WriteByte('}')
		return output.Bytes(), nil
	case yamlv3.SequenceNode:
		var output bytes.Buffer
		output.WriteByte('[')
		for index, child := range node.Content {
			if index > 0 {
				output.WriteByte(',')
			}
			encoded, err := yamlNodeJSON(child)
			if err != nil {
				return nil, err
			}
			output.Write(encoded)
		}
		output.WriteByte(']')
		return output.Bytes(), nil
	case yamlv3.ScalarNode:
		switch node.Tag {
		case "!!str":
			return json.Marshal(node.Value)
		case "!!bool":
			var value bool
			if err := node.Decode(&value); err != nil {
				return nil, err
			}
			return json.RawMessage(strconv.FormatBool(value)), nil
		case "!!null":
			return json.RawMessage("null"), nil
		case "!!int", "!!float":
			number := strings.ReplaceAll(node.Value, "_", "")
			if !json.Valid([]byte(number)) {
				return nil, fmt.Errorf(
					"spec number %q must use JSON number syntax",
					node.Value,
				)
			}
			return json.RawMessage(number), nil
		default:
			return nil, fmt.Errorf(
				"spec scalar %q is not JSON-compatible",
				node.Value,
			)
		}
	case yamlv3.AliasNode:
		return yamlNodeJSON(node.Alias)
	default:
		return nil, fmt.Errorf("unsupported YAML node in spec")
	}
}

func jsonValueYAMLNode(value any) (*yamlv3.Node, error) {
	switch current := value.(type) {
	case nil:
		return &yamlv3.Node{
			Kind:  yamlv3.ScalarNode,
			Tag:   "!!null",
			Value: "null",
		}, nil
	case bool:
		return &yamlv3.Node{
			Kind:  yamlv3.ScalarNode,
			Tag:   "!!bool",
			Value: strconv.FormatBool(current),
		}, nil
	case string:
		return &yamlv3.Node{
			Kind:  yamlv3.ScalarNode,
			Tag:   "!!str",
			Value: current,
		}, nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(current.String(), ".eE") {
			tag = "!!float"
		}
		return &yamlv3.Node{
			Kind:  yamlv3.ScalarNode,
			Tag:   tag,
			Value: current.String(),
		}, nil
	case []any:
		node := &yamlv3.Node{
			Kind: yamlv3.SequenceNode,
			Tag:  "!!seq",
		}
		for _, item := range current {
			child, err := jsonValueYAMLNode(item)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, child)
		}
		return node, nil
	case map[string]any:
		node := &yamlv3.Node{
			Kind: yamlv3.MappingNode,
			Tag:  "!!map",
		}
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child, err := jsonValueYAMLNode(current[key])
			if err != nil {
				return nil, err
			}
			node.Content = append(
				node.Content,
				&yamlv3.Node{
					Kind:  yamlv3.ScalarNode,
					Tag:   "!!str",
					Value: key,
				},
				child,
			)
		}
		return node, nil
	default:
		return nil, fmt.Errorf(
			"unsupported JSON value %T in spec",
			value,
		)
	}
}

func decode(data []byte) (config.Document, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var wire documentWire
	if err := decoder.Decode(&wire); err != nil {
		return config.Document{}, fmt.Errorf(
			"decode YAML inference config: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents")
		}
		return config.Document{}, fmt.Errorf(
			"decode YAML inference config: %w",
			err,
		)
	}
	document := config.Document{
		Version:   wire.Version,
		Providers: make([]config.ProviderConfig, len(wire.Providers)),
		Route:     wire.Route,
	}
	for providerIndex, provider := range wire.Providers {
		converted := config.ProviderConfig{
			ID:       provider.ID,
			Driver:   provider.Driver,
			Profiles: make([]config.ProfileConfig, len(provider.Profiles)),
			Spec:     provider.Spec.value,
		}
		for profileIndex, profile := range provider.Profiles {
			converted.Profiles[profileIndex] = config.ProfileConfig{
				ID:         profile.ID,
				Operations: profile.Operations,
				Secrets:    profile.Secrets,
				Spec:       profile.Spec.value,
			}
		}
		document.Providers[providerIndex] = converted
	}
	if err := document.Validate(); err != nil {
		return config.Document{}, err
	}
	return document, nil
}

func encode(document config.Document) ([]byte, error) {
	wire := documentWire{
		Version:   document.Version,
		Providers: make([]providerWire, len(document.Providers)),
		Route:     document.Route,
	}
	for providerIndex, provider := range document.Providers {
		converted := providerWire{
			ID:       provider.ID,
			Driver:   provider.Driver,
			Profiles: make([]profileWire, len(provider.Profiles)),
			Spec:     rawSpec{value: provider.Spec},
		}
		for profileIndex, profile := range provider.Profiles {
			converted.Profiles[profileIndex] = profileWire{
				ID:         profile.ID,
				Operations: profile.Operations,
				Secrets:    profile.Secrets,
				Spec:       rawSpec{value: profile.Spec},
			}
		}
		wire.Providers[providerIndex] = converted
	}
	var output bytes.Buffer
	encoder := yamlv3.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(wire); err != nil {
		return nil, fmt.Errorf("encode YAML inference config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close YAML inference config encoder: %w", err)
	}
	return output.Bytes(), nil
}

func revision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var _ config.Store = (*Store)(nil)
