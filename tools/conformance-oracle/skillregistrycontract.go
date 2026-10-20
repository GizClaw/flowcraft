package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/skill"
)

// The skill registry contract: what discovery, ranking and the snapshot
// service answer about a tree.
//
// The parse half (skill-contract.json) is about one file's bytes. This is about
// *which* files are read and in what order, which is where the port can get
// quietly wrong in the direction of reading more: a symlink that escapes the
// configured roots, a hidden directory that must be skipped, an alias that must
// not make the walk loop, a duplicate name that must be listed once per path,
// and a body read that must refuse a path the snapshot does not carry.
//
// A row is a tree and the questions put to it:
//
//	entries    -- the directories, files and symlinks the harness writes
//	roots      -- the configured roots, relative to the harness's directory
//	disabled   -- Options.Disabled, each entry marked "name" or "path"
//	discover   -- core's Outcome: skills, diagnostics, roots, scan roots
//	service    -- the same tree through a Service: List, Roots, Diagnostics,
//	              and the mention/rank/read answers
//
// Every absolute path in an answer is masked down to its logical form
// ("a/alpha/SKILL.md"), because the harness writes into a temporary directory
// whose name differs per run and per implementation. Two more strings are
// masked: an operating system's sentence after the verb and path core wrote,
// and a YAML library's error text after the sentence that classifies it -- the
// same rule the parse contract states, for the same reason.
//
// Deliberately not rows: a body read of a file that grew past the 256 KiB cap
// or was deleted after discovery (unreachable through a Service, which only
// serves paths it discovered, and each would need a post-discovery mutation
// step), and a directory the operator cannot read (a permission bit is not a
// contract).
//
// A ranking answer prints each score to twelve significant digits, by the same
// `pinnedScore` the bm25 dump uses and for the same reason: ranking goes
// through idf and `math.Log`, whose last bit is the host's rather than core's,
// and this file too is compared byte for byte on every architecture.

// registryContract is the committed document.
type registryContract struct {
	// Note says what the table is for.
	Note []string `json:"note"`
	// Masked names what a reader must not expect to compare.
	Masked []string `json:"masked"`
	// Trees are the fixtures and their answers.
	Trees []registryTree `json:"trees"`
	// Renders are the per-turn section, one answer per skill list. They need
	// no files: rendering reads metadata only.
	Renders []registryRender `json:"renders"`
}

// registryTree is one fixture tree and every question put to it.
type registryTree struct {
	// Name identifies the tree in a mismatch.
	Name string `json:"name"`
	// Why says what the tree is evidence of.
	Why string `json:"why"`
	// Entries are what the harness writes, parents first.
	Entries []registryEntry `json:"entries"`
	// Roots are the configured roots, relative to the harness's directory.
	Roots []string `json:"roots"`
	// Options are the service's configuration, when the tree has a service.
	// A tree with no roots and no entries, and no options, still asks for a
	// discovery answer.
	Disabled []registryDisable `json:"disabled,omitempty"`
	// TopN is Options.TopN as configured; zero means core's default.
	TopN int `json:"top_n,omitempty"`
	// MinScore is Options.MinScore.
	MinScore float64 `json:"min_score,omitempty"`
	// Discover is core's Outcome for the tree.
	Discover registryOutcome `json:"discover"`
	// Service is the same tree through a Service, when the row asks for one.
	Service *registryService `json:"service,omitempty"`
}

// registryEntry is one filesystem entry.
type registryEntry struct {
	// Path is where the entry is written, relative to the harness's directory.
	Path string `json:"path"`
	// Kind is "dir", "file" or "link"; empty means "file".
	Kind string `json:"kind,omitempty"`
	// Data is a file's bytes.
	Data string `json:"data,omitempty"`
	// Target is a link's target, relative to the harness's directory too, so
	// the two sides can build it without knowing the absolute path.
	Target string `json:"target,omitempty"`
}

// registryDisable is one Options.Disabled entry with its rule spelled out.
type registryDisable struct {
	// Kind is "name" or "path".
	Kind string `json:"kind"`
	// Value is the name, or the path relative to the harness's directory.
	Value string `json:"value"`
}

// registryOutcome is a discovery answer, with every path masked.
type registryOutcome struct {
	// Skills are the discovered skills, in Outcome order.
	Skills []registrySkill `json:"skills"`
	// Diagnostics are the non-fatal issues, in walk order.
	Diagnostics []registryDiagnostic `json:"diagnostics"`
	// Roots are the configured roots that contained a skill.
	Roots []string `json:"roots"`
	// ScanRoots are every configured root in absolute form.
	ScanRoots []string `json:"scan_roots"`
}

// registrySkill is one skill with its path masked.
type registrySkill struct {
	// Name is the validated name.
	Name string `json:"name"`
	// Description is the one-line description.
	Description string `json:"description"`
	// ShortDescription is the non-standard metadata.short-description.
	ShortDescription string `json:"short_description,omitempty"`
	// Path is the canonical path, masked.
	Path string `json:"path"`
}

// registryDiagnostic is one diagnostic with its path and OS sentence masked.
type registryDiagnostic struct {
	// Path is the path the diagnosis is about, masked.
	Path string `json:"path"`
	// Message is the sentence, masked.
	Message string `json:"message"`
	// Warning marks a tolerated shape issue.
	Warning bool `json:"warning,omitempty"`
}

// registryService is one tree's answers through a Service.
type registryService struct {
	// List is Service.List, after Options.Disabled.
	List []registrySkill `json:"list"`
	// Roots is Service.Roots.
	Roots []string `json:"roots"`
	// Diagnostics is Service.Diagnostics.
	Diagnostics []registryDiagnostic `json:"diagnostics"`
	// Mentions are the explicit $name rows.
	Mentions []registryMention `json:"mentions,omitempty"`
	// Rankings are the BM25 rows.
	Rankings []registryRanking `json:"rankings,omitempty"`
	// Reads are the skill_read rows.
	Reads []registryRead `json:"reads,omitempty"`
	// Reload re-runs discovery after the harness mutates the tree, and answers
	// with the new List. It is the only row that reads the tree twice.
	Reload *registryReload `json:"reload,omitempty"`
}

// registryMention is one mention question.
type registryMention struct {
	// Text is the input.
	Text string `json:"text"`
	// Answer is the resolved skills, in mention order.
	Answer []registrySkill `json:"answer"`
}

// registryRanking is one BM25 question.
type registryRanking struct {
	// Query is the ranking query.
	Query string `json:"query"`
	// TopN is the call's limit; zero means the service's.
	TopN int `json:"top_n"`
	// MinScore is the call's threshold.
	MinScore float64 `json:"min_score"`
	// Answer is the ranked skills with their scores.
	Answer []registryScored `json:"answer"`
}

// registryScored is one ranked skill.
type registryScored struct {
	// Name is the skill's name.
	Name string `json:"name"`
	// Path is the masked path.
	Path string `json:"path"`
	// Score is the BM25 score, to twelve significant digits; see the note at
	// the top about why it is not the whole double.
	Score string `json:"score"`
}

// registryRead is one body read.
type registryRead struct {
	// By is "name" or "path".
	By string `json:"by"`
	// Key is the name, or the masked path.
	Key string `json:"key"`
	// Answer is the read's answer.
	Answer registryReadAnswer `json:"answer"`
}

// registryReadAnswer is what one read returned.
type registryReadAnswer struct {
	// Path is the skill's masked path, when the read resolved one.
	Path string `json:"path,omitempty"`
	// Body is the frontmatter-stripped body, trimmed.
	Body string `json:"body,omitempty"`
	// Error is the refusal, when it refused.
	Error *string `json:"error,omitempty"`
}

// registryReload is the answer after a second discovery pass.
type registryReload struct {
	// Add is a file the harness writes between the two passes.
	Add registryEntry `json:"add"`
	// List is Service.List after Reload.
	List []registrySkill `json:"list"`
	// Diagnostics is Service.Diagnostics after Reload.
	Diagnostics []registryDiagnostic `json:"diagnostics"`
}

// registryRender is one per-turn section.
type registryRender struct {
	// Name identifies the row.
	Name string `json:"name"`
	// Why says what the row is evidence of.
	Why string `json:"why"`
	// Skills is the input list.
	Skills []registrySkill `json:"skills"`
	// Answer is the rendered section.
	Answer string `json:"answer"`
}

// writeSkillRegistryContract dumps the contract to w.
func writeSkillRegistryContract(w io.Writer) error {
	document, err := skillRegistryDocument()
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

// skillRegistryDocument runs every tree and returns the document.
func skillRegistryDocument() (registryContract, error) {
	trees, err := runRegistryTrees(registryTrees())
	if err != nil {
		return registryContract{}, err
	}
	return registryContract{
		Note: []string{
			"core/skill's registry, as a table: trees the harness builds, and what discovery, ranking and the snapshot service answer about them.",
			"Every absolute path is masked down to its logical form (a/alpha/SKILL.md), because the harness writes into a temporary directory whose name differs per run and per implementation.",
			"An operating system's sentence is masked after the verb and path core wrote, and a YAML library's error text after the sentence that classifies it: both are the platform's wording, not core's decision.",
			"Not rows: a body read of a file that grew past the cap or was deleted after discovery, and a directory the operator cannot read. Each needs a mutation step or a permission bit, and the second is the platform's sentence rather than the port's behavior.",
		},
		Masked: []string{
			"the harness's directory, in every string, in both its given and its canonical form",
			"an operating system's sentence after `open <path>: `, `stat <path>: `, `lstat <path>: ` or `read <path>: `",
			"the path inside a `resolve symlink: lstat <path>: ` diagnostic",
			"a YAML library's error text, after the sentence that classifies it",
		},
		Trees:   trees,
		Renders: registryRenders(),
	}, nil
}

// runRegistryTrees builds and asks every tree.
func runRegistryTrees(trees []registryTree) ([]registryTree, error) {
	out := make([]registryTree, 0, len(trees))
	for _, tree := range trees {
		// An absent list and an empty one read the same in a replay, but JSON
		// spells them differently: the document says `[]`, so a reader never has
		// to handle a null it did not ask for.
		if tree.Entries == nil {
			tree.Entries = []registryEntry{}
		}
		if tree.Roots == nil {
			tree.Roots = []string{}
		}
		ran, err := runRegistryTree(tree)
		if err != nil {
			return nil, fmt.Errorf("tree %s: %w", tree.Name, err)
		}
		out = append(out, ran)
	}
	return out, nil
}

// runRegistryTree materializes one tree and records every answer.
func runRegistryTree(tree registryTree) (registryTree, error) {
	directory, err := os.MkdirTemp("", "skill-registry-contract-")
	if err != nil {
		return tree, err
	}
	defer os.RemoveAll(directory)
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return tree, err
	}
	if err := materializeRegistryTree(directory, tree.Entries); err != nil {
		return tree, err
	}
	roots := make([]string, 0, len(tree.Roots))
	for _, root := range tree.Roots {
		roots = append(roots, filepath.Join(directory, filepath.FromSlash(root)))
	}
	mask := func(text string) string {
		return maskRegistryText(text, directory, canonical)
	}

	outcome := skill.Discover(nil, roots)
	tree.Discover = maskOutcome(outcome, mask)
	if tree.Service == nil {
		return tree, nil
	}
	disabled := make([]string, 0, len(tree.Disabled))
	for _, entry := range tree.Disabled {
		switch entry.Kind {
		case "path":
			// Discovery records canonical paths, so a disabled path has to be
			// built the same way: the harness's temporary directory is reached
			// through a symlink on macOS, and an unresolved form would match
			// nothing.
			disabled = append(disabled, filepath.Join(canonical, filepath.FromSlash(entry.Value)))
		default:
			disabled = append(disabled, entry.Value)
		}
	}
	service := skill.NewService(nil, skill.Options{
		Roots:    roots,
		TopN:     tree.TopN,
		MinScore: tree.MinScore,
		Disabled: disabled,
	})
	tree.Service.List = maskSkills(service.List(), mask)
	tree.Service.Roots = maskAll(service.Roots(), mask)
	tree.Service.Diagnostics = maskDiagnostics(service.Diagnostics(), mask)
	for index, mention := range tree.Service.Mentions {
		tree.Service.Mentions[index].Answer = maskSkills(service.Mentioned(mention.Text), mask)
	}
	for index, ranking := range tree.Service.Rankings {
		tree.Service.Rankings[index].Answer = runRegistryRanking(service, ranking, mask)
	}
	for index, read := range tree.Service.Reads {
		tree.Service.Reads[index].Answer = runRegistryRead(service, read, canonical, mask)
	}
	if tree.Service.Reload != nil {
		if err := materializeRegistryTree(directory, []registryEntry{tree.Service.Reload.Add}); err != nil {
			return tree, err
		}
		service.Reload()
		tree.Service.Reload.List = maskSkills(service.List(), mask)
		tree.Service.Reload.Diagnostics = maskDiagnostics(service.Diagnostics(), mask)
	}
	return tree, nil
}

// runRegistryRanking asks one BM25 question and masks the answer.
func runRegistryRanking(service *skill.Service, row registryRanking, mask func(string) string) []registryScored {
	scored := service.RankScored(row.Query, row.TopN, row.MinScore)
	out := make([]registryScored, 0, len(scored))
	for _, item := range scored {
		out = append(out, registryScored{
			Name:  item.Skill.Name,
			Path:  mask(item.Skill.Path),
			Score: pinnedScore(item.Score),
		})
	}
	return out
}

// runRegistryRead asks one body-read question. A path is resolved through the
// harness's canonical directory, because that is the form discovery records.
func runRegistryRead(
	service *skill.Service, row registryRead, canonical string, mask func(string) string,
) registryReadAnswer {
	var (
		sk   skill.Metadata
		body string
		err  error
	)
	if row.By == "path" {
		sk, body, err = service.ReadByPath(filepath.Join(canonical, filepath.FromSlash(row.Key)))
	} else {
		sk, body, err = service.ReadFull(row.Key)
	}
	if err != nil {
		message := mask(err.Error())
		return registryReadAnswer{Error: &message}
	}
	return registryReadAnswer{Path: mask(sk.Path), Body: mask(body)}
}

// materializeRegistryTree writes every entry, parents before children.
func materializeRegistryTree(directory string, entries []registryEntry) error {
	for _, entry := range entries {
		path := filepath.Join(directory, filepath.FromSlash(entry.Path))
		if entry.Kind != "link" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
		}
		switch entry.Kind {
		case "dir":
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case "link":
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			target := filepath.Join(directory, filepath.FromSlash(entry.Target))
			if err := os.Symlink(target, path); err != nil {
				return fmt.Errorf("link %s: %w", entry.Path, err)
			}
		default:
			if err := os.WriteFile(path, []byte(entry.Data), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

// maskOutcome masks one discovery answer.
func maskOutcome(outcome skill.Outcome, mask func(string) string) registryOutcome {
	return registryOutcome{
		Skills:      maskSkills(outcome.Skills, mask),
		Diagnostics: maskDiagnostics(outcome.Diagnostics, mask),
		Roots:       maskAll(outcome.Roots, mask),
		ScanRoots:   maskAll(outcome.ScanRoots, mask),
	}
}

// maskSkills masks a skill list.
func maskSkills(skills []skill.Metadata, mask func(string) string) []registrySkill {
	out := make([]registrySkill, 0, len(skills))
	for _, sk := range skills {
		out = append(out, registrySkill{
			Name:             sk.Name,
			Description:      mask(sk.Description),
			ShortDescription: mask(sk.ShortDescription),
			Path:             mask(sk.Path),
		})
	}
	return out
}

// maskDiagnostics masks a diagnostic list.
func maskDiagnostics(diagnostics []skill.Diagnostic, mask func(string) string) []registryDiagnostic {
	out := make([]registryDiagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		out = append(out, registryDiagnostic{
			Path:    mask(diagnostic.Path),
			Message: mask(diagnostic.Message),
			Warning: diagnostic.Warning,
		})
	}
	return out
}

// maskAll masks a string list.
func maskAll(values []string, mask func(string) string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, mask(value))
	}
	return out
}

// maskRegistryText applies the masks the document lists: the harness's
// directory (canonical form first), then an operating system's sentence, then a
// YAML library's error text.
func maskRegistryText(text, directory, canonical string) string {
	for _, prefix := range []string{canonical, directory} {
		if prefix == "" {
			continue
		}
		text = strings.ReplaceAll(text, prefix+string(filepath.Separator), "")
		if text == prefix {
			text = "."
		}
	}
	if strings.HasPrefix(text, "resolve symlink: ") {
		// The operating system's sentence, and the path it names, are the
		// platform's: a dangling link is worded `lstat <path>: <sentence>` by
		// Go and something else by any other runtime. What core decided is that
		// it is an error diagnostic and not a skill.
		return "resolve symlink: <os error>"
	}
	for _, verb := range []string{"open ", "stat ", "lstat ", "read ", "readdirent ", "readlink "} {
		if rest, ok := strings.CutPrefix(text, verb); ok {
			if at := strings.Index(rest, ": "); at >= 0 {
				return verb + rest[:at+2] + "<os error>"
			}
		}
	}
	const prefix = "skill: parse frontmatter: "
	if strings.HasPrefix(text, prefix) {
		if strings.HasPrefix(text, prefix+"YAML key") {
			return text
		}
		return prefix + "<decode error>"
	}
	return text
}

// skillBody is a well-formed SKILL.md, so the rows stay about topology.
func skillBody(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name +
		"\n\nInstructions for " + name + ".\n"
}

// registryTrees is the table.
func registryTrees() []registryTree {
	// The one tree four rows share: two roots, a duplicate name across them,
	// and a nested directory, so every ordering rule has something to order.
	shared := []registryEntry{
		{Path: "a/alpha/SKILL.md", Data: skillBody("alpha", "review a change for defects")},
		{Path: "a/nested/beta/SKILL.md", Data: skillBody("beta", "run the test suite")},
		{Path: "a/dup/SKILL.md", Data: skillBody("dup", "duplicate in root a")},
		{Path: "b/dup/SKILL.md", Data: skillBody("dup", "duplicate in root b")},
	}
	sharedRoots := []string{"a", "b"}

	return []registryTree{
		{
			Name:    "roots-and-duplicates",
			Why:     "two roots are walked, a duplicate name is listed once per path, and the canonical (name, path) order decides which one ByName resolves",
			Entries: shared,
			Roots:   sharedRoots,
			Service: &registryService{
				Mentions: []registryMention{
					{Text: "please $alpha", Answer: nil},
					{Text: "$dup", Answer: nil},
				},
				Reads: []registryRead{
					{By: "name", Key: "alpha"},
					{By: "path", Key: "a/nested/beta/SKILL.md"},
					{By: "name", Key: "missing"},
					{By: "path", Key: "a/hidden/SKILL.md"},
				},
			},
		},
		{
			Name:    "missing-root",
			Why:     "a configured root that does not exist is silently empty: no skill, no diagnostic, and it is still listed in ScanRoots",
			Entries: nil,
			Roots:   []string{"does-not-exist", "a"},
			Service: &registryService{},
		},
		{
			Name: "hidden-and-shapes",
			Why:  "hidden entries are skipped at every level, a non-SKILL.md file is not a skill, and a directory named SKILL.md is walked rather than read",
			Entries: []registryEntry{
				{Path: "a/visible/SKILL.md", Data: skillBody("visible", "a visible skill")},
				{Path: "a/.hidden/SKILL.md", Data: skillBody("hidden", "must not be discovered")},
				{Path: "a/.hidden-file.md", Data: "not a skill"},
				{Path: "a/notes.md", Data: "not a skill either"},
				{Path: "a/SKILL.md/inside/SKILL.md", Data: skillBody("deep", "inside a directory called SKILL.md")},
			},
			Roots:   []string{"a"},
			Service: &registryService{},
		},
		{
			Name: "symlink-alias-cycle",
			Why:  "an alias pointing back at a walked tree is not re-descended: directories are tracked by canonical path, so the walk stays finite and the skill is collected once",
			Entries: []registryEntry{
				{Path: "a/real/SKILL.md", Data: skillBody("real", "the only real skill")},
				{Path: "a/alias-one", Kind: "link", Target: "a"},
				{Path: "a/alias-two", Kind: "link", Target: "a"},
			},
			Roots:   []string{"a"},
			Service: &registryService{},
		},
		{
			Name: "symlink-inside-another-root",
			Why:  "a symlink whose target lives inside another configured root is followed, and the skill is collected once under its canonical path",
			Entries: []registryEntry{
				{Path: "a/linked", Kind: "link", Target: "b/linked"},
				{Path: "b/linked/SKILL.md", Data: skillBody("linked", "reached through a symlink")},
			},
			Roots:   []string{"a", "b"},
			Service: &registryService{},
		},
		{
			Name: "symlink-escape",
			Why:  "a file symlink and a directory symlink that both leave the configured roots are refused with an error diagnostic and never yield a skill",
			Entries: []registryEntry{
				{Path: "outside/leak/SKILL.md", Data: skillBody("leak", "must not be reachable")},
				{Path: "a/SKILL.md", Kind: "link", Target: "outside/leak/SKILL.md"},
				{Path: "a/leak-dir", Kind: "link", Target: "outside/leak"},
			},
			Roots:   []string{"a"},
			Service: &registryService{},
		},
		{
			Name: "symlink-dangling",
			Why:  "a dangling symlink is an error diagnostic, not a silent skip: a walk that cannot follow a link must say so",
			Entries: []registryEntry{
				{Path: "a/real/SKILL.md", Data: skillBody("real", "a real skill")},
				{Path: "a/dangling", Kind: "link", Target: "nowhere"},
			},
			Roots:   []string{"a"},
			Service: &registryService{},
		},
		{
			Name: "repeated-roots",
			Why:  "the same root configured twice, plus an alias resolving to it, is walked and listed once, while ScanRoots keeps every configured entry",
			Entries: []registryEntry{
				{Path: "a/solo/SKILL.md", Data: skillBody("solo", "one skill")},
				{Path: "alias", Kind: "link", Target: "a"},
			},
			Roots:   []string{"a", "a", "alias"},
			Service: &registryService{},
		},
		{
			Name: "diagnostics-kind",
			Why:  "a name that disagrees with its directory is a warning while an unparsable SKILL.md is an error: discovery marks the tolerated shape issue, not the failure",
			Entries: []registryEntry{
				{Path: "a/mismatch/SKILL.md", Data: skillBody("something-else", "tolerated mismatch")},
				{Path: "a/broken/SKILL.md", Data: "---\nname: broken\n---\n\nbody\n"},
			},
			Roots:   []string{"a"},
			Service: &registryService{},
		},
		{
			Name:     "disabled",
			Why:      "a disabled name and a disabled absolute path are both excluded from List and from ranking, while discovery still sees them",
			Entries:  shared,
			Roots:    sharedRoots,
			Disabled: []registryDisable{{Kind: "name", Value: "alpha"}, {Kind: "path", Value: "b/dup/SKILL.md"}},
			Service: &registryService{
				Rankings: []registryRanking{
					{Query: "duplicate", TopN: 0, MinScore: 0},
					{Query: "review defects", TopN: 0, MinScore: 0},
				},
			},
		},
		{
			Name:    "ranking",
			Why:     "the ranked list is capped by the call's limit, filtered by the call's threshold, stable for equal scores, and empty for an empty or unmatched query",
			Entries: shared,
			Roots:   sharedRoots,
			Service: &registryService{
				Rankings: []registryRanking{
					{Query: "duplicate", TopN: 0, MinScore: 0},
					{Query: "duplicate", TopN: 1, MinScore: 0},
					{Query: "duplicate", TopN: 1, MinScore: 0.9},
					{Query: "test suite", TopN: 0, MinScore: 0},
					{Query: "", TopN: 0, MinScore: 0},
					{Query: "   ", TopN: 0, MinScore: 0},
					{Query: "kubernetes", TopN: 0, MinScore: 0},
					{Query: "review", TopN: 2, MinScore: 0},
				},
			},
		},
		{
			Name:     "ranking-defaults",
			Why:      "a service configured with TopN and MinScore applies both when the call passes zero, and a call's own limit wins over the configured one",
			Entries:  shared,
			Roots:    sharedRoots,
			TopN:     2,
			MinScore: 0.1,
			Service: &registryService{
				Rankings: []registryRanking{
					{Query: "duplicate", TopN: 0, MinScore: 0},
					{Query: "duplicate", TopN: 1, MinScore: 0},
					{Query: "review", TopN: 0, MinScore: 0},
				},
			},
		},
		{
			Name: "mentions",
			Why:  "the mention grammar: a mention needs a non-word character before the $, the character is consumed by the match, an upper-case or dotted name is not a mention, and an unknown name resolves to nothing",
			Entries: []registryEntry{
				{Path: "a/alpha/SKILL.md", Data: skillBody("alpha", "the alpha skill")},
				{Path: "a/beta/SKILL.md", Data: skillBody("beta", "the beta skill")},
				{Path: "a/alpha-beta/SKILL.md", Data: skillBody("alpha-beta", "the hyphenated skill")},
			},
			Roots: []string{"a"},
			Service: &registryService{
				Mentions: []registryMention{
					{Text: "$alpha", Answer: nil},
					{Text: "a$alpha", Answer: nil},
					{Text: "$500", Answer: nil},
					{Text: "$alpha$beta", Answer: nil},
					{Text: "$beta then $alpha then $beta", Answer: nil},
					{Text: "$Alpha and $alpha.beta", Answer: nil},
					{Text: "$alpha-beta and $alpha", Answer: nil},
					{Text: "unknown $missing and $alpha", Answer: nil},
					{Text: "no mentions at all", Answer: nil},
				},
			},
		},
		{
			Name: "body-read",
			Why:  "a body read strips the frontmatter and trims the remainder; a path outside the snapshot is not found even when a skill with that name exists",
			Entries: []registryEntry{
				{Path: "a/plain/SKILL.md", Data: "---\nname: plain\ndescription: a plain skill\n---\n\n\n  # Plain\n\nBody text.\n\n"},
			},
			Roots: []string{"a"},
			Service: &registryService{
				Reads: []registryRead{
					{By: "name", Key: "plain"},
					{By: "path", Key: "a/plain/SKILL.md"},
					{By: "path", Key: "a/absent/SKILL.md"},
				},
			},
		},
		{
			Name: "reload-sees-new-skills",
			Why:  "Reload re-runs discovery, so a skill written after construction becomes visible without rebuilding the service",
			Entries: []registryEntry{
				{Path: "a/first/SKILL.md", Data: skillBody("first", "the skill of the first pass")},
			},
			Roots: []string{"a"},
			Service: &registryService{
				Reload: &registryReload{
					Add: registryEntry{Path: "a/second/SKILL.md", Data: skillBody("second", "written between the passes")},
				},
			},
		},
	}
}

// registryRenders is the per-turn section table: no files, only metadata.
func registryRenders() []registryRender {
	short := registrySkill{Name: "alpha", Description: "review a change for defects", Path: "/skills/alpha/SKILL.md"}
	second := registrySkill{Name: "beta", Description: "run the test suite", Path: "/skills/beta/SKILL.md"}
	long := registrySkill{
		Name:        "long",
		Description: strings.Repeat("中", 1030),
		Path:        "/skills/long/SKILL.md",
	}
	rows := []registryRender{
		{
			Name:   "empty",
			Why:    "an empty list renders the empty string, so the caller can skip the section entirely",
			Skills: nil,
			Answer: "",
		},
		{
			Name:   "one",
			Why:    "one line per skill: the name, the description and the file path, with no trailing newline",
			Skills: []registrySkill{short},
			Answer: "",
		},
		{
			Name:   "two",
			Why:    "two lines keep the order they were given",
			Skills: []registrySkill{short, second},
			Answer: "",
		},
		{
			Name:   "description-truncated",
			Why:    "a description past the rune cap is truncated in runes at render time, which is where a caller that built Metadata by hand reaches",
			Skills: []registrySkill{long},
			Answer: "",
		},
	}
	for index := range rows {
		if rows[index].Skills == nil {
			rows[index].Skills = []registrySkill{}
		}
		rows[index].Answer = skill.RenderSection(metadataOf(rows[index].Skills))
	}
	return rows
}

// metadataOf converts a row's skills back to core's Metadata.
func metadataOf(skills []registrySkill) []skill.Metadata {
	out := make([]skill.Metadata, 0, len(skills))
	for _, sk := range skills {
		out = append(out, skill.Metadata{
			Name:             sk.Name,
			Description:      sk.Description,
			ShortDescription: sk.ShortDescription,
			Path:             sk.Path,
		})
	}
	return out
}
