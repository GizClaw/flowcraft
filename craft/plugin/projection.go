package plugin

import "github.com/GizClaw/flowcraft/craft/plugin/server"

// Projection is what one entry contributes to its host: the declarations
// that entry made, filtered by the grants this host approved for it.
//
// Every field is already filtered, which is the point. A consumer reads a
// field and publishes it, so there is no gate to remember and nothing to
// re-check: the question "may this plugin publish its skills?" is answered
// once, when the projection is built, and the answer is what the field
// holds. The zero value contributes nothing, which is what an invalid
// entry, a colliding one and a plugin nobody granted anything to all are.
type Projection struct {
	// Tools reports whether the entry's process may publish the tools it
	// declares over the wire.
	Tools bool
	// Skills are the skill directories the entry declares, as manifest
	// relative paths.
	Skills []string
	// Hooks are the hook files the entry declares, as manifest relative
	// paths.
	Hooks []string
	// Nodes are the graph nodes the entry declares.
	Nodes []NodeDecl
	// Dropped names the channels this entry declares without the grant
	// that publishes them, in channel order. It is the written record of a
	// refusal: the plugin still loads, and the section it declared is not
	// published anywhere.
	Dropped []string
}

// Projection returns what the entry contributes.
//
// It is the only supported way to read a contribution. The manifest's own
// sections are the plugin's declaration, not this host's decision about
// it, and a caller that reads them directly publishes whatever the plugin
// asked for whether or not it was granted.
func (e Entry) Projection() Projection {
	if e.Error != "" || e.Conflict != "" {
		return Projection{}
	}
	return e.Manifest.project(e.Grants)
}

// project filters the manifest's contributions through the grants.
func (m Manifest) project(grants server.GrantSet) Projection {
	held := func(grant Grant) bool { return grants.Has(string(grant)) }
	var out Projection
	out.Tools = held(GrantTools)
	out.Skills = publish(held(GrantSkills), m.Skills, "skills", &out.Dropped)
	out.Hooks = publish(held(GrantHooks), m.Hooks, "hooks", &out.Dropped)
	out.Nodes = publish(held(GrantNodes), m.Nodes, "nodes", &out.Dropped)
	return out
}

// publish is the projection's one rule: a section the grants cover passes
// through as declared, and a section they do not is dropped and named. A
// section nobody declared is not a drop — there was nothing to refuse.
func publish[T any](granted bool, declared []T, kind string, dropped *[]string) []T {
	if granted {
		return declared
	}
	if len(declared) > 0 {
		*dropped = append(*dropped, kind)
	}
	return nil
}
