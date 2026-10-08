package plugin

import "sort"

// CharterRow documents one plugin-declared contribution: which
// manifest field carries it, which permission authorizes it, what
// happens when the declaration arrives without that permission, and
// why a permission might have no gate in this module.
type CharterRow struct {
	// Kind names the contribution ("skills", "hooks", "mcp", "nodes",
	// "ui").
	Kind string
	// ManifestField is the Go field name on Manifest.
	ManifestField string
	// Permission is the manifest permission that authorizes it.
	Permission string
	// DropRule is the fail-closed answer to a declaration that arrives
	// without the permission: the host drops the section and keeps
	// loading the plugin. It is the written record of the ignore;
	// charter_source_test in package craft pairs it with a runtime
	// probe that shows the drop.
	DropRule string
	// Exemption names the owner of a check that does not live in this
	// module, for example the shell. Empty means the source audit must
	// find a negated HasPermission gate for the permission; a set value
	// makes the audit require the opposite, so an exemption cannot
	// outlive its subject.
	Exemption string
}

// Charter is the contribution table. charter_test scans it against
// Manifest and validPermissions in both directions;
// charter_source_test audits the gates behind it.
var Charter = []CharterRow{
	{
		Kind: "mcp", ManifestField: "MCP", Permission: "mcp:provide",
		DropRule: "the mcp section is dropped: no child process starts, " +
			"no host token is minted, and a plugin node bound to it " +
			"fails with the missing grant",
	},
	{
		Kind: "skills", ManifestField: "Skills",
		Permission: "skills:provide",
		DropRule: "the skills list is dropped: SkillRoots omits the " +
			"plugin's skill directories",
	},
	{
		Kind: "hooks", ManifestField: "Hooks", Permission: "hooks:provide",
		DropRule: "the hooks list is dropped: HookFiles omits the " +
			"plugin's hook files",
	},
	{
		Kind: "nodes", ManifestField: "Nodes", Permission: "nodes:provide",
		DropRule: "the nodes section is dropped: no node resource and no " +
			"engine dependency is synthesized",
	},
	{
		Kind: "ui", ManifestField: "UI", Permission: "ui:webview",
		DropRule: "nothing is dropped here: craft hands ui.entry to the " +
			"shell without consulting the permission",
		Exemption: "the shell's UI registry gates the bundle; craft only " +
			"validates the path and publishes it",
	},
	{
		Kind: "storage", Permission: "storage:kv",
		DropRule: "nothing is dropped here: plugin KV stays reachable " +
			"through the host regardless",
		Exemption: "the shell binds plugin KV per plugin id; Host.KV is " +
			"the host-side accessor and carries no gate",
	},
}

// ValidPermissions returns the sorted permission whitelist.
func ValidPermissions() []string {
	out := make([]string, 0, len(validPermissions))
	for permission := range validPermissions {
		out = append(out, permission)
	}
	sort.Strings(out)
	return out
}
