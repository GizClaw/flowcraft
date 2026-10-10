package skill

import "strings"

// renderHeader is the fixed preamble of the per-turn section. It
// names the paired tools by convention: this package renders the
// section, and the application ships skill_search and skill_read on
// top of [Service.RankScored] and [Service.ReadFull].
const renderHeader = "## Skills\n" +
	"Skills relevant to this turn. To use one, search with skill_search " +
	"and load its full instructions with skill_read. The user can also " +
	"activate a skill by mentioning $name.\n"

// RenderSection renders the per-turn "## Skills" metadata list. Bodies
// are never inlined: one line per skill carries the name, the
// description and the file path, and the model opens a body on demand
// through the paired tools. An empty list renders "".
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
