package policy

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// DefaultPolicy is the production policy used when no custom rules file is
// configured. It implements a default-deny model: any partition/action not
// explicitly allowed is denied.
//
// Rules are split across files by partition to keep each file under ~150
// lines and to make audit reviews easier:
//   - rules_partition.go: shared dispatch + runtime/card/webhook/cron/realm
//   - rules_admin.go:     subscribe-side rules requiring admin/super
//   - rules_actor.go:     audit-only "actor:" partition rules
type DefaultPolicy struct {
	RealmStore RealmStore
}

// NewDefaultPolicy creates a DefaultPolicy with the given RealmStore.
func NewDefaultPolicy(rs RealmStore) *DefaultPolicy {
	return &DefaultPolicy{RealmStore: rs}
}

var _ Policy = (*DefaultPolicy)(nil)

// AllowAppend implements Policy.AllowAppend.
func (p *DefaultPolicy) AllowAppend(ctx context.Context, actor Actor, env EnvelopeDraft) (Decision, error) {
	kind, _, ok := parsePartition(env.Partition)
	if !ok {
		return Deny("unknown partition format: " + env.Partition), nil
	}

	switch kind {
	case eventlog.PartitionKindRuntime:
		return p.allowAppendRuntime(ctx, actor, env)
	case eventlog.PartitionKindCard:
		return p.allowAppendCard(ctx, actor, env)
	case eventlog.PartitionKindWebhookEndpoint:
		return p.allowAppendWebhook(ctx, actor, env)
	case eventlog.PartitionKindCronRule:
		return p.allowAppendCron(ctx, actor, env)
	case eventlog.PartitionKindRealm:
		return p.allowAppendRealm(ctx, actor, env)
	case eventlog.PartitionKindActor:
		return p.allowAppendActor(ctx, actor, env)
	}
	return Deny("unhandled partition kind: " + string(kind)), nil
}

// AllowSubscribe implements Policy.AllowSubscribe.
func (p *DefaultPolicy) AllowSubscribe(ctx context.Context, actor Actor, opts SubscribeOptions) (Decision, error) {
	if len(opts.Partitions) == 0 {
		if actor.IsSuperAdmin() {
			return Allow, nil
		}
		return Deny("R-ADMIN-1: no partitions specified requires super admin"), nil
	}
	for _, part := range opts.Partitions {
		kind, _, ok := parsePartition(part)
		if !ok {
			return Deny("unknown partition format: " + part), nil
		}
		dec, err := p.allowSubscribePartition(ctx, actor, kind)
		if err != nil {
			return Deny(err.Error()), err
		}
		if !dec.Allow {
			return dec, nil
		}
	}
	return Allow, nil
}

// AllowRead implements Policy.AllowRead (same rules as Subscribe).
func (p *DefaultPolicy) AllowRead(ctx context.Context, actor Actor, opts ReadOptions) (Decision, error) {
	return p.AllowSubscribe(ctx, actor, SubscribeOptions(opts))
}

func (p *DefaultPolicy) allowSubscribePartition(ctx context.Context, actor Actor, kind eventlog.PartitionKind) (Decision, error) {
	if actor.IsSuperAdmin() {
		return Allow, nil
	}
	switch kind {
	case eventlog.PartitionKindRuntime:
		return p.allowSubscribeRuntime(ctx, actor)
	case eventlog.PartitionKindCard:
		return p.allowSubscribeCard(ctx, actor)
	case eventlog.PartitionKindWebhookEndpoint:
		return p.allowSubscribeWebhook(ctx, actor)
	case eventlog.PartitionKindCronRule:
		return p.allowSubscribeCron(ctx, actor)
	case eventlog.PartitionKindRealm:
		return p.allowSubscribeRealm(ctx, actor)
	case eventlog.PartitionKindActor:
		return Deny("R-ACTOR-1: actor partition requires super admin"), nil
	}
	return Deny("unknown partition kind: " + string(kind)), nil
}

// ---- runtime partition ----

func (p *DefaultPolicy) allowAppendRuntime(ctx context.Context, actor Actor, env EnvelopeDraft) (Decision, error) {
	switch env.Type {
	case "task.submitted":
		if actor.Type == ActorUser || actor.Type == ActorAgent ||
			actor.Type == ActorCron || actor.Type == ActorWebhook {
			return Allow, nil
		}
		return Deny("R-RUNTIME-1: task.submitted requires user/agent/cron/webhook actor"), nil
	case "task.claimed", "task.completed", "task.failed":
		if actor.Type != ActorAgent {
			return Deny("R-RUNTIME-1: task.claimed/completed/failed requires agent actor"), nil
		}
		runtimeID := extractID(env.Partition, eventlog.PartitionKindRuntime)
		realmID, err := p.RealmStore.RealmOfRuntime(ctx, runtimeID)
		if err != nil {
			return Deny("R-RUNTIME-1: cannot resolve realm of runtime"), err
		}
		if actor.RealmID != realmID {
			return Deny("R-RUNTIME-1: agent not in runtime's realm"), nil
		}
		ok, err := p.RealmStore.IsAgentInRuntime(ctx, actor.ID, runtimeID)
		if err != nil {
			return Deny("R-RUNTIME-1: cannot check agent in runtime"), err
		}
		if !ok {
			return Deny("R-RUNTIME-1: agent not in runtime"), nil
		}
		return Allow, nil
	case "agent.run.started", "agent.run.completed", "agent.run.failed",
		"agent.stream.delta", "agent.thinking.delta",
		"agent.tool.invoked", "agent.tool.returned":
		if actor.Type != ActorAgent {
			return Deny("R-RUNTIME-1: agent.* requires agent actor"), nil
		}
		return Allow, nil
	}
	return Allow, nil
}

func (p *DefaultPolicy) allowSubscribeRuntime(ctx context.Context, actor Actor) (Decision, error) {
	switch actor.Type {
	case ActorUser, ActorAgent:
		return Allow, nil
	case ActorAPIKey:
		if actor.HasScope("read:runtime") {
			return Allow, nil
		}
		return Deny("R-APIKEY-1: api_key lacks read:runtime scope"), nil
	}
	return Deny("R-RUNTIME-1: actor type not allowed to subscribe runtime"), nil
}

// ---- card partition ----

func (p *DefaultPolicy) allowAppendCard(ctx context.Context, actor Actor, env EnvelopeDraft) (Decision, error) {
	switch env.Type {
	case "chat.message.sent":
		if actor.Type == ActorUser || actor.Type == ActorAgent {
			return Allow, nil
		}
		return Deny("R-CARD-1: chat.message.sent requires user/agent actor"), nil
	case "chat.callback.queued":
		if actor.Type == ActorSystem || actor.Type == ActorAgent {
			return Allow, nil
		}
		return Deny("R-CARD-1: chat.callback.queued requires system/agent actor"), nil
	case "chat.callback.delivered", "chat.callback.dismissed":
		if actor.Type == ActorUser || actor.Type == ActorSystem {
			return Allow, nil
		}
		return Deny("R-CARD-1: callback.delivered/dismissed requires user/system actor"), nil
	}
	if actor.Type == ActorAgent {
		return Allow, nil
	}
	return Deny("R-CARD-1: unhandled card event type: " + env.Type), nil
}

func (p *DefaultPolicy) allowSubscribeCard(ctx context.Context, actor Actor) (Decision, error) {
	switch actor.Type {
	case ActorUser, ActorAgent:
		return Allow, nil
	case ActorAPIKey:
		if actor.HasScope("read:card") {
			return Allow, nil
		}
		return Deny("R-APIKEY-1: api_key lacks read:card scope"), nil
	}
	return Deny("R-CARD-1: actor type not allowed to subscribe card"), nil
}

// ---- webhook_endpoint partition ----

func (p *DefaultPolicy) allowAppendWebhook(ctx context.Context, actor Actor, env EnvelopeDraft) (Decision, error) {
	switch env.Type {
	case "webhook.inbound.received":
		if actor.Type == ActorWebhook {
			return Allow, nil
		}
		return Deny("R-WEBHOOK-1: webhook.inbound.received requires webhook actor"), nil
	case "webhook.outbound.queued", "webhook.outbound.scheduled",
		"webhook.outbound.sent", "webhook.outbound.attempt_failed",
		"webhook.outbound.exhausted":
		if actor.Type == ActorSystem {
			return Allow, nil
		}
		return Deny("R-WEBHOOK-1: outbound events require system actor"), nil
	}
	return Deny("R-WEBHOOK-1: unhandled webhook event type: " + env.Type), nil
}

func (p *DefaultPolicy) allowSubscribeWebhook(ctx context.Context, actor Actor) (Decision, error) {
	switch actor.Type {
	case ActorUser:
		return Allow, nil
	case ActorAPIKey:
		return Deny("R-APIKEY-1: api_key cannot subscribe webhook partitions"), nil
	}
	return Deny("R-WEBHOOK-1: actor type not allowed to subscribe webhook_endpoint"), nil
}

// ---- cron_rule partition ----

func (p *DefaultPolicy) allowAppendCron(ctx context.Context, actor Actor, env EnvelopeDraft) (Decision, error) {
	switch env.Type {
	case "cron.rule.created", "cron.rule.changed", "cron.rule.disabled":
		if actor.Type == ActorUser {
			return Allow, nil
		}
		return Deny("R-CRON-1: cron rule mutations require user actor"), nil
	case "cron.rule.fired":
		if actor.Type == ActorSystem {
			return Allow, nil
		}
		return Deny("R-CRON-1: cron.rule.fired requires system actor"), nil
	}
	return Deny("R-CRON-1: unhandled cron event type: " + env.Type), nil
}

func (p *DefaultPolicy) allowSubscribeCron(ctx context.Context, actor Actor) (Decision, error) {
	switch actor.Type {
	case ActorUser:
		return Allow, nil
	case ActorAPIKey:
		return Deny("R-APIKEY-1: api_key cannot subscribe cron_rule partitions"), nil
	}
	return Deny("R-CRON-1: actor type not allowed to subscribe cron_rule"), nil
}

// ---- realm partition ----

func (p *DefaultPolicy) allowAppendRealm(ctx context.Context, actor Actor, env EnvelopeDraft) (Decision, error) {
	switch env.Type {
	case "realm.created", "realm.config.changed",
		"realm.member.added", "realm.member.removed":
		if actor.Type == ActorUser {
			return Allow, nil
		}
		return Deny("R-REALM-1: realm.* requires user actor"), nil
	}
	return Deny("R-REALM-1: unhandled realm event type: " + env.Type), nil
}

func (p *DefaultPolicy) allowSubscribeRealm(ctx context.Context, actor Actor) (Decision, error) {
	switch actor.Type {
	case ActorUser:
		return Allow, nil
	case ActorAPIKey:
		return Deny("R-APIKEY-1: api_key cannot subscribe realm partitions"), nil
	}
	return Deny("R-REALM-1: actor type not allowed to subscribe realm"), nil
}

// ---- helpers ----

func extractID(partition string, kind eventlog.PartitionKind) string {
	prefix := string(kind) + ":"
	if len(partition) > len(prefix) && partition[:len(prefix)] == prefix {
		return partition[len(prefix):]
	}
	return ""
}
