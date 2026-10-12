package skill

import "testing"

func FuzzParseFrontmatter(f *testing.F) {
	for _, seed := range []string{
		"---\nname: x\ndescription: d\n---\n\nbody",
		"---\nname: \"a\"\ndescription: 'b'\n---\n",
		"no frontmatter",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _, err := splitFrontmatter([]byte(input))
		if err != nil {
			return
		}
		_, _ = parseBytes("fuzz/SKILL.md", []byte(input))
	})
}
