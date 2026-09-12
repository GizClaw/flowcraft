package model

import "fmt"

// ModelID is the public, credential-free identity of a provider model.
type ModelID struct {
	Provider string `json:"provider" yaml:"provider"`
	Name     string `json:"name" yaml:"name"`
}

func (id ModelID) Validate() error {
	if id.Provider == "" {
		return fmt.Errorf("model provider is required")
	}
	if id.Name == "" {
		return fmt.Errorf("model name is required")
	}
	return nil
}

// ModelRef combines a public model identity with an internal credential
// profile used only while resolving a call.
type ModelRef struct {
	ID      ModelID `json:"id" yaml:"id"`
	Profile string  `json:"profile,omitempty" yaml:"profile,omitempty"`
}

func (r ModelRef) Validate() error {
	return r.ID.Validate()
}
