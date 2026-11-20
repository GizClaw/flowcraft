package config

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/goccy/go-json"

	"go.yaml.in/yaml/v3"
)

// DecodeFunc turns a merged document into a value.
//
// The document arrives as canonical JSON — key-sorted, with every placeholder
// already resolved and every merge already applied — because that is the form
// the merge produced. It is not a YAML node on purpose: the surface of this
// package never exposes the intermediate representation, so a caller cannot
// depend on the syntax the layers were written in.
type DecodeFunc[T any] func(ctx context.Context, canonical []byte) (T, error)

// Strict is the default decoder: JSON semantics with unknown fields rejected.
//
// Strictness is the point. A configuration is read by a person who expects a
// typo to be reported, not to become a silently absent setting, so the
// canonical form is decoded with DisallowUnknownFields and a trailing document
// is an error.
func Strict[T any](_ context.Context, canonical []byte) (T, error) {
	return decode.Decode[T](canonical)
}

// Option adjusts one load.
type Option func(*options)

// defaultLabel names a document whose caller did not supply a label.
const defaultLabel = "config"

type options struct {
	resolvers  map[string]ResolverFunc
	label      string
	provenance bool
}

func newOptions(opts []Option) options {
	settings := options{label: defaultLabel, provenance: true}
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	return settings
}

// WithLabel sets the name this document is reported under: it prefixes every
// error the load produces. The default is "config".
//
// Options apply in order and the last label wins, which is the only rule there
// is: a caller that wraps another's options decides which name survives. A
// loader that wants its own name on the errors appends its label after the
// caller's options; runtime.LoadLayers and agent-graph/nodes do the opposite, so
// a host can rename what they read. Both are knowable from here, neither is
// guessed at.
func WithLabel(label string) Option {
	return func(o *options) {
		if label != "" {
			o.label = label
		}
	}
}

// WithProvenance records which layer supplied each value. It is the default, and
// it exists so a caller can put it back after a set of options that disabled it:
// an option is a function over an unexported struct, so without this an outer
// caller could not undo another one's choice.
func WithProvenance() Option { return func(o *options) { o.provenance = true } }

// WithoutProvenance skips recording which layer supplied each value.
//
// Provenance is the answer to "which file put this value here", and it costs
// about a tenth of a load: one entry per merged leaf. A caller that only wants
// the value — a runtime built from a configuration, a host that never reports
// origins — should not pay for the report, so it says so here. The resulting
// [Doc] then reports an empty [Provenance]: Origin answers false for every path.
func WithoutProvenance() Option { return func(o *options) { o.provenance = false } }

// Doc is one merged document.
//
// It exists because decoding is not the only thing a caller wants from a stack:
// a host that reports its effective configuration needs the value at a path, and
// an operator asking "why is this list empty" needs the layer that set it. Both
// are answered from the merged tree rather than by re-reading the inputs, which
// would answer from a different merge than the one that ran.
type Doc struct {
	label string
	root  *yaml.Node
	prov  Provenance
}

// Label is the name this document is reported under.
func (d *Doc) Label() string { return d.label }

// Provenance reports which layer supplied each merged value.
func (d *Doc) Provenance() Provenance { return d.prov }

// Origin returns the layer that last set the value at path.
func (d *Doc) Origin(path ...string) (Ref, bool) { return d.prov.Origin(path...) }

// Raw returns the merged value at path, or false when the document has none.
//
// The value is what the YAML decodes to in Go: strings, numbers, bools,
// nested maps and slices. Traversal is by mapping key only, because a sequence
// is one value under the merge rules; its items are read through the value.
func (d *Doc) Raw(path ...string) (any, bool) {
	node, found := lookup(d.root, path)
	if !found {
		return nil, false
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

// Canonical returns the merged document as canonical JSON: key-sorted, every
// placeholder resolved, every layer applied.
//
// It is the document's identity, not the value's: two stacks that differ only
// in formatting or in the order a layer was written produce the same bytes.
// The digest of a decoded value is a separate question, because a value carries
// defaults the document never wrote.
func (d *Doc) Canonical() ([]byte, error) {
	if canonical, ok := canonicalFromNode(d.root); ok {
		return canonical, nil
	}
	return d.canonicalViaYAML()
}

// canonicalFromNode renders the merged tree through the YAML decoder's own value
// model and straight into JSON. The tree is already parsed, so re-encoding it as
// YAML text and parsing that text again would do the same work twice: measured
// on a four-layer, 256-key stack this is the difference between 840µs and 90µs.
//
// It reports false when the document needs the exact path — a mapping key that
// is not a string, a scalar JSON cannot carry, or a scalar the two YAML libraries
// resolve differently. Those documents are the ones where the conversion's own
// sentence ("YAML key !!null null cannot be spelled as a JSON key") is worth a
// round trip, because that sentence is what a reader sees when their document is
// wrong.
func canonicalFromNode(root *yaml.Node) ([]byte, bool) {
	if !sameMeaningInBothLibraries(root) {
		return nil, false
	}
	var value any
	if err := root.Decode(&value); err != nil {
		return nil, false
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return canonical, true
}

// yaml11Booleans is the closed set of spellings YAML 1.1 resolves to a boolean
// and YAML 1.2 does not.
//
// The two paths this package can take disagree about them: the merged tree is
// parsed by yaml.v3, which keeps `yes` a string, while the exact path's yaml.v2
// turns it into true — as a value (`enabled: yes`) and as a key (`n: 1` becomes
// `"false": 1`). A document holding one therefore has two meanings, so the fast
// path steps aside instead of picking one. true/True/TRUE and their false
// counterparts are absent on purpose: both libraries resolve those to booleans.
var yaml11Booleans = map[string]struct{}{
	"y":   {},
	"yes": {},
	"n":   {},
	"no":  {},
	"on":  {},
	"off": {},
}

// sameMeaningInBothLibraries reports whether every scalar in the tree resolves to
// the same value under yaml.v3 (the fast path) and yaml.v2 (the exact path).
//
// Only a plain string scalar can differ. A quoted `"yes"` is a string to both,
// so it keeps the fast path; every other tag resolves identically in the two
// libraries for the values a configuration holds — probed against octal, hex,
// binary, underscored and sexagesimal integers, and floats.
func sameMeaningInBothLibraries(node *yaml.Node) bool {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if !sameMeaningInBothLibraries(child) {
				return false
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if !sameMeaningInBothLibraries(node.Content[i]) ||
				!sameMeaningInBothLibraries(node.Content[i+1]) {
				return false
			}
		}
	case yaml.ScalarNode:
		if node.Tag != "!!str" || node.Style != 0 {
			return true
		}
		_, diverges := yaml11Booleans[strings.ToLower(node.Value)]
		return !diverges
	}
	return true
}

// canonicalTree returns a copy of the merged tree with collection presentation
// normalised.
//
// The merged tree keeps the style its document was written in, and a layer
// written as JSON arrives as a flow mapping: yaml.Marshal then emits text that
// starts with "{", which decode.ToJSON sniffs as JSON and passes through
// unsorted. Canonical bytes must not depend on how a layer was written, so the
// copy is block-styled before it is encoded. Scalar styles are left alone on
// purpose — a quoted "yes" is a string to both libraries, and unquoting it would
// turn it into a boolean on the way back.
func canonicalTree(node *yaml.Node) *yaml.Node {
	cloned := cloneNode(node)
	normaliseCollectionStyle(cloned)
	return cloned
}

func normaliseCollectionStyle(node *yaml.Node) {
	if node.Kind == yaml.MappingNode || node.Kind == yaml.SequenceNode {
		node.Style = 0
	}
	for _, child := range node.Content {
		normaliseCollectionStyle(child)
	}
}

// canonicalViaYAML is the exact path: encode the tree as YAML text, then run the
// strict conversion the rest of the module uses. It is what spells a non-string
// mapping key into a JSON key and what names a key that cannot be spelled.
func (d *Doc) canonicalViaYAML() ([]byte, error) {
	body, err := yaml.Marshal(canonicalTree(d.root))
	if err != nil {
		return nil, errdefs.Validationf("%s: re-encode merged document: %v", d.label, err)
	}
	// The document-start marker keeps the conversion on its YAML branch: ToJSON
	// sniffs the first byte, so an emitted empty collection ("{}") would be read
	// as JSON and passed through as written — unsorted, and with a trailing
	// newline the fast path does not produce. Trimming the text instead would
	// eat the final newline of a literal block scalar, which is content.
	body = append([]byte("---\n"), body...)
	// Going through ToJSON rather than marshalling the node directly is what
	// rejects a duplicate mapping key and a key the conversion cannot spell:
	// the merge would otherwise take the last duplicate in silence.
	canonical, err := decode.ToJSON(body)
	if err != nil {
		return nil, errdefs.Validationf("%s: %v", d.label, err)
	}
	return canonical, nil
}

// lookup walks the merged tree by mapping key.
func lookup(node *yaml.Node, path []string) (*yaml.Node, bool) {
	current := documentBody(node)
	for _, segment := range path {
		if current.Kind != yaml.MappingNode {
			return nil, false
		}
		found := false
		for i := 0; i+1 < len(current.Content); i += 2 {
			if current.Content[i].Value != segment {
				continue
			}
			current = current.Content[i+1]
			found = true
			break
		}
		if !found {
			return nil, false
		}
	}
	return current, true
}

// Load parses, merges, expands and decodes a stack of layers.
//
// Placeholders are resolved once, after the merge: a value a higher layer
// overrides is never looked up. That matters because a resolver may read a
// secret store, log an access, or fail — resolving per layer would do that work
// for values nobody ends up using.
//
// A nil decode function means [Strict]. The returned [Doc] is the merged
// document the value was decoded from; a caller that wants only the value
// ignores it.
func Load[T any](ctx context.Context, layers []Layer, decode DecodeFunc[T], opts ...Option) (T, *Doc, error) {
	var zero T
	settings := newOptions(opts)
	if decode == nil {
		decode = Strict[T]
	}
	if len(layers) == 0 {
		return zero, nil, errdefs.Validationf("%s: at least one layer is required", settings.label)
	}
	ordered, err := orderedLayers(layers, settings.label)
	if err != nil {
		return zero, nil, err
	}

	provenance := Provenance{}
	// A nil recorder is how the merge is told not to record: WithProvenance is
	// the common case for a host, and one branch per leaf is cheaper than
	// building an index nobody reads.
	var recorder *Provenance
	if settings.provenance {
		recorder = &provenance
	}
	var merged *yaml.Node
	for _, layer := range ordered {
		body, err := layer.body(ctx, settings.label)
		if err != nil {
			return zero, nil, err
		}
		document, err := parseDocument(body)
		if err != nil {
			return zero, nil, errdefs.Validationf(
				"%s: layer %q: %v", settings.label, layer.Name, err)
		}
		if document == nil {
			// A layer of comments contributes nothing; the stack is refused
			// only when every layer is like this.
			continue
		}
		// Merge keys are expanded before anything else looks at the tree, so
		// a `<<` is a literal key that Raw, Origin and Canonical all agree on.
		if err := expandMergeKeys(document); err != nil {
			return zero, nil, errdefs.Validationf(
				"%s: layer %q: %v", settings.label, layer.Name, err)
		}
		if err := checkDuplicateKeys(document); err != nil {
			return zero, nil, errdefs.Validationf(
				"%s: layer %q: %v", settings.label, layer.Name, err)
		}
		ref := Ref{Priority: layer.Priority, Name: layer.Name}
		if merged == nil {
			merged = document
			recordAll(recorder, merged, nil, ref)
			continue
		}
		merged = mergeNode(merged, document, recorder, ref, nil)
	}
	if merged == nil || len(merged.Content) == 0 {
		return zero, nil, errdefs.Validationf(
			"%s: the layers describe an empty document", settings.label)
	}

	doc := &Doc{label: settings.label, root: documentBody(merged), prov: provenance}
	if err := resolvePlaceholders(ctx, doc.root, "", settings); err != nil {
		return zero, nil, err
	}
	value, err := decodeDoc(ctx, doc, decode)
	if err != nil {
		return zero, nil, err
	}
	return value, doc, nil
}

// Decode expands and decodes one document, for a caller that owns a single file
// rather than a stack — a deployment document, for one.
//
// It applies the same expansion rules [Load] does, so a document and a layer
// cannot disagree about what a placeholder means.
func Decode[T any](ctx context.Context, body []byte, decode DecodeFunc[T], opts ...Option) (T, error) {
	var zero T
	settings := newOptions(opts)
	if decode == nil {
		decode = Strict[T]
	}
	document, err := parseDocument(body)
	if err != nil {
		return zero, errdefs.Validationf("%s: %v", settings.label, err)
	}
	if document == nil {
		return zero, errdefs.Validationf("%s: the document is empty", settings.label)
	}
	if err := expandMergeKeys(document); err != nil {
		return zero, errdefs.Validationf("%s: %v", settings.label, err)
	}
	doc := &Doc{label: settings.label, root: documentBody(document)}
	if err := resolvePlaceholders(ctx, doc.root, "", settings); err != nil {
		return zero, err
	}
	return decodeDoc(ctx, doc, decode)
}

// parseDocument reads exactly one YAML document from a body.
//
// "Exactly one" is the rule the rest of this module already applies to a single
// configuration document: YAML silently stops at the first `---`, so a layer
// that holds two documents would contribute the first and drop the second
// without a word. A nil node and no error means the body carries no document at
// all — comments only, or nothing.
func parseDocument(body []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var document yaml.Node
	err := decoder.Decode(&document)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New(
				"the body holds more than one YAML document; a `---` starts a second one")
		}
		return nil, err
	}
	return &document, nil
}

// checkDuplicateKeys refuses a mapping that writes the same key twice.
//
// YAML accepts it and takes the last value, which is indistinguishable from a
// typo the reader cannot see: the merge would collapse the two entries before
// anything could report them, so the check has to happen on the layer as
// written. The walk covers nested mappings; two entries of one sequence are
// different mappings and may repeat a key.
func checkDuplicateKeys(document *yaml.Node) error {
	node := documentBody(document)
	switch node.Kind {
	case yaml.MappingNode:
		seen := make(map[string]int, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if first, duplicate := seen[key.Value]; duplicate {
				return errdefs.Validationf(
					"duplicate key %q (already defined on line %d)", key.Value, first)
			}
			seen[key.Value] = key.Line
			if err := checkDuplicateKeys(node.Content[i+1]); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := checkDuplicateKeys(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// decodeDoc decodes one merged document through the caller's decoder.
func decodeDoc[T any](ctx context.Context, doc *Doc, decode DecodeFunc[T]) (T, error) {
	var zero T
	canonical, err := doc.Canonical()
	if err != nil {
		return zero, err
	}
	value, err := decode(ctx, canonical)
	if err != nil {
		return zero, errdefs.Validationf("%s: %v", doc.label, err)
	}
	return value, nil
}
