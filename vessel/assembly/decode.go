package assembly

import (
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Decode reads a single JSON or YAML manifest and validates it.
func Decode(r io.Reader) (Manifest, error) {
	return DecodeWithDefaults(r, DefaultDefaults())
}

// DecodeWithDefaults reads a single JSON or YAML manifest and validates it
// using caller-supplied defaults for omitted backend fields.
func DecodeWithDefaults(r io.Reader, defaults Defaults) (Manifest, error) {
	return DecodeWithCatalog(r, defaults, nil)
}

// DecodeWithCatalog reads a single JSON or YAML manifest and validates explicit
// custom backend names against catalog.
func DecodeWithCatalog(r io.Reader, defaults Defaults, catalog *Catalog) (Manifest, error) {
	if r == nil {
		return Manifest{}, fmt.Errorf("vessel assembly: nil reader")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return Manifest{}, fmt.Errorf("vessel assembly: read manifest: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("vessel assembly: decode manifest: %w", err)
	}
	if err := m.ValidateWithCatalog(defaults, catalog); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// DecodeFile reads and validates a single JSON or YAML manifest file.
func DecodeFile(path string) (Manifest, error) {
	return DecodeFileWithDefaults(path, DefaultDefaults())
}

// DecodeFileWithDefaults reads and validates a single JSON or YAML manifest
// file using caller-supplied defaults for omitted backend fields.
func DecodeFileWithDefaults(path string, defaults Defaults) (Manifest, error) {
	return DecodeFileWithCatalog(path, defaults, nil)
}

// DecodeFileWithCatalog reads and validates a manifest file using caller
// defaults and catalog-registered custom backend names.
func DecodeFileWithCatalog(path string, defaults Defaults, catalog *Catalog) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("vessel assembly: open manifest %q: %w", path, err)
	}
	defer f.Close()
	return DecodeWithCatalog(f, defaults, catalog)
}
