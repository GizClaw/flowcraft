package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

func formatYAMLTree(n *yaml.Node) {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		formatYAMLTree(n.Content[0])
		return
	}
	if n.Kind != yaml.MappingNode {
		return
	}
	// Sort key-value pairs by key string
	type pair struct {
		key   *yaml.Node
		value *yaml.Node
	}
	var pairs []pair
	for i := 0; i+1 < len(n.Content); i += 2 {
		pairs = append(pairs, pair{key: n.Content[i], value: n.Content[i+1]})
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].key.Value < pairs[j].key.Value
	})
	n.Content = n.Content[:0]
	for _, p := range pairs {
		formatYAMLTree(p.value)
		n.Content = append(n.Content, p.key, p.value)
	}
}

func formatYAMLFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	formatYAMLTree(&root)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		_ = enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func formatContracts(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml" {
			return nil
		}
		return formatYAMLFile(path)
	})
}
