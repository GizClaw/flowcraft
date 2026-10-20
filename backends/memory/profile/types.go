package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const (
	// DefaultMaxTokens bounds the rendered block. The unit is the module's one
	// cost model (pack.EstimatedTokens), so the block's bound and the request
	// budget that carries it are counted the same way.
	DefaultMaxTokens = 512
	// DefaultMaxEntries bounds how many lines one block may hold.
	DefaultMaxEntries = 32
	// DefaultDedupeThreshold is the one-says-the-other overlap above which a
	// distilled candidate restates a line the block already holds, so the
	// lower-ranked one is dropped instead of spending the bound twice.
	DefaultDedupeThreshold = 0.8
	// schemaVersion stamps the stored document shape.
	schemaVersion = 1
	// Entry origins.
	OriginExplicit  = "explicit"
	OriginDistilled = "distilled"
	// entryIDDomain separates entry ids from every other content hash in the
	// module; producerDomain does the same for the policy stamp.
	entryIDDomain  = "flowcraft.memory.profile-entry\x00v1\x00"
	producerDomain = "flowcraft.memory.profile-policy\x00v1\x00"
)

// Entry is one line of a block: the text that is served, the writer that put it
// there, and -- for a distilled line -- the facts it was judged from.
type Entry struct {
	// ID is content-derived (EntryID), so the same line keeps its identity
	// across rewrites and a host can name one line without holding a cursor.
	ID string `json:"id"`
	// Text is the line as it is served. It is a fact's own text, verbatim: the
	// block selects and orders, it never paraphrases.
	Text string `json:"text"`
	// Origin names the writer: OriginExplicit for a line the host wrote,
	// OriginDistilled for one a pass judged.
	Origin string `json:"origin"`
	// Provenance names the facts a distilled line was judged from, sorted.
	// An explicit line has none: its author is the host, not the corpus.
	Provenance []string `json:"provenance,omitempty"`
	// UpdatedAt is when the line was last written into the block.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Document is one conversation's stored block: the two halves, the policy the
// distilled half was judged under, and when the document last changed.
type Document struct {
	SchemaVersion int       `json:"schema_version"`
	Producer      string    `json:"producer,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
	// Explicit is the host's half, in the order the host wrote it.
	Explicit []Entry `json:"explicit,omitempty"`
	// Distilled is the pass's half, in the order the pass ranked it.
	Distilled []Entry `json:"distilled,omitempty"`
}

// Class is one profile class: the shapes that make a fact a statement of it.
// A class is matched against the fact's own text, not against metadata: the
// extractor's default strategy writes no predicate, so a rule that needs one
// would judge almost nothing (see DefaultClasses for what the corpus showed).
type Class struct {
	// Name labels the class ("preference"), and its position in the list is
	// its priority: the block keeps the earlier classes when the bound binds.
	Name string
	// Phrases are folded phrases the fact's subject opens with ("loves",
	// "works as", "plans to"). Matching is token-prefix: the phrase has to
	// start right after the subject, optionally across a bounded run of
	// reporting words ("said", "she", "her"), which is how a paraphrase of a
	// quoted statement reads.
	Phrases []string
	// Copula also accepts the subject followed by "is a/an <word>": the
	// identity shape a phrase list cannot enumerate ("Caroline is an artist",
	// "Melanie is a parent").
	Copula bool
}

// Config bounds one block and names the policy that fills its distilled half.
type Config struct {
	MaxTokens  int
	MaxEntries int
	// Distill turns the pass's half on. While it is off the store serves no
	// distilled entry, whatever a document holds, and the pass's own repair is
	// to write an empty half.
	Distill bool
	// Classes is the vocabulary. Nil is DefaultClasses; an empty (non-nil)
	// slice turns the distilled half off without turning Distill off, which is
	// the shape a deployment with no facts in the profile's languages wants.
	Classes []Class
	// DedupeThreshold is the restatement threshold (default
	// DefaultDedupeThreshold): the one-says-the-other overlap above which a
	// candidate line is dropped because the block already says it.
	DedupeThreshold float64
}

func (config Config) withDefaults() Config {
	if config.MaxTokens == 0 {
		config.MaxTokens = DefaultMaxTokens
	}
	if config.MaxEntries == 0 {
		config.MaxEntries = DefaultMaxEntries
	}
	if config.Classes == nil {
		config.Classes = DefaultClasses()
	}
	if config.DedupeThreshold == 0 {
		config.DedupeThreshold = DefaultDedupeThreshold
	}
	return config
}

func (config Config) validate() error {
	if config.MaxTokens < 1 {
		return fmt.Errorf("profile: max_tokens must be positive, got %d", config.MaxTokens)
	}
	if config.MaxEntries < 1 {
		return fmt.Errorf("profile: max_entries must be positive, got %d", config.MaxEntries)
	}
	if config.DedupeThreshold < 0 || config.DedupeThreshold > 1 {
		return fmt.Errorf("profile: dedupe threshold must be in [0,1], got %v", config.DedupeThreshold)
	}
	return validateClasses(config.Classes)
}

// ProducerVersion digests the distillation policy: the rule version, the caps
// that shape the pass's half, the dedupe threshold and the vocabulary. It is
// what a stored distilled half is served against, so a block judged under
// another policy reads as stale rather than as this policy's judgment.
func (config Config) ProducerVersion() string {
	var builder strings.Builder
	builder.WriteString(producerDomain)
	fmt.Fprintf(&builder, "\nmax_tokens=%d\nmax_entries=%d\ndedupe=%.6f\n", config.MaxTokens, config.MaxEntries, config.DedupeThreshold)
	for _, class := range config.Classes {
		fmt.Fprintf(&builder, "class=%q copula=%t\n", class.Name, class.Copula)
		for _, phrase := range class.Phrases {
			fmt.Fprintf(&builder, "phrase=%q\n", phrase)
		}
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return "profile-" + hex.EncodeToString(sum[:6])
}

// EntryID is the content-derived identity of one line: the same text is the
// same entry in every block that holds it.
func EntryID(text string) string {
	sum := sha256.Sum256([]byte(entryIDDomain + text))
	return "profile-entry-" + hex.EncodeToString(sum[:8])
}

// Block is one conversation's block as the read path serves it.
type Block struct {
	// Text is the rendered block: one "- " line per entry, in served order.
	Text string
	// Entries counts the served lines.
	Entries int
	// Sources names the served lines' provenance as read-path source
	// references, so the context item that carries the block carries where it
	// came from.
	Sources []corememory.SourceRef
	// Truncated reports a served block that had to be cut to fit the store's
	// bound: a document written under a larger cap, served under this one.
	Truncated bool
}

// SaveResult reports what one write left in the block.
type SaveResult struct {
	Document Document
	// Dropped counts the distilled lines a write removed to keep the block
	// within its bound. An explicit write makes room for the host's lines by
	// dropping the pass's lowest-ranked ones; the next pass re-judges what is
	// left.
	Dropped int
}

// Render writes the entries as the block's text: one line per entry, each
// prefixed with "- ", in the order given. The prefix is what makes a multi-line
// entry's boundaries visible: fact text can carry newlines, and a block whose
// lines cannot be told apart cannot be read as a list.
func Render(entries []Entry) string {
	var builder strings.Builder
	for index, entry := range entries {
		if index > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString("- ")
		builder.WriteString(strings.Join(strings.Fields(entry.Text), " "))
	}
	return builder.String()
}

// entrySources renders one entry's provenance as read-path source references:
// an external source named by the entry, with the facts behind a distilled line
// in the locator. The block is not a message and not a document, so the source
// kind is the module's escape hatch rather than a lie about where it came from.
func entrySources(conversationID string, entry Entry) corememory.SourceRef {
	source := corememory.SourceRef{
		Kind: corememory.SourceExternal,
		ID:   "profile/" + conversationID + "/" + entry.ID,
	}
	if len(entry.Provenance) > 0 {
		source.Locator = "facts:" + strings.Join(entry.Provenance, ",")
	}
	return source
}

// entriesTokens is the estimated cost of the entries as served.
func entriesTokens(entries []Entry) int {
	total := 0
	for _, entry := range entries {
		total += pack.EstimatedTokens(entry.Text)
	}
	return total
}

func validateClasses(classes []Class) error {
	seen := make(map[string]struct{}, len(classes))
	for index, class := range classes {
		name := strings.TrimSpace(class.Name)
		if name == "" {
			return fmt.Errorf("profile: classes[%d] has no name", index)
		}
		if name != class.Name {
			return fmt.Errorf("profile: class %q carries surrounding whitespace", class.Name)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("profile: class %q is declared twice", name)
		}
		seen[name] = struct{}{}
		if len(class.Phrases) == 0 && !class.Copula {
			return fmt.Errorf("profile: class %q has neither phrases nor the copula shape", name)
		}
		for phraseIndex, phrase := range class.Phrases {
			if strings.TrimSpace(phrase) == "" {
				return fmt.Errorf("profile: class %q phrase %d is empty", name, phraseIndex)
			}
			if foldToken(phrase) != phrase {
				return fmt.Errorf("profile: class %q phrase %q is not folded (lowercase, single spaces)", name, phrase)
			}
		}
	}
	return nil
}

func validateEntry(entry Entry) error {
	if strings.TrimSpace(entry.Text) == "" {
		return errors.New("profile: entry text is required")
	}
	if entry.Origin != OriginExplicit && entry.Origin != OriginDistilled {
		return fmt.Errorf("profile: entry %q has unknown origin %q", entry.ID, entry.Origin)
	}
	if entry.ID != EntryID(entry.Text) {
		return fmt.Errorf("profile: entry id does not match its text (%q)", entry.Text)
	}
	if len(entry.Provenance) > 0 {
		if !sort.StringsAreSorted(entry.Provenance) {
			return fmt.Errorf("profile: entry %q provenance must be sorted", entry.ID)
		}
		for index, fact := range entry.Provenance {
			if strings.TrimSpace(fact) == "" {
				return fmt.Errorf("profile: entry %q provenance %d is empty", entry.ID, index)
			}
			if index > 0 && entry.Provenance[index-1] == fact {
				return fmt.Errorf("profile: entry %q provenance must be unique", entry.ID)
			}
		}
	}
	if entry.Origin == OriginExplicit && len(entry.Provenance) > 0 {
		return fmt.Errorf("profile: explicit entry %q must not carry fact provenance", entry.ID)
	}
	return nil
}
