// Package story reads the cast a workspace declares: which agent directs, which
// ones are seats, and what each is called.
//
// It exists because a seat's name is a presentation fact, not a deployment fact:
// the manager serves `seat_5`, and the table calls it 五号周岚. The mapping lives in
// one file beside the deployment document so a surface can label a stream without
// knowing which scenario wrote it.
package story

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// File is the cast declaration inside a workspace.
const File = "story.yaml"

// Cast is one workspace's declared cast.
type Cast struct {
	// Director is the agent that plans or referees. It has no label: it does not
	// speak.
	Director string `json:"director"`
	// Speakers are the agents that do speak, in declaration order.
	Speakers []string `json:"speakers"`
	// Labels are the user-facing names, keyed by agent. A missing label falls back
	// to the agent name, so a cast without labels still works.
	Labels map[string]string `json:"labels"`
}

// Load reads the cast beside a deployment document. A workspace without a
// story.yaml is a single-agent workspace: that is the other mode, not an error.
func Load(dir string) (Cast, error) {
	path := filepath.Join(dir, File)
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Cast{}, nil
		}
		return Cast{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cast Cast
	if err := yaml.Unmarshal(body, &cast); err != nil {
		return Cast{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := cast.validate(); err != nil {
		return Cast{}, fmt.Errorf("%s: %w", path, err)
	}
	return cast, nil
}

// Declared reports whether this cast describes a directed workspace.
func (c Cast) Declared() bool { return strings.TrimSpace(c.Director) != "" }

// Label is the user-facing name of one agent.
func (c Cast) Label(name string) string {
	if label := strings.TrimSpace(c.Labels[name]); label != "" {
		return label
	}
	return name
}

func (c Cast) validate() error {
	if !c.Declared() {
		return nil
	}
	seen := make(map[string]bool, len(c.Speakers))
	for _, name := range c.Speakers {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("speakers: an empty name")
		}
		if seen[name] {
			return fmt.Errorf("speakers: %q is declared twice", name)
		}
		seen[name] = true
	}
	return nil
}
