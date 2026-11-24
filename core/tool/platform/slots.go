package platform

import (
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/skill"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Package platform declares the capability slots the host fills and a tool module reads.
//
// They are not in the tool contract package on purpose: `core/internal/layout` forbids that package
// from naming a subsystem at all — "a field of type *fsx.Tree would put a subsystem in the contract" —
// and a slot variable naming one is the same mistake one indirection away. A subpackage keeps the
// contract free of subsystems and still gives both sides one declaration, which is what the rest of
// this comment is about.
//
// The platform's capability slots.
//
// A slot is normally declared by the package that owns the capability, and [Slot] identity is the
// name, so a tool in another module declares its own handle with the same name and type and the two
// are one capability. That works while both sides are in the same process and the names are written
// down once.
//
// These three are different in one way: the *host* fills them and an implementation module reads them,
// across a module boundary that neither side may cross in the other direction. A host cannot import a
// tool module (that is the rule that keeps the kind tables in the application), and a tool module
// cannot import the host. So if each side spelled the name itself, a typo would be a silent zero at
// call time, and a type that drifted would be a silent zero with no error at all — the registration
// check compares names, not types.
//
// Declaring them here makes that agreement a compile error, which is what a seam between two modules
// needs. A capability that one *tool* hands another stays declared locally, where it belongs: this file
// is the list of what the platform provides, not a registry of every capability.
var (
	// TreeSlot is the session's file boundary: the read handles and the write set a tool reads and
	// writes through. A path outside it is refused by the handle, not by a string comparison, so a
	// tool that used the os package instead would bypass the boundary entirely.
	TreeSlot = tool.NewSlot[*fsx.Tree]("fsx.tree")

	// ProcessSlot starts a process in the session's sandbox — the same starter the host wraps with
	// its approval policy, so a tool that runs a command gets the operator's decision for free.
	ProcessSlot = tool.NewSlot[sandbox.SessionStarter]("sandbox.process")

	// SkillsSlot is the session's skill index: the roots the deployment configured, parsed once per
	// process and read by the skill tools.
	SkillsSlot = tool.NewSlot[*skill.Index]("skill.index")
)
