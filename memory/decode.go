package memory

import (
	"fmt"
	"io"
	"os"

	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"gopkg.in/yaml.v3"
)

// Decode reads one YAML or JSON memory spec and validates it through Compile.
func Decode(r io.Reader) (Spec, error) {
	if r == nil {
		return Spec{}, fmt.Errorf("memory: nil reader")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return Spec{}, fmt.Errorf("memory: read spec: %w", err)
	}
	var spec Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("memory: decode spec: %w", err)
	}
	if _, err := compiler.Compile(spec); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// DecodeFile reads one YAML or JSON memory spec file and validates it through
// Compile.
func DecodeFile(path string) (Spec, error) {
	f, err := os.Open(path)
	if err != nil {
		return Spec{}, fmt.Errorf("memory: open spec %q: %w", path, err)
	}
	defer f.Close()
	return Decode(f)
}
