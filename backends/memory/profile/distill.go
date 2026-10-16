package profile

import (
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/backends/memory/internal/textsim"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
)

// maxScaffoldSkip bounds how many reporting words may sit between a fact's
// subject and the statement it reports. A paraphrase of a quoted statement
// reads "Caroline said she loves pottery", so the distance is small when the
// fact states something about its subject and large when it narrates a turn.
const maxScaffoldSkip = 8

// scaffoldWords are the words a paraphrase may put between the subject and the
// statement: the reporting verb, the pronoun it introduces, the possessive of a
// nominalized statement ("her goal is ..."), and the adverbs a paraphrase adds.
// Skipping them is what lets a reported statement read as a statement.
const scaffoldWords = `said says saying shared shares sharing expressed expresses expressing
mentioned mentions mentioning explained explains explaining told tells telling
stated states stating noted notes noting added adds adding described describes
describing reported reports reporting that she he they her his their also still
now currently s`

var scaffold = func() map[string]struct{} {
	words := make(map[string]struct{})
	for _, word := range strings.Fields(scaffoldWords) {
		words[word] = struct{}{}
	}
	return words
}()

// DefaultClasses is the vocabulary this module measured on its own corpus: the
// classes a profile line belongs to, in the order a bound-bounded block keeps
// them.
//
// The shapes are deliberate about what they leave out. A preference is a
// third-person verb ("loves", "enjoys"), not the bare noun: "love is truly
// magical" is a remark, not a preference, and the bare form is how a
// paraphrase writes one. The identity shape is the copula ("is an artist")
// because a phrase list cannot enumerate what a person can be. Events are
// absent on purpose: "attended", "shared", "took" describe a turn, not a
// person, and a class that admits them turns the block back into a second
// retrieval pool.
//
// It is measured, not guessed: over this project's own 3828-fact corpus the
// vocabulary matched 240 facts (6.3%, 17-38 per conversation), and what it
// matched reads as a profile -- what each participant loves, plays, works as,
// and is working toward.
func DefaultClasses() []Class {
	return []Class{
		{
			Name: "preference",
			Phrases: []string{
				"loves", "likes", "prefers", "enjoys", "adores", "hates", "dislikes",
				"is a fan of", "is passionate about", "cares about",
			},
		},
		{
			Name: "identity",
			Phrases: []string{
				"works as", "works at", "works for", "lives in", "lived in", "is from",
				"grew up in", "volunteers at", "volunteers for", "is married to",
			},
			Copula: true,
		},
		{
			Name: "habit",
			Phrases: []string{
				"plays", "collects", "practices", "reads", "paints", "writes", "cooks",
				"travels", "runs", "swims", "dances", "sings", "gardens", "has been",
			},
		},
		{
			Name: "goal",
			Phrases: []string{
				"wants to", "hopes to", "plans to", "dreams of", "aspires to",
				"intends to", "wishes to", "is determined to", "goal is", "dream is",
			},
		},
	}
}

// Classify reports the class one fact states about its subject, or none.
//
// A fact states a profile line when it names exactly one entity -- a sentence
// naming two people is a remark about a relation or an event, not a line about
// one of them -- that entity is where the fact's subject sits, and what follows
// it is one of the class shapes, across at most maxScaffoldSkip reporting
// words. Everything else is not judged: a missed line costs the block nothing
// the retrieval lanes do not already serve, while a wrong one spends the bound
// on a statement about a turn.
func Classify(text string, entities []string, classes []Class) (string, bool) {
	if len(classes) == 0 || len(entities) != 1 {
		return "", false
	}
	subject := foldTokens(entities[0])
	if len(subject) == 0 {
		return "", false
	}
	tokens := foldTokens(foldText(text))
	position := -1
	for index := 0; index+len(subject) <= len(tokens); index++ {
		if sameTokens(tokens[index:index+len(subject)], subject) {
			position = index
			break
		}
	}
	if position < 0 {
		return "", false
	}
	rest := tokens[position+len(subject):]
	skipped := 0
	for skipped < len(rest) && skipped < maxScaffoldSkip {
		if _, ok := scaffold[rest[skipped]]; !ok {
			break
		}
		skipped++
	}
	rest = rest[skipped:]
	if len(rest) == 0 {
		return "", false
	}
	for _, class := range classes {
		if class.Copula && copulaShape(rest) {
			return class.Name, true
		}
		if opensWithPhrase(rest, class.Phrases) {
			return class.Name, true
		}
	}
	return "", false
}

// Distill returns the pass's half of one conversation's block: the facts that
// state something stable about the conversation's participants, ranked by class
// (the class order is the priority), then by recency, and cut to the bound.
//
// superseded names the facts a passing maintenance judgment replaced -- the
// older side of a supersede finding. They are not candidates: the block states
// what holds now, and the fact that was replaced is the state before.
//
// The rule never paraphrases: an entry is a fact's own text, so the block can be
// read against the corpus it came from, and an entry that a later pass drops
// leaves the fact where it was.
func Distill(facts []factview.Fact, superseded map[string]struct{}, config Config) []Entry {
	config = config.withDefaults()
	if len(config.Classes) == 0 {
		return nil
	}
	type candidate struct {
		fact  factview.Fact
		class int
	}
	candidates := make([]candidate, 0, len(facts))
	for _, fact := range facts {
		if _, replaced := superseded[fact.ID]; replaced {
			continue
		}
		name, ok := Classify(fact.Text, fact.Entities, config.Classes)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{fact: fact, class: classIndex(config.Classes, name)})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].class != candidates[j].class {
			return candidates[i].class < candidates[j].class
		}
		left, right := recencyKey(candidates[i].fact), recencyKey(candidates[j].fact)
		if !left.Equal(right) {
			return left.After(right)
		}
		return candidates[i].fact.ID < candidates[j].fact.ID
	})
	entries := make([]Entry, 0, min(config.MaxEntries, len(candidates)))
	selected := make([]map[string]struct{}, 0, len(candidates))
	used := 0
	for _, item := range candidates {
		if len(entries) >= config.MaxEntries {
			break
		}
		text := foldText(item.fact.Text)
		if text == "" {
			continue
		}
		cost := pack.EstimatedTokens(text)
		if used+cost > config.MaxTokens {
			// A line that does not fit is skipped rather than ending the
			// pass: the next-best line that fits is worth the bound more than
			// the room it leaves empty.
			continue
		}
		set := textsim.Set(text)
		if restates(set, selected, config.DedupeThreshold) {
			continue
		}
		selected = append(selected, set)
		used += cost
		entries = append(entries, Entry{
			ID:         EntryID(text),
			Text:       text,
			Origin:     OriginDistilled,
			Provenance: []string{item.fact.ID},
		})
	}
	return entries
}

// restates reports whether one candidate line says what a line the block
// already holds says. The test is one-directional containment in both
// directions: a restatement that adds a trailing clause is not very similar by
// Jaccard, but it is fully contained in the longer line, and it is still one
// claim spending the bound twice.
func restates(candidate map[string]struct{}, selected []map[string]struct{}, threshold float64) bool {
	for _, held := range selected {
		if textsim.ContainmentOf(candidate, held) >= threshold ||
			textsim.ContainmentOf(held, candidate) >= threshold {
			return true
		}
	}
	return false
}

// recencyKey is the moment a fact is dated at, which is what the ranking orders
// a class's candidates by: the event time the deriver resolved, or the time the
// fact was written when the turn stated none.
func recencyKey(fact factview.Fact) time.Time {
	if !fact.EventTime.IsZero() {
		return fact.EventTime.UTC()
	}
	return fact.CreatedAt.UTC()
}

func classIndex(classes []Class, name string) int {
	for index, class := range classes {
		if class.Name == name {
			return index
		}
	}
	return len(classes)
}

func copulaShape(rest []string) bool {
	if len(rest) < 3 {
		return false
	}
	if rest[0] != "is" && rest[0] != "was" {
		return false
	}
	if rest[1] != "a" && rest[1] != "an" {
		return false
	}
	return isWord(rest[2])
}

func opensWithPhrase(rest []string, phrases []string) bool {
	for _, phrase := range phrases {
		tokens := foldTokens(phrase)
		if len(tokens) == 0 || len(rest) < len(tokens) {
			continue
		}
		if sameTokens(rest[:len(tokens)], tokens) {
			return true
		}
	}
	return false
}

func sameTokens(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func isWord(token string) bool {
	for _, r := range token {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return token != ""
}

// foldText applies the module's text normalization: NFKC and deterministic
// whitespace folding, case preserved. Facts are stored normalized, so this is
// what makes a host's line and a distilled line the same shape.
func foldText(text string) string {
	return factview.NormalizeText(text)
}

// foldToken folds one vocabulary phrase: lowercase, single spaces. It is what
// validateClasses holds a phrase to.
func foldToken(phrase string) string {
	return strings.ToLower(strings.Join(strings.Fields(phrase), " "))
}

// foldTokens splits folded text into the tokens the rule matches on: runs of
// letters and digits, lowercased. Punctuation and apostrophes separate tokens,
// so "Caroline's" is "caroline" followed by the scaffold word "s".
func foldTokens(text string) []string {
	tokens := make([]string, 0, len(text)/6+1)
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return tokens
}
