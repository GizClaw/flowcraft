package deploy

import (
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	yamlv3 "gopkg.in/yaml.v3"
)

// SubDocument is the settings shape shared by every resource impl that
// wraps a module's own config loader — workspace, sandbox, inference,
// tool. Each of those modules already parses a versioned YAML document
// of its own, so a deployment does not restate their schemas: it says
// where the document is.
//
// Exactly one form must be present:
//
//	settings: {file: ./workspaces.yaml}   # standalone file
//
//	settings:                             # kept inline
//	  inline:
//	    version: v1
//	    workspaces:
//	      project: {driver: local, settings: {root: .}}
//
// Inline keeps the whole deployment in one pure-YAML file; file keeps
// large sections reviewable on their own. Both feed the module's
// Parse, so the module's own version field and strictness still
// apply — there is no second schema to keep in sync.
type SubDocument struct {
	File   string  `yaml:"file,omitempty"`
	Inline *Opaque `yaml:"inline,omitempty"`
}

// YAML returns the sub-document's bytes, ready for the module's Parse.
//
// Requiring exactly one form is deliberate: a mistyped file key that
// silently fell back to an empty inline document would produce an
// empty registry and fail much later, at the first missing ref.
func (s SubDocument) YAML() ([]byte, error) {
	switch {
	case s.File != "" && s.Inline != nil:
		return nil, errdefs.Validationf(
			"deploy config settings: file and inline are mutually exclusive")
	case s.File != "":
		data, err := os.ReadFile(s.File)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"deploy config settings: read %s: %w", s.File, err))
		}
		return data, nil
	case s.Inline != nil:
		data, err := yamlv3.Marshal(s.Inline.Node())
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"deploy config settings: re-encode inline document: %w", err))
		}
		return data, nil
	default:
		return nil, errdefs.Validationf(
			"deploy config settings: file or inline is required")
	}
}
