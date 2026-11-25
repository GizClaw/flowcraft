package skill

import (
	"strings"
	"testing"
)

// FuzzParse reads the one document this package takes from disks it does not own: a
// SKILL.md found in a workspace. Discovery walks whatever a deployment mounted, so the
// bytes are as untrusted as a provider payload — a front-matter parser is exactly the
// kind of code where a panic on an edge input turns a search result into a crash.
//
// The invariant beyond "no panic" is what the parser promises its caller: a document
// that parsed has a name (a missing one falls back to the directory) and a description
// (a missing one is the error case), so discovery never has to re-check either.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"---\nname: pdf\ndescription: Fill a form\n---\n\nInstructions.\n",
		"---\nname: pdf\n---\nbody",
		"---\ndescription: no name\n---\nbody",
		"---\nname: pdf\ndescription: d\n---\n",
		"---\nname: pdf\ndescription: d\nshort_description: s\n---\nbody",
		"no front matter at all",
		"---\n",
		"---\n---\n",
		"--- \nname: x\n---\n",
		"---\nname: [a list]\ndescription: d\n---\n",
		"---\nname: x\ndescription: d\n---\n---\nname: y\n---\n",
		strings.Repeat("---\nname: ", 100),
		"\xff\xfe\x00",
		"",
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		result, err := Parse("SKILL.md", data)
		if err != nil {
			return
		}
		if result.Metadata.Name == "" {
			t.Fatalf("parsed without a name, so nothing can discover it:\n%q", data)
		}
		if result.Metadata.Description == "" {
			t.Fatalf("parsed without a description, which the parser documents as an error:\n%q", data)
		}
	})
}
