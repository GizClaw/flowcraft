package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/config/utils"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Layer is one named configuration layer in a layered document. Later
// layers override earlier ones: maps merge recursively, scalars and
// arrays are replaced wholesale, and an explicit null deletes the key.
// Source forms (literal, content, file, embed) reuse the Loader, so
// path confinement, size limits, and error classification apply to
// every layer.
type Layer struct {
	// Name identifies the layer in Layered.Origins (e.g. "defaults",
	// "host"). Required.
	Name string
	// Source is the layer document.
	Source Source
}

// Layered is the merged result of LoadLayers: the JSON document ready
// for strict decoding by the consumer, plus per-key provenance.
type Layered struct {
	// Data is the merged document as JSON.
	Data []byte
	// Origins maps every present leaf key path ("tools.middlewares" ->
	// "tools.middlewares.0.kind") to the layer name that last set it.
	Origins map[string]string
}

// LoadLayers resolves every layer through the Loader, deep-merges them
// in order, and returns the merged JSON document plus provenance. The
// caller still owns strict decoding (DecodeSettings / utils.Decode), so
// unknown fields in any layer fail at the consumer's build step with
// the same guarantees as a single document.
func (l *Loader) LoadLayers(ctx context.Context, layers []Layer) (Layered, error) {
	if l == nil {
		return Layered{}, errdefs.Validationf("config layer: loader is nil")
	}
	if len(layers) == 0 {
		return Layered{}, errdefs.Validationf(
			"config layer: at least one layer is required")
	}
	for i, layer := range layers {
		if strings.TrimSpace(layer.Name) == "" {
			return Layered{}, errdefs.Validationf(
				"config layer: layers[%d]: name is required", i)
		}
	}

	merged := make(map[string]any)
	origins := make(map[string]string)
	for _, layer := range layers {
		data, err := l.Load(ctx, layer.Source)
		if err != nil {
			return Layered{}, fmt.Errorf("config layer %q: %w", layer.Name, err)
		}
		doc, err := decodeDocumentMap(data)
		if err != nil {
			return Layered{}, fmt.Errorf("config layer %q: %w", layer.Name, err)
		}
		mergeInto(merged, origins, doc, layer.Name, "")
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return Layered{}, errdefs.Internalf(
			"config layer: encode merged document: %v", err)
	}
	return Layered{Data: raw, Origins: origins}, nil
}

// decodeDocumentMap converts one JSON/YAML document to its map form.
// JSON arrays and scalars are rejected: a config layer must be an
// object.
func decodeDocumentMap(data []byte) (map[string]any, error) {
	jsonData, err := utils.ToJSON(data)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("config layer: decode document: %w", err)
	}
	return doc, nil
}

func mergeInto(dst map[string]any, origins map[string]string, src map[string]any, layerName, prefix string) {
	for key, value := range src {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if child, ok := value.(map[string]any); ok {
			existing, exists := dst[key]
			existingMap, isMap := existing.(map[string]any)
			if !exists || !isMap {
				existingMap = make(map[string]any)
				dst[key] = existingMap
			}
			mergeInto(existingMap, origins, child, layerName, path)
			continue
		}
		if value == nil {
			delete(dst, key)
			clearOriginsUnder(origins, path)
			delete(origins, path)
			continue
		}
		dst[key] = value
		clearOriginsUnder(origins, path)
		origins[path] = layerName
	}
}

func clearOriginsUnder(origins map[string]string, prefix string) {
	for path := range origins {
		if path == prefix || strings.HasPrefix(path, prefix+".") {
			delete(origins, path)
		}
	}
}
