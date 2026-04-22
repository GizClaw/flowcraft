package policy

import (
	"context"
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// ActorType enumerates the kinds of actors that can appear in envelopes.
type ActorType string

const (
	ActorUser    ActorType = "user"
	ActorAgent   ActorType = "agent"
	ActorAPIKey  ActorType = "api_key"
	ActorCron    ActorType = "cron"
	ActorSystem  ActorType = "system"
	ActorWebhook ActorType = "webhook"
)

// Actor describes who is performing an action.
// This struct is the source of truth for all authorization decisions.
type Actor struct {
	Type      ActorType // user / agent / api_key / cron / system / webhook
	ID        string
	RealmID   string
	Super     bool   // super admin flag; bypasses most restrictions
	RuntimeID string // for agent/cron-bound actors
	Scopes    []string
	Roles     []string // member/admin/owner within realm
}

// IsSuperAdmin returns true if this actor is a super admin.
func (a Actor) IsSuperAdmin() bool { return a.Super }

// HasScope returns true if the actor has the given scope.
// api_key actors are restricted to their granted scopes; other actors have all scopes.
func (a Actor) HasScope(scope string) bool {
	if a.Type == ActorAPIKey {
		return slices.Contains(a.Scopes, scope)
	}
	return true // other actor types have all scopes
}

// Decision represents the result of an authorization check.
type Decision struct {
	Allow  bool
	Reason string
}

func (d Decision) String() string {
	if d.Allow {
		return "allow"
	}
	return "deny: " + d.Reason
}

var (
	Allow = Decision{Allow: true, Reason: "default-allow"}
	Deny  = func(reason string) Decision { return Decision{Allow: false, Reason: reason} }
)

// Policy is the central authorization interface.
// All authorization decisions go through this interface — no ad-hoc checks in handlers.
type Policy interface {
	AllowAppend(ctx context.Context, actor Actor, env EnvelopeDraft) (Decision, error)
	AllowSubscribe(ctx context.Context, actor Actor, opts SubscribeOptions) (Decision, error)
	AllowRead(ctx context.Context, actor Actor, opts ReadOptions) (Decision, error)
}

// SubscribeOptions are the parameters for a subscription authorization check.
type SubscribeOptions struct {
	Partitions []string
	Types      []string
}

// ReadOptions are the parameters for a read authorization check.
type ReadOptions struct {
	Partitions []string
	Types      []string
}

// EnvelopeDraft is the subset of eventlog.Envelope needed for authorization.
// We duplicate the struct here to avoid importing the eventlog package.
type EnvelopeDraft struct {
	Partition string
	Type      string
}

// context keys for actor storage
type ctxKey struct{}

// WithActor stores an actor in the context for policy checks.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// ActorFrom extracts an actor from a context.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(ctxKey{}).(Actor)
	return a, ok
}

// MustActor extracts an actor from context or panics.
// Use this when the middleware is expected to have set the actor.
func MustActor(ctx context.Context) Actor {
	a, ok := ActorFrom(ctx)
	if !ok {
		panic("policy: actor missing in ctx (middleware not wired?)")
	}
	return a
}

// SystemActor constructs an actor for a system component.
func SystemActor(name string) Actor {
	return Actor{Type: ActorSystem, ID: name}
}

// AllowAppendFunc and AllowSubscribeFunc and AllowReadFunc are
// functional options for building policies.
type (
	AllowAppendFunc    func(ctx context.Context, actor Actor, env EnvelopeDraft) (Decision, error)
	AllowSubscribeFunc func(ctx context.Context, actor Actor, opts SubscribeOptions) (Decision, error)
	AllowReadFunc      func(ctx context.Context, actor Actor, opts ReadOptions) (Decision, error)
)

func (f AllowAppendFunc) AllowAppend(ctx context.Context, actor Actor, env EnvelopeDraft) (Decision, error) {
	return f(ctx, actor, env)
}
func (f AllowSubscribeFunc) AllowSubscribe(ctx context.Context, actor Actor, opts SubscribeOptions) (Decision, error) {
	return f(ctx, actor, opts)
}
func (f AllowReadFunc) AllowRead(ctx context.Context, actor Actor, opts ReadOptions) (Decision, error) {
	return f(ctx, actor, opts)
}

// ---- RealmStore interface ----

// RealmStore looks up realm membership and roles.
type RealmStore interface {
	// IsMember returns true if userID is a member of realmID with the given role.
	IsMember(realmID, userID string, role string) bool
	// RuntimeOwner returns the user ID that owns the given runtime.
	RuntimeOwner(ctx context.Context, runtimeID string) (string, error)
	// IsAgentInRuntime returns true if agentID belongs to the runtime.
	IsAgentInRuntime(ctx context.Context, agentID, runtimeID string) (bool, error)
	// RealmOfRuntime returns the realm ID that owns the given runtime.
	RealmOfRuntime(ctx context.Context, runtimeID string) (string, error)
	// RealmAdminOf returns true if userID is an admin of realmID.
	RealmAdminOf(ctx context.Context, realmID, userID string) (bool, error)
}

// parsePartition extracts the kind and ID from a partition string like
// "runtime:xxx". The partition prefixes come from eventlog so we never carry a
// duplicated source of truth (catch this with go vet + tests).
func parsePartition(partition string) (kind eventlog.PartitionKind, id string, ok bool) {
	return eventlog.SplitPartition(partition)
}

// SystemActorWhitelist is the set of system component names allowed to appear as actors.
var SystemActorWhitelist = map[string]bool{
	"cron_scheduler":          true,
	"webhook_outbound_sender": true,
	"chat_auto_ack":           true,
}

// ValidateSystemActor checks that a system actor name is in the whitelist.
func ValidateSystemActor(name string) error {
	if !SystemActorWhitelist[name] {
		return fmt.Errorf("policy: system actor %q not in whitelist", name)
	}
	return nil
}
