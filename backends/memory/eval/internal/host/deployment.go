// Package host wires the memory deployment a harness command measures. It
// exists so the runners under cmd/ build the same assembly from the same
// document: a diagnostic that wired its own resources could report on a
// deployment the runner never builds.
//
// It is internal because it is host wiring, not harness surface: the eval
// package stays independent of core/deploy and the provider drivers.
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	flowmemory "github.com/GizClaw/flowcraft/backends/memory"
	"github.com/GizClaw/flowcraft/backends/memory/eval"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/inference"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/secret"
	"github.com/GizClaw/flowcraft/core/workspace"
	"github.com/GizClaw/flowcraft/driver/bytedance"
	"github.com/GizClaw/flowcraft/driver/openai"
)

// Deployment carries the built assemblies and their lifecycle.
type Deployment struct {
	Memory *flowmemory.Assembly
	// Inference is nil for an ad-hoc deployment, which wires no provider.
	Inference *inference.Assembly
	// Recent is the recent-message window the assembly will serve with, the
	// settings document plus the library defaults. Hosts report this instead of
	// the flag they passed, because a deploy document overrides the flag and a
	// run must record the value it measured under.
	Recent flowmemory.RecentSettings
	// Rerank is the retrieval.rerank setting the memory resource was built
	// with. It is read off the deploy document because the fingerprint needs the
	// effective value: rerank.AlgorithmVersion names the policy linked into the
	// binary whether or not retrieval ever calls it.
	Rerank bool
	Close  func()
}

// Build returns the deployment the document at deployPath describes. An empty
// path yields an ad-hoc assembly with no models and a throwaway workspace: it
// exercises the recent, BM25, and entity lanes without an LLM.
func Build(deployPath string, recentItems int) (Deployment, error) {
	ctx := context.Background()
	if deployPath != "" {
		registry := resource.NewRegistry()
		for _, register := range []func(*resource.Registry) error{
			workspace.Register, secret.Register, inference.Register,
			openai.Register, bytedance.Register, flowmemory.Register,
		} {
			if err := register(registry); err != nil {
				return Deployment{}, err
			}
		}
		raw, err := os.ReadFile(deployPath)
		if err != nil {
			return Deployment{}, err
		}
		doc, err := deploy.Parse(raw)
		if err != nil {
			return Deployment{}, err
		}
		var settings resourceSettings
		for _, configured := range doc.Resources {
			if configured.Kind != corememory.AssemblyKind || len(configured.Settings) == 0 {
				continue
			}
			if err := json.Unmarshal(configured.Settings, &settings); err != nil {
				return Deployment{}, err
			}
		}
		loader := resource.NewLoader(resource.WithBaseDir(filepath.Dir(deployPath)))
		result, err := deploy.NewBuilder(registry, deploy.WithLoader(loader)).Deploy(ctx, doc)
		if err != nil {
			return Deployment{}, err
		}
		value, ok := result.Value("memories")
		if !ok {
			return Deployment{}, fmt.Errorf("deploy document has no %q resource", "memories")
		}
		assembly, ok := value.(*flowmemory.Assembly)
		if !ok {
			return Deployment{}, fmt.Errorf("resource memories is %T", value)
		}
		var engine *inference.Assembly
		if raw, ok := result.Value("infer"); ok {
			if typed, ok := raw.(*inference.Assembly); ok {
				engine = typed
			}
		}
		return Deployment{
			Memory: assembly, Inference: engine, Recent: assembly.RecentSettings(),
			Rerank: settings.Retrieval.Rerank,
			Close:  func() { _ = result.Close() },
		}, nil
	}
	dir, err := os.MkdirTemp("", "memory-eval-*")
	if err != nil {
		return Deployment{}, err
	}
	ws, err := workspace.NewLocalWorkspace(filepath.Join(dir, "workspace"))
	if err != nil {
		return Deployment{}, err
	}
	value, err := flowmemory.NewFactory().New(ctx, resource.Input{
		// No generate/embed models configured: the run exercises the recent,
		// BM25, and entity lanes without an LLM.
		Settings: []byte(fmt.Sprintf(
			`{"interval":"0","scopes":[{"runtime_id":"memories"}],"recent":{"max_items":%d}}`, recentItems)),
		Deps: map[string]any{"workspace": ws},
	})
	if err != nil {
		return Deployment{}, err
	}
	assembly, ok := value.(*flowmemory.Assembly)
	if !ok {
		return Deployment{}, fmt.Errorf("factory returned %T", value)
	}
	return Deployment{
		Memory: assembly, Recent: assembly.RecentSettings(),
		Close: func() {
			_ = assembly.Close()
			_ = os.RemoveAll(dir)
		},
	}, nil
}

// resourceSettings is the slice of the memory resource's settings the fingerprint
// reports on besides the accessors above, so a run states the effective
// retrieval toggles instead of only the policy code linked into the binary.
type resourceSettings struct {
	Retrieval struct {
		Rerank bool `json:"rerank,omitempty"`
	} `json:"retrieval,omitempty"`
}

// LibraryState summarizes the derivation the assembly will answer from: the
// policy digest the watermarks were read under, and how many conversations carry
// no derivation for it. The two are not the same question -- a workspace derived
// under an older policy reports every conversation as underived while retrieval
// still serves the facts the older generation wrote -- so a host has to read the
// store rather than trust the digest it was built with.
//
// It is empty (rather than zeroed) when the store cannot be read, so a caller
// never reports an empty library it did not observe.
func LibraryState(ctx context.Context, memory *flowmemory.Assembly) eval.LibraryState {
	if memory == nil {
		return eval.LibraryState{}
	}
	diagnostics, err := memory.Diagnostics(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: reading the store's derivation state: %v\n", err)
		return eval.LibraryState{}
	}
	scopes := make([]eval.ScopeState, 0, len(diagnostics.Scopes))
	for _, scope := range diagnostics.Scopes {
		state := eval.ScopeState{
			RuntimeID: scope.Scope.RuntimeID,
			UserID:    scope.Scope.UserID,
			AgentID:   scope.Scope.AgentID,
		}
		for _, conversation := range scope.Conversations {
			state.Watermarks = append(state.Watermarks, eval.ConversationState{
				ConversationID: conversation.ConversationID,
				Watermark:      conversation.Watermark,
				Behind:         conversation.Behind,
			})
		}
		scopes = append(scopes, state)
	}
	return eval.NewLibraryState(memory.PolicyDigest(), scopes)
}

// LoadEnvFile sets KEY=VALUE pairs from path, so deploy documents can resolve
// ${env:...} without shell scripting. Blank lines, # comments, and empty values
// are ignored. A key that is already set is overwritten: naming the file is how
// a command says where its credentials come from.
func LoadEnvFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}

// MessageProvenance resolves a recalled item's message sources through the
// canonical message store, so evidence recall can follow a fact back to the
// turns it was derived from, and can do it by dataset turn identity for a store
// ingest tagged. It implements the harness's ProvenanceResolver.
type MessageProvenance struct {
	store *msgsource.MessageStore
	scope corememory.Scope
}

// NewMessageProvenance returns a resolver over one scope of the store. The
// store is the one the deployment's assembly wrote, so the resolver reads the
// same canonical messages retrieval was built from.
func NewMessageProvenance(store *msgsource.MessageStore, scope corememory.Scope) MessageProvenance {
	return MessageProvenance{store: store, scope: scope}
}

func (resolver MessageProvenance) ResolveSources(ctx context.Context, item corememory.ContextItem) []eval.ResolvedSource {
	if resolver.store == nil {
		return nil
	}
	var sources []eval.ResolvedSource
	for _, source := range item.Sources {
		if source.Kind != corememory.SourceMessage {
			continue
		}
		conversationID, messageID, ok := splitSourceID(source.ID)
		if !ok {
			continue
		}
		record, found, err := resolver.store.Get(ctx, resolver.scope, conversationID, messageID)
		if err != nil || !found {
			continue
		}
		sources = append(sources, eval.ResolvedSource{
			ConversationID: conversationID, MessageID: messageID,
			TurnID: strings.TrimSpace(record.Metadata[eval.DatasetTurnMetadataKey]),
			Text:   record.Message.Content.Text(),
		})
	}
	return sources
}

// CarriesTurnIDs reports whether the newest records of one conversation already
// carry dataset turn ids, that is, whether the store was ingested with the
// identity tagging the harness writes. A store that predates it leaves evidence
// matching on committed text alone, so a probe measuring identity coverage has
// to say so rather than report the absence of ids as an absence of recall.
func (resolver MessageProvenance) CarriesTurnIDs(ctx context.Context, conversationID string, limit int) bool {
	if resolver.store == nil || strings.TrimSpace(conversationID) == "" {
		return false
	}
	records, err := resolver.store.Latest(ctx, resolver.scope, conversationID, msgsource.LatestOptions{Limit: limit})
	if err != nil {
		return false
	}
	for _, record := range records {
		if strings.TrimSpace(record.Metadata[eval.DatasetTurnMetadataKey]) != "" {
			return true
		}
	}
	return false
}

// splitSourceID splits a "<conversation>/<message>" source reference. The
// message id may itself contain no separator, and the conversation is the part
// before the last one.
func splitSourceID(id string) (string, string, bool) {
	index := strings.LastIndex(id, "/")
	if index <= 0 || index == len(id)-1 {
		return "", "", false
	}
	return id[:index], id[index+1:], true
}
