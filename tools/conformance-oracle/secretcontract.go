package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/secret"
)

// The secret contract: what a `${secret:...}` reference means, what a Secret
// value is on the wire, and what resolving one through a store answers.
//
// Three tables, because the grammar has three seams and they refuse
// differently:
//
//   - `refs` is the scheme (`SecretResolver.Scheme`), which runs at *build*
//     time and emits a marker rather than a value. Its refusals are prefixed
//     `resource settings expand: `. The paths here are what the `${}` parser
//     hands the scheme, i.e. already trimmed of outer space (`parseReference`
//     in `core/resource/reference.go` trims both sides of the colon).
//   - `resolves` is `SecretResolver.Resolve`, which runs at *use* time. Its
//     refusals are prefixed `resource secret: `, they are a different family
//     from the scheme's, and a backend failure is wrapped into one of them
//     rather than reclassified -- core does not turn a store outage into a
//     `not_available`, and the port must not either.
//   - `values` is the `Secret` value itself: which documents decode, what the
//     wire form is (always `"<secret>"`), and that every rendering verb
//     redacts (in Rust: `Display` and `Debug`).
//
// The stores in every row are `resource.SecretStoreFunc` wrappers over a
// canned answer table, so a row is a function of this file: no process
// environment, no clock, no filesystem. The stores core ships (`env`, `file`)
// and the rules for wiring them (`core/deploy`'s id fallback and default
// uniqueness) come with the store tables in a later dump.
type secretContract struct {
	// Note says what the tables cover.
	Note []string `json:"note"`
	// Masked names what a reader must not expect to compare.
	Masked []string `json:"masked"`
	// Refs holds the scheme's answers per reference path.
	Refs []secretRefsCase `json:"refs"`
	// Resolves holds use-time lookups, one store and name per row.
	Resolves []secretResolveCase `json:"resolves"`
	// Values holds Secret documents, their wire form and their resolution.
	Values []secretValueCase `json:"values"`
	// Stores holds the built-in stores, their settings decode and their
	// lookups.
	Stores []secretStoreCase `json:"stores"`
}

// secretStoreCase builds one store from its settings and asks it about names.
type secretStoreCase struct {
	// Name identifies the case in a mismatch.
	Name string `json:"name"`
	// Why says what the row is evidence of.
	Why string `json:"why"`
	// Impl is the `secret.Store` implementation to build.
	Impl string `json:"impl"`
	// Settings is the factory's settings subtree. A string equal to
	// `$BASE` is the harness's temporary directory, which both sides
	// substitute, so no path in this file depends on the run.
	Settings json.RawMessage `json:"settings"`
	// Files is the medium a file-backed store reads, written under the
	// temporary directory before the store is built.
	Files []secretFile `json:"files,omitempty"`
	// ID is the store's `SecretStoreID`, nil when it answers "".
	ID *string `json:"id"`
	// Default is the store's `DefaultSecretStore` answer.
	Default bool `json:"default"`
	// Error is the factory's refusal, when it refused the settings.
	Error string `json:"error,omitempty"`
	// ErrorPrefix is the factory's refusal up to the point where the
	// decoder's own words begin. A settings document the decode refuses
	// ends in a sentence that belongs to `encoding/json` (`json: unknown
	// field "x"`), which no port can reproduce; the sentence core wrote
	// around it is the contract, and it is compared as a prefix.
	ErrorPrefix string `json:"error_prefix,omitempty"`
	// Lookups are the questions put to the built store.
	Lookups []secretLookupAnswer `json:"lookups,omitempty"`
}

// secretFile is one file the harness writes into the medium.
type secretFile struct {
	// Name is the path inside the base directory.
	Name string `json:"name"`
	// Data is the file's contents.
	Data string `json:"data,omitempty"`
	// Fill writes that many `x` bytes instead of Data, for a file whose
	// size is the point.
	Fill int `json:"fill,omitempty"`
}

// secretLookupAnswer is what one lookup answered.
type secretLookupAnswer struct {
	// Name is the secret name asked about.
	Name string `json:"name"`
	// Found is the store's `found` return.
	Found bool `json:"found"`
	// Value is the value a found secret answered.
	Value string `json:"value,omitempty"`
	// FromProcess means the value is the process's environment and is not
	// in this file: the replay compares the store's answer against its own
	// `os.LookupEnv` rather than against a string committed here.
	FromProcess bool `json:"from_process,omitempty"`
	// Error is the lookup's refusal, when it refused.
	Error string `json:"error,omitempty"`
}

// secretStoreSpec is one store in a scenario: the id it is addressable by,
// whether it claims the default flag, and the answers it gives.
type secretStoreSpec struct {
	// ID is the key the store is registered under, and the prefix a
	// `store.NAME` reference matches.
	ID string `json:"id"`
	// Default is the store's `DefaultSecretStore` answer. Only the wiring
	// rows read it; the refs rows name their default store explicitly.
	Default bool `json:"default,omitempty"`
	// Answers is the canned lookup table.
	Answers []secretLookup `json:"answers,omitempty"`
}

// secretLookup is one name's canned answer: a value, a not-found, or a
// backend failure.
type secretLookup struct {
	// Name is the secret name looked up.
	Name string `json:"name"`
	// Value is what the store returns when found.
	Value string `json:"value,omitempty"`
	// Found is nil for "found" -- the ordinary case -- and false for a
	// store that has no such secret.
	Found *bool `json:"found,omitempty"`
	// Error is a backend failure, returned as an ordinary Go error.
	Error string `json:"error,omitempty"`
}

// secretRefsCase puts one path list to one resolver.
type secretRefsCase struct {
	// Name identifies the case in a mismatch.
	Name string `json:"name"`
	// Why says what the row is evidence of.
	Why string `json:"why"`
	// Stores are the configured stores.
	Stores []secretStoreSpec `json:"stores"`
	// DefaultStore is the id NAME-only references resolve through; empty
	// means NAME-only references are refused.
	DefaultStore string `json:"default_store"`
	// Paths are the reference paths, in order.
	Paths []string `json:"paths"`
	// Answers are the scheme's answers, in the same order.
	Answers []secretRefAnswer `json:"answers"`
}

// secretRefAnswer is one path's answer: a marker or a refusal.
type secretRefAnswer struct {
	// Path repeats the path asked about, so a diff names the row.
	Path string `json:"path"`
	// Store is the marker's store id.
	Store string `json:"store,omitempty"`
	// Name is the marker's secret name.
	Name string `json:"name,omitempty"`
	// Error is the refusal's text.
	Error string `json:"error,omitempty"`
}

// secretResolveCase asks one store for one name.
type secretResolveCase struct {
	// Name identifies the case in a mismatch.
	Name string `json:"name"`
	// Why says what the row is evidence of.
	Why string `json:"why"`
	// Stores are the configured stores.
	Stores []secretStoreSpec `json:"stores"`
	// DefaultStore is the resolver's default.
	DefaultStore string `json:"default_store"`
	// NoResolver asks a nil resolver, which is `Secret.Resolve`'s answer
	// when a factory was handed no `Input.Secrets`.
	NoResolver bool `json:"no_resolver,omitempty"`
	// Store is the store to resolve through.
	Store string `json:"store"`
	// SecretName is the name to resolve.
	SecretName string `json:"secret_name"`
	// Value is the resolved secret.
	Value string `json:"value,omitempty"`
	// Error is the refusal's text.
	Error string `json:"error,omitempty"`
}

// secretValueCase decodes one settings value into a Secret and reports what
// it is, how it renders, and what it resolves to.
type secretValueCase struct {
	// Name identifies the case in a mismatch.
	Name string `json:"name"`
	// Why says what the row is evidence of.
	Why string `json:"why"`
	// Doc is the JSON text of the settings value, as written.
	Doc string `json:"doc"`
	// Stores are the stores the resolution runs against.
	Stores []secretStoreSpec `json:"stores,omitempty"`
	// DefaultStore is the resolver's default.
	DefaultStore string `json:"default_store"`
	// NoResolver resolves without a resolver at all.
	NoResolver bool `json:"no_resolver,omitempty"`
	// OK reports whether the document decoded.
	OK bool `json:"ok"`
	// Error is the decode refusal's text.
	Error string `json:"error,omitempty"`
	// Ref reports whether the decoded value is a lazy reference.
	Ref bool `json:"ref,omitempty"`
	// Wire is `json.Marshal`'s output for the decoded value.
	Wire string `json:"wire,omitempty"`
	// Display is `fmt.Sprint`'s output.
	Display string `json:"display,omitempty"`
	// GoSyntax is `fmt.Sprintf("%#v")`'s output.
	GoSyntax string `json:"go_syntax,omitempty"`
	// Resolved is the value `Secret.Resolve` answered.
	Resolved string `json:"resolved"`
	// ResolveError is the resolution refusal's text.
	ResolveError string `json:"resolve_error,omitempty"`
}

// writeSecretContract dumps the secret grammar tables to w.
func writeSecretContract(w io.Writer) error {
	contract := secretContract{
		Note: []string{
			"A ${secret:...} reference has three seams, and they refuse in three sentences: the scheme (build time) emits a marker and prefixes refusals with `resource settings expand: `, Resolve (use time) prefixes them with `resource secret: ` and wraps a backend failure into one, and the Secret value itself redacts to `<secret>` under every rendering verb.",
			"A path is what the ${} parser hands the scheme: outer space around the colon is already trimmed by parseReference, so the rows here never contain a leading or trailing blank.",
			"Every store in this file is a resource.SecretStoreFunc over a canned answer table: no process environment, no clock, no filesystem, so a row is a function of this file and nothing is masked.",
			"Not covered here: the stores core ships (`env`, `file`), the strict settings decode behind them, and the wiring rules `core/deploy` applies (id fallback to the resource name, duplicate ids, one default). Those come with the store tables.",
			"Every refusal below is a validation error; the wrapping of a backend failure is the one that looks like it should be a not_available and is not.",
		},
		Masked: []string{
			"nothing runtime-dependent: the stores are canned answer tables, so no sentence here depends on the run",
			"the escaping spelling of `wire`: core writes it with json.Marshal, which HTML-escapes `<`, `>` and `&`, and the port writes it with serde_json, which does not. Both decode to the string \"<secret>\", so the replay compares the decoded value and not the bytes",
		},
		Refs:     secretRefsCases(),
		Resolves: secretResolveCases(),
		Values:   secretValueCases(),
		Stores:   secretStoreCases(),
	}
	for i := range contract.Refs {
		row := &contract.Refs[i]
		row.Answers = secretRefAnswers(*row)
	}
	for i := range contract.Resolves {
		row := &contract.Resolves[i]
		row.Value, row.Error = secretResolveAnswer(
			row.Stores, row.DefaultStore, row.NoResolver, row.Store, row.SecretName)
	}
	for i := range contract.Values {
		row := &contract.Values[i]
		secretValueAnswer(row)
	}
	for i := range contract.Stores {
		row := &contract.Stores[i]
		if err := secretStoreAnswer(row); err != nil {
			return err
		}
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(contract)
}

// secretStoreCases is the built-in stores' table.
func secretStoreCases() []secretStoreCase {
	return []secretStoreCase{
		{
			Name:     "env-default",
			Why:      "the env store reads the process environment by the name the reference gives it, and reports a missing variable as not-found rather than as a failure",
			Impl:     "env",
			Settings: json.RawMessage(`{"default": true}`),
			Lookups: []secretLookupAnswer{
				{Name: "PATH", Found: true, FromProcess: true},
				{Name: "FLOWCRAFT_CONTRACT_ABSENT", Found: false},
			},
		},
		{
			Name:     "env-id",
			Why:      "id and default come from the settings, and an absent id means the deployment resource's name is used instead -- the store answers no id",
			Impl:     "env",
			Settings: json.RawMessage(`{"id": "vault-env"}`),
		},
		{
			Name:     "file-base-required",
			Why:      "a file store without a base directory is refused at build time, before any lookup",
			Impl:     "file",
			Settings: json.RawMessage(`{"default": true}`),
		},
		{
			Name:     "file-token",
			Why:      "a file store reads one file per secret and strips the trailing newline a `printf` or `echo` leaves, so a key stored as a line works",
			Impl:     "file",
			Settings: json.RawMessage(`{"base": "$BASE"}`),
			Files: []secretFile{
				{Name: "token", Data: "tok-456\n"},
				{Name: "crlf", Data: "tok-789\r\n\r\n"},
			},
			Lookups: []secretLookupAnswer{
				{Name: "token", Found: true, Value: "tok-456"},
				{Name: "crlf", Found: true, Value: "tok-789"},
				{Name: "missing", Found: false},
				{Name: "../secret", Found: false},
				{Name: "/etc/passwd", Found: false},
			},
		},
		{
			Name:     "file-cap",
			Why:      "the loader caps what it will read, and a file over the cap is a refusal rather than a truncated secret",
			Impl:     "file",
			Settings: json.RawMessage(`{"base": "$BASE"}`),
			Files: []secretFile{
				{Name: "small", Fill: 3},
				{Name: "big", Fill: 1<<20 + 1},
			},
			Lookups: []secretLookupAnswer{
				{Name: "small", Found: true, Value: "xxx"},
				{Name: "big", Found: false},
			},
		},
		{
			Name:     "file-id",
			Why:      "a file store carries its id and default flag from the settings, like the env store",
			Impl:     "file",
			Settings: json.RawMessage(`{"base": "$BASE", "id": "files", "default": true}`),
			Files: []secretFile{
				{Name: "token", Data: "tok-456"},
			},
			Lookups: []secretLookupAnswer{
				{Name: "token", Found: true, Value: "tok-456"},
			},
		},
		{
			Name:     "file-blank-base",
			Why:      "a base directory of spaces is blank after trimming, so it is missing rather than a path",
			Impl:     "file",
			Settings: json.RawMessage(`{"base": "  "}`),
		},
		{
			Name:     "file-null-base",
			Why:      "a null field leaves its scalar untouched, which is encoding/json's rule for every type -- so a null base is the same missing base, not a shape refusal",
			Impl:     "file",
			Settings: json.RawMessage(`{"base": null, "default": true}`),
		},
		{
			Name:     "env-unknown-field",
			Why:      "settings decode strictly, so a typo is refused instead of silently dropped -- and the part of the sentence that is core's is the prefix",
			Impl:     "env",
			Settings: json.RawMessage(`{"defualt": true}`),
		},
		{
			Name:     "file-unknown-field",
			Why:      "the same strictness in the other store's struct: one row per settings type, because a typo in one proves nothing about the other",
			Impl:     "file",
			Settings: json.RawMessage(`{"base": "$BASE", "defualt": true}`),
		},
	}
}

// secretStoreAnswer builds one row's store and asks it its questions.
func secretStoreAnswer(row *secretStoreCase) error {
	base, err := os.MkdirTemp("", "flowcraft-secret-contract")
	if err != nil {
		return fmt.Errorf("secret store %q: temp dir: %w", row.Name, err)
	}
	defer func() {
		if err := os.RemoveAll(base); err != nil {
			fmt.Fprintf(os.Stderr, "secret store %q: remove temp dir: %v\n", row.Name, err)
		}
	}()
	for _, file := range row.Files {
		data := []byte(file.Data)
		if file.Fill > 0 {
			data = bytes.Repeat([]byte("x"), file.Fill)
		}
		path := filepath.Join(base, file.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("secret store %q: %w", row.Name, err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("secret store %q: %w", row.Name, err)
		}
	}
	settings, err := substituteBase(row.Settings, base)
	if err != nil {
		return fmt.Errorf("secret store %q: %w", row.Name, err)
	}
	value, err := buildSecretStore(row.Impl, settings)
	if err != nil {
		// A decode refusal ends in the decoder's own words, which belong to
		// `encoding/json`; core's part of the sentence is a prefix and is
		// what this document commits.
		if prefix := decodePrefix(row.Impl); strings.HasPrefix(err.Error(), prefix) {
			row.ErrorPrefix = prefix
		} else {
			row.Error = err.Error()
		}
		return nil
	}
	store, ok := value.(resource.SecretStore)
	if !ok {
		return fmt.Errorf("secret store %q: %T is not a resource.SecretStore", row.Name, value)
	}
	row.Default = store.DefaultSecretStore()
	if named, ok := value.(resource.SecretStoreID); ok && named.SecretStoreID() != "" {
		id := named.SecretStoreID()
		row.ID = &id
	}
	for i := range row.Lookups {
		answer := &row.Lookups[i]
		value, found, err := store.Lookup(context.Background(), answer.Name)
		switch {
		case err != nil:
			answer.Error = err.Error()
			answer.Found = false
		case !found:
			answer.Found = false
		default:
			answer.Found = true
			// A process-environment row does not commit the machine's
			// value: the replay compares against its own process.
			if !answer.FromProcess {
				answer.Value = value
			}
		}
	}
	return nil
}

// decodePrefix is the sentence core writes before a settings decode failure's
// decoder text: the factory's wrapper, then DecodeTyped's, then
// DecodeSettings'. The tail names the field the way that decoder names it and
// is not part of the contract.
func decodePrefix(impl string) string {
	return "secret " + impl + " store: decode settings: resource settings decode: resource settings: "
}

// buildSecretStore builds one `secret.Store` value through the registry, which
// is the only way core exposes its built-in impls -- `Register` is the seam, so
// the row also proves both impls are registered.
func buildSecretStore(impl string, settings []byte) (any, error) {
	registry := resource.NewRegistry()
	if err := secret.Register(registry); err != nil {
		return nil, fmt.Errorf("register secret stores: %w", err)
	}
	factory, ok := registry.Lookup(secret.ResourceKind, impl)
	if !ok {
		return nil, fmt.Errorf("%s/%s is not registered", secret.ResourceKind, impl)
	}
	return factory.New(context.Background(), resource.Input{Settings: settings})
}

// substituteBase replaces every string equal to `$BASE` -- at any depth -- with
// the harness's temporary directory, and returns the settings as bytes.
func substituteBase(settings json.RawMessage, base string) ([]byte, error) {
	var decoded any
	if err := json.Unmarshal(settings, &decoded); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}
	return json.Marshal(substituteString(decoded, "$BASE", base))
}

// substituteString is the walk behind substituteBase.
func substituteString(value any, from, to string) any {
	switch typed := value.(type) {
	case string:
		if typed == from {
			return to
		}
		return typed
	case map[string]any:
		for key, item := range typed {
			typed[key] = substituteString(item, from, to)
		}
		return typed
	case []any:
		for i, item := range typed {
			typed[i] = substituteString(item, from, to)
		}
		return typed
	default:
		return value
	}
}

// secretRefsCases is the scheme's table.
func secretRefsCases() []secretRefsCase {
	return []secretRefsCase{
		{
			Name: "default-and-named",
			Why:  "a dotted prefix naming a configured store is an explicit store reference; anything else falls back to the default store whole, so dotted secret names stay addressable",
			Stores: []secretStoreSpec{
				{ID: "env"},
				{ID: "files"},
			},
			DefaultStore: "env",
			Paths: []string{
				"TOKEN",
				"files.token",
				"my.app.key",
				".TOKEN",
				"TOKEN.",
				"a.files.token",
				"env .key",
				"absent.token",
			},
		},
		{
			Name: "no-default",
			Why:  "a named store resolves without a default, and a NAME-only reference is refused rather than guessed",
			Stores: []secretStoreSpec{
				{ID: "files"},
			},
			Paths: []string{
				"files.token",
				"TOKEN",
				".TOKEN",
			},
		},
		{
			Name: "empty-name",
			Why:  "the store prefix matches before the name is validated, so `files.` is the empty-name refusal; a lone dot has no prefix at all and is a name in the default store",
			Stores: []secretStoreSpec{
				{ID: "files"},
			},
			DefaultStore: "files",
			Paths: []string{
				"files.",
				".",
				"files.  ",
			},
		},
		{
			Name: "prefix-without-default",
			Why:  "an unconfigured dotted prefix is not a refusal about the store: the whole path becomes the name of a default-store reference, and with no default the answer is the missing-default sentence",
			Stores: []secretStoreSpec{
				{ID: "files"},
			},
			Paths: []string{
				"vault.token",
				"files.token",
			},
		},
	}
}

// secretRefAnswers runs one row's paths through the scheme.
func secretRefAnswers(row secretRefsCase) []secretRefAnswer {
	scheme := resource.NewSecretResolver(
		secretStores(row.Stores), row.DefaultStore).Scheme()
	answers := make([]secretRefAnswer, 0, len(row.Paths))
	for _, path := range row.Paths {
		answer := secretRefAnswer{Path: path}
		value, err := scheme.Resolve(
			context.Background(), resource.Reference{Scheme: "secret", Path: path})
		if err != nil {
			answer.Error = err.Error()
			answers = append(answers, answer)
			continue
		}
		ref, ok := value.(resource.SecretRef)
		if !ok {
			answer.Error = fmt.Sprintf("scheme answered %T, want resource.SecretRef", value)
			answers = append(answers, answer)
			continue
		}
		answer.Store, answer.Name = ref.Store, ref.Name
		answers = append(answers, answer)
	}
	return answers
}

// secretResolveCases is the use-time table.
func secretResolveCases() []secretResolveCase {
	return []secretResolveCase{
		{
			Name: "value",
			Why:  "a found secret comes back verbatim, newlines and all",
			Stores: []secretStoreSpec{{
				ID:      "env",
				Answers: []secretLookup{{Name: "TOKEN", Value: "tok-123"}},
			}},
			DefaultStore: "env",
			Store:        "env",
			SecretName:   "TOKEN",
		},
		{
			Name: "missing",
			Why:  "a store that has no such secret is a refusal naming both the secret and the store, not an empty value",
			Stores: []secretStoreSpec{{
				ID:      "env",
				Answers: []secretLookup{{Name: "NOPE", Found: boolPtr(false)}},
			}},
			DefaultStore: "env",
			Store:        "env",
			SecretName:   "NOPE",
		},
		{
			Name: "store-failure",
			Why:  "a backend failure is wrapped verbatim into a validation sentence instead of being reclassified as not_available",
			Stores: []secretStoreSpec{{
				ID:      "env",
				Answers: []secretLookup{{Name: "DOWN", Error: "backend is down"}},
			}},
			DefaultStore: "env",
			Store:        "env",
			SecretName:   "DOWN",
		},
		{
			Name: "unconfigured-store",
			Why:  "resolving through an id that was never configured is refused before any store is asked",
			Stores: []secretStoreSpec{{
				ID:      "env",
				Answers: []secretLookup{{Name: "TOKEN", Value: "tok-123"}},
			}},
			DefaultStore: "env",
			Store:        "vault",
			SecretName:   "TOKEN",
		},
		{
			Name:         "no-resolver",
			Why:          "a Secret handed to a factory with no Input.Secrets refuses with the same sentence shape, naming the secret it could not resolve",
			Stores:       []secretStoreSpec{{ID: "env"}},
			DefaultStore: "env",
			NoResolver:   true,
			Store:        "env",
			SecretName:   "TOKEN",
		},
	}
}

// secretResolveAnswer asks one resolver for one secret.
func secretResolveAnswer(
	stores []secretStoreSpec,
	defaultStore string,
	noResolver bool,
	storeName, name string,
) (value, refusal string) {
	var resolved *resource.SecretResolver
	if !noResolver {
		resolved = resource.NewSecretResolver(secretStores(stores), defaultStore)
	}
	answer, err := resolved.Resolve(context.Background(), storeName, name)
	if err != nil {
		return "", err.Error()
	}
	return answer, ""
}

// secretValueCases is the wire table.
func secretValueCases() []secretValueCase {
	envStore := []secretStoreSpec{{
		ID: "env",
		Answers: []secretLookup{
			{Name: "TOKEN", Value: "tok-123"},
			{Name: "NOPE", Found: boolPtr(false)},
		},
	}}
	return []secretValueCase{
		{
			Name:         "literal",
			Why:          "a plain string is a literal secret: not a reference, and resolvable with no resolver at all",
			Doc:          `"literal-value"`,
			DefaultStore: "env",
		},
		{
			Name:         "literal-empty",
			Why:          "the empty string is a literal too, not an absent value",
			Doc:          `""`,
			DefaultStore: "env",
		},
		{
			Name:         "literal-blank",
			Why:          "a literal of spaces is kept as written: blank is refused for a reference, never for a literal",
			Doc:          `"  "`,
			DefaultStore: "env",
		},
		{
			Name:         "reference",
			Why:          "a reference object decodes to a lazy reference and resolves through its store at use time",
			Doc:          `{"store":"env","name":"TOKEN"}`,
			Stores:       envStore,
			DefaultStore: "env",
		},
		{
			Name:         "reference-extra-field",
			Why:          "unknown fields in a reference object are ignored, because core decodes it with a plain json.Unmarshal",
			Doc:          `{"store":"env","name":"TOKEN","note":"kept"} `,
			Stores:       envStore,
			DefaultStore: "env",
		},
		{
			Name:         "reference-unconfigured-store",
			Why:          "a reference decodes without checking the store exists; the refusal arrives at use time",
			Doc:          `{"store":"vault","name":"TOKEN"}`,
			Stores:       envStore,
			DefaultStore: "env",
		},
		{
			Name:         "reference-missing-secret",
			Why:          "an empty lookup is a use-time refusal, not an empty value",
			Doc:          `{"store":"env","name":"NOPE"}`,
			Stores:       envStore,
			DefaultStore: "env",
		},
		{
			Name:         "reference-no-store",
			Why:          "a reference object without a store is refused at decode time, and blank counts as absent",
			Doc:          `{"name":"TOKEN"}`,
			DefaultStore: "env",
		},
		{
			Name:         "reference-blank-store",
			Why:          "a store of spaces is blank after trimming",
			Doc:          `{"store":"  ","name":"TOKEN"}`,
			DefaultStore: "env",
		},
		{
			Name:         "reference-blank-name",
			Why:          "a name of tabs is blank after trimming",
			Doc:          `{"store":"env","name":"\t"}`,
			DefaultStore: "env",
		},
		{
			Name:         "reference-case-folded-fields",
			Why:          "reference field names match case-insensitively, which is encoding/json's rule and not a choice of core's",
			Doc:          `{"Store":"env","NAME":"TOKEN"}`,
			Stores:       envStore,
			DefaultStore: "env",
		},
		{
			Name:         "reference-null-field",
			Why:          "a null field is the same no-op it is for a plain string, so it leaves that half of the reference blank",
			Doc:          `{"store":null,"name":"TOKEN"}`,
			DefaultStore: "env",
		},
		{
			Name:         "reference-number-field",
			Why:          "a field of the wrong type fails the struct decode, so the shape refusal quotes the document rather than calling the field blank",
			Doc:          `{"store":5,"name":"TOKEN"}`,
			DefaultStore: "env",
		},
		{
			Name:         "null",
			Why:          "null unmarshals into the string branch as a no-op, so it is the empty literal -- not a reference, not the blank-reference refusal, and not the wrong-shape one",
			Doc:          `null`,
			DefaultStore: "env",
		},
		{
			Name:         "empty-object",
			Why:          "an empty object decodes into the reference struct and leaves it zero, so it is the blank-reference refusal",
			Doc:          `{}`,
			DefaultStore: "env",
		},
		{
			Name:         "number",
			Why:          "a scalar that is not a string is the wrong-shape refusal, and it quotes the document as written",
			Doc:          `5`,
			DefaultStore: "env",
		},
		{
			Name:         "array",
			Why:          "an array is the wrong-shape refusal too, quoted as written",
			Doc:          `[1, 2]`,
			DefaultStore: "env",
		},
		{
			Name:         "bool",
			Why:          "a boolean is the wrong-shape refusal, quoted as written",
			Doc:          `true`,
			DefaultStore: "env",
		},
		{
			Name:         "literal-no-resolver",
			Why:          "a literal short-circuits before the resolver, so a factory with no Input.Secrets still reads its own literal",
			Doc:          `"literal-value"`,
			DefaultStore: "env",
			NoResolver:   true,
		},
		{
			Name:         "reference-no-resolver",
			Why:          "a reference with no resolver is refused, naming the secret",
			Doc:          `{"store":"env","name":"TOKEN"}`,
			Stores:       envStore,
			DefaultStore: "env",
			NoResolver:   true,
		},
	}
}

// secretValueAnswer decodes one row's document and records what it became.
func secretValueAnswer(row *secretValueCase) {
	var value resource.Secret
	if err := json.Unmarshal([]byte(row.Doc), &value); err != nil {
		row.Error = err.Error()
		return
	}
	row.OK = true
	row.Ref = value.IsRef()
	wire, err := json.Marshal(value)
	if err != nil {
		row.Error = fmt.Sprintf("marshal: %v", err)
		return
	}
	row.Wire = string(wire)
	row.Display = fmt.Sprint(value)
	row.GoSyntax = fmt.Sprintf("%#v", value)

	var resolved *resource.SecretResolver
	if !row.NoResolver {
		resolved = resource.NewSecretResolver(secretStores(row.Stores), row.DefaultStore)
	}
	answer, err := value.Resolve(context.Background(), resolved)
	if err != nil {
		row.ResolveError = err.Error()
		return
	}
	row.Resolved = answer
}

// secretStores builds the canned stores a row names.
func secretStores(specs []secretStoreSpec) map[string]resource.SecretStore {
	stores := make(map[string]resource.SecretStore, len(specs))
	for _, spec := range specs {
		answers := make(map[string]secretLookup, len(spec.Answers))
		for _, answer := range spec.Answers {
			answers[answer.Name] = answer
		}
		stores[spec.ID] = resource.SecretStoreFunc{
			DefaultSecret: spec.Default,
			LookupFn: func(_ context.Context, name string) (string, bool, error) {
				answer, ok := answers[name]
				if !ok {
					return "", false, nil
				}
				if answer.Error != "" {
					return "", false, errors.New(answer.Error)
				}
				return answer.Value, answer.Found == nil || *answer.Found, nil
			},
		}
	}
	return stores
}

// boolPtr is the address of a bool literal, for the fields that spell "false"
// rather than omit it.
func boolPtr(value bool) *bool { return &value }
