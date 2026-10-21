package plugin

import (
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Grant is one capability a plugin asks the host to be allowed to use.
// Craft defines one grant per contribution channel — the ways a plugin
// publishes something into its host — and an application adds one per host
// primitive it serves (see [server.Primitive.Grant]).
//
// Every name is a dotted "resource.op", the shape a primitive id already
// has, so one vocabulary reads as one list: tools.provide and events.emit
// are both "what this plugin may use", and neither is spelled differently
// because of who defined it.
type Grant string

// Craft's own grants, one per contribution channel.
const (
	// GrantTools is the channel that publishes the tools a plugin's
	// process declares over the wire.
	GrantTools Grant = "tools.provide"
	// GrantSkills is the channel that publishes the manifest's skills
	// list.
	GrantSkills Grant = "skills.provide"
	// GrantHooks is the channel that publishes the manifest's hooks list.
	GrantHooks Grant = "hooks.provide"
	// GrantNodes is the channel that publishes the manifest's nodes
	// section.
	GrantNodes Grant = "nodes.provide"
	// GrantUI is the UI bundle. Craft serves it and reports the grant;
	// the shell decides whether to load it.
	GrantUI Grant = "ui.webview"
	// GrantStorage is the plugin's key/value store. Craft binds it per
	// plugin id; the shell decides what a plugin holding it may mount.
	GrantStorage Grant = "storage.kv"
)

// craftGrants is every grant craft defines, in the order a projection
// reports a drop.
var craftGrants = []Grant{
	GrantTools, GrantSkills, GrantHooks, GrantNodes, GrantUI, GrantStorage,
}

// Vocabulary is every grant name a manifest may require: craft's own
// channels plus the grants the application's host primitives declare. A
// name outside the vocabulary fails validation, so a typo fails the install
// instead of reading as a grant nothing will ever ask a primitive for.
type Vocabulary map[Grant]bool

// NewVocabulary returns the vocabulary of one host: craft's channels plus
// the grants the application's primitives require. Blank entries are
// skipped, so a catalog assembled from capabilities that contribute no
// primitives needs no filtering.
func NewVocabulary(grants ...string) Vocabulary {
	vocabulary := make(Vocabulary, len(craftGrants)+len(grants))
	for _, grant := range craftGrants {
		vocabulary[grant] = true
	}
	for _, grant := range grants {
		grant = strings.TrimSpace(grant)
		if grant == "" {
			continue
		}
		vocabulary[Grant(grant)] = true
	}
	return vocabulary
}

// Has reports whether grant may be required.
func (v Vocabulary) Has(grant Grant) bool { return v[grant] }

// Grants returns the vocabulary in name order, which is the order an error
// message names it in.
func (v Vocabulary) Grants() []string {
	out := make([]string, 0, len(v))
	for grant := range v {
		out = append(out, string(grant))
	}
	sort.Strings(out)
	return out
}

// requires reports whether the manifest asks for grant. The asks are the
// manifest's; what the plugin holds is [Entry.Grants], which is the host's.
func (m Manifest) requires(grant Grant) bool {
	for _, candidate := range m.Requires {
		if candidate == string(grant) {
			return true
		}
	}
	return false
}

// checkRun refuses a manifest that asks for the tools channel without a
// process to declare them from: tools arrive over the plugin wire, so the
// run section is the declaration. It sits here, beside the channel it is
// about, because it is the one part of the manifest's shape that has an
// owner other than the wire.
func (m Manifest) checkRun() error {
	if m.Run != nil || !m.requires(GrantTools) {
		return nil
	}
	return errdefs.Validationf(
		"plugin %s: %s requires a run section: a plugin publishes the "+
			"tools it declares from its own process", m.ID, GrantTools)
}
