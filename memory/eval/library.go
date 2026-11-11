package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// LibraryState records the derivation a run's answers were read from.
//
// The fingerprint's policy digest names the configuration the *code* was built
// with, which is not the same thing as the generation of facts in the store: a
// run with -skip-derive grades whatever the workspace already holds, and derived
// watermarks are keyed by policy digest. So a workspace derived under an older
// policy reports every conversation as underived here, while the fingerprint of
// the run that grades it still claims the newer digest -- two runs over one
// workspace and two derivation policies used to carry the same fingerprint.
//
// The watermarks themselves are already keyed per policy (worker/checkpoint.go),
// so "Underived" is read straight off the store rather than inferred.
type LibraryState struct {
	// PolicyDigest is the digest the watermarks were read under: the code's, not
	// necessarily the one that wrote the stored facts.
	PolicyDigest string `json:"policy_digest,omitempty"`
	// Digest summarizes the derivation cursors (scope, conversation, watermark),
	// so two runs that read the same generation of facts carry the same value.
	Digest string `json:"digest,omitempty"`
	// Conversations counts every conversation the store knows about, Underived
	// how many carry no watermark under PolicyDigest (their facts, if any, belong
	// to another generation), and Behind how many have commits past their
	// watermark.
	Conversations int          `json:"conversations,omitempty"`
	Underived     int          `json:"underived,omitempty"`
	Behind        int          `json:"behind,omitempty"`
	Scopes        []ScopeState `json:"scopes,omitempty"`
}

// ScopeState is one scope's derivation state: its cursors, not its content.
type ScopeState struct {
	RuntimeID string `json:"runtime_id"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	// Conversations, Underived, Behind and MaxWatermark are filled by
	// NewLibraryState from Watermarks.
	Conversations int                 `json:"conversations"`
	Underived     int                 `json:"underived,omitempty"`
	Behind        int                 `json:"behind,omitempty"`
	MaxWatermark  uint64              `json:"max_watermark,omitempty"`
	Watermarks    []ConversationState `json:"watermarks,omitempty"`
}

// ConversationState is one conversation's derivation cursor.
type ConversationState struct {
	ConversationID string `json:"conversation_id"`
	Watermark      uint64 `json:"watermark"`
	Behind         bool   `json:"behind,omitempty"`
}

// NewLibraryState summarizes the cursors a caller read off the store, in place:
// the per-scope counts and the whole-library digest are derived here so no
// caller can compute them differently. The scopes are stored sorted by name, so
// the JSON and the digest do not depend on the store's iteration order.
func NewLibraryState(policyDigest string, scopes []ScopeState) LibraryState {
	state := LibraryState{PolicyDigest: strings.TrimSpace(policyDigest)}
	lines := make([]string, 0, len(scopes))
	for index := range scopes {
		scope := scopes[index]
		sort.Slice(scope.Watermarks, func(i, j int) bool {
			return scope.Watermarks[i].ConversationID < scope.Watermarks[j].ConversationID
		})
		scope.Conversations = len(scope.Watermarks)
		scope.Underived, scope.Behind, scope.MaxWatermark = 0, 0, 0
		for _, conversation := range scope.Watermarks {
			if conversation.Watermark == 0 {
				scope.Underived++
			}
			if conversation.Behind {
				scope.Behind++
			}
			scope.MaxWatermark = max(scope.MaxWatermark, conversation.Watermark)
			lines = append(lines, strings.Join([]string{
				scope.RuntimeID, scope.UserID, scope.AgentID,
				conversation.ConversationID, strconv.FormatUint(conversation.Watermark, 10),
			}, "|"))
		}
		state.Conversations += scope.Conversations
		state.Underived += scope.Underived
		state.Behind += scope.Behind
		scopes[index] = scope
	}
	sort.Slice(scopes, func(i, j int) bool {
		return scopeName(scopes[i]) < scopeName(scopes[j])
	})
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	state.Digest = hex.EncodeToString(sum[:])[:12]
	state.Scopes = scopes
	return state
}

func scopeName(scope ScopeState) string {
	return scope.RuntimeID + "/" + scope.UserID + "/" + scope.AgentID
}

// Empty reports whether the store state was never read, which is what a run
// against an unwired assembly produces. The library block is omitted from the
// report in that case rather than claiming an empty library.
func (state LibraryState) Empty() bool {
	return state.PolicyDigest == "" && state.Digest == "" && len(state.Scopes) == 0
}

// String renders the state as one log line.
func (state LibraryState) String() string {
	if state.Empty() {
		return "unread"
	}
	summary := fmt.Sprintf("policy_digest=%s digest=%s conversations=%d underived=%d behind=%d",
		short(state.PolicyDigest), state.Digest, state.Conversations, state.Underived, state.Behind)
	for _, scope := range state.Scopes {
		summary += fmt.Sprintf(" scope[%s]=%d/%d", scopeName(scope), scope.Conversations, scope.MaxWatermark)
	}
	return summary
}
