package skill

import "strings"

// renderHeader is the fixed preamble of the per-turn section. Bodies are never
// inlined: the model loads one when it decides to use a skill.
const renderHeader = "## Skills\n" +
	"Skills relevant to this turn. Load a skill's full instructions before " +
	"using it. The user can also activate a skill by mentioning $name.\n"

// RenderSection renders the "## Skills" metadata list: one line per skill with
// the name, the description and the file path. An empty list renders "".
func RenderSection(skills []Metadata) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(renderHeader)
	for _, sk := range skills {
		b.WriteString("- ")
		b.WriteString(sk.Name)
		b.WriteString(": ")
		b.WriteString(truncateRunes(sk.Description, maxDescriptionLen))
		b.WriteString(" (file: ")
		b.WriteString(sk.Path)
		b.WriteString(")\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
