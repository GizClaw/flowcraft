package tooltest

import (
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Binding is the smallest authority a tool can be exercised under: a session and a
// grant that allows every wired tool, with an empty capability pocket. A tool that
// declares needs reads absent capabilities under it, which is the point.
func Binding() tool.Binding {
	return tool.Bind(Session("tooltest"), policy.Grant{})
}

// BindingFor returns a binding over a named session, so a test can assert that two
// calls did (or did not) share scratch space.
func BindingFor(id string) tool.Binding {
	return tool.Bind(Session(id), policy.Grant{})
}

// Session is a session named id with an empty scratch map.
func Session(id string) tool.Session { return &session{id: id, scratch: map[string]any{}} }

type session struct {
	id      string
	scratch map[string]any
}

func (s *session) ID() string { return s.id }
func (s *session) Scratch() tool.Scratch {
	return scratch(s.scratch)
}

type scratch map[string]any

func (s scratch) Load(key string) (any, bool) { value, ok := s[key]; return value, ok }
func (s scratch) Store(key string, value any) { s[key] = value }
func (s scratch) Delete(key string)           { delete(s, key) }
