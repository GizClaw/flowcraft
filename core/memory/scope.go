package memory

import (
	"errors"
	"fmt"
	"strings"
)

// Scope identifies a memory instance and its tenant. Conversation and dataset
// addresses intentionally live on individual requests so there is only one
// source of truth for each address.
type Scope struct {
	// RuntimeID names the configured memory resource.
	RuntimeID string
	// UserID is the tenant partition. Empty selects a documented global scope.
	UserID string
	// AgentID is part of the hard partition. Empty selects the explicit
	// global-agent partition; it never widens reads to other agents.
	AgentID string
}

// Validate enforces the stable partition contract.
func (s Scope) Validate() error {
	if strings.TrimSpace(s.RuntimeID) == "" {
		return NewError(KindInvalidRequest, "", errors.New("memory: scope runtime_id is required"))
	}
	for name, value := range map[string]string{
		"runtime_id": s.RuntimeID,
		"user_id":    s.UserID,
		"agent_id":   s.AgentID,
	} {
		if strings.ContainsRune(value, '\x00') {
			return NewError(KindInvalidRequest, "", fmt.Errorf("memory: scope %s must not contain NUL", name))
		}
	}
	return nil
}

// HardPartitionKey returns the collision-free runtime, tenant, and agent key.
// An empty AgentID is an explicit global-agent partition.
func (s Scope) HardPartitionKey() string {
	return s.RuntimeID + "\x00" + s.UserID + "\x00" + s.AgentID
}

// IsZero reports whether every field is empty.
func (s Scope) IsZero() bool {
	return s.RuntimeID == "" && s.UserID == "" && s.AgentID == ""
}

// String renders a log-friendly, non-contractual representation.
func (s Scope) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "rt=%s", s.RuntimeID)
	if s.UserID != "" {
		fmt.Fprintf(&b, ", user=%s", s.UserID)
	}
	if s.AgentID != "" {
		fmt.Fprintf(&b, ", agent=%s", s.AgentID)
	}
	return b.String()
}
