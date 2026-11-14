// Package skill discovers and parses SKILL.md documents and renders the
// per-turn skills section.
//
// A skill is a directory holding a SKILL.md: YAML frontmatter (name,
// description) plus a Markdown body the model reads on demand. This package
// never inlines a body — the section carries one line per skill, and a tool
// loads a body when the model asks for it.
//
// Reading goes through fsx, so a skill tree is whatever an fsx.Tree can reach
// and the policy that governs it is the caller's. Parsing is pure: Parse takes
// bytes, so a caller can test it without a filesystem.
//
// Not carried over from the previous implementation: the duplicate-key analysis
// of the frontmatter (two YAML keys collapsing onto one JSON key). It needs a
// second YAML decoder and a decode helper that no longer exists; the parser here
// refuses malformed documents and warns on shape issues, which covers the common
// failure, but a frontmatter carrying duplicate keys now loads silently. Add it
// back if a real skill is caught by it.
package skill
