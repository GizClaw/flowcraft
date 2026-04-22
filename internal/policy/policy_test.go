package policy

import (
	"context"
	"testing"
)

// stubRealm is a deterministic RealmStore used across the rule tests. It
// avoids reaching into a SQLite store so the tests stay focused on the
// policy logic itself.
type stubRealm struct {
	Members               map[string]string // userID -> realmID with role admin
	RuntimeOwnerByID      map[string]string
	AgentInRuntime        map[string]map[string]bool
	RealmOfRuntimeByID    map[string]string
	RealmAdminByUserRealm map[string]bool
}

func (s *stubRealm) IsMember(realmID, userID, _ string) bool {
	return s.Members[userID] == realmID
}
func (s *stubRealm) RuntimeOwner(_ context.Context, runtimeID string) (string, error) {
	return s.RuntimeOwnerByID[runtimeID], nil
}
func (s *stubRealm) IsAgentInRuntime(_ context.Context, agentID, runtimeID string) (bool, error) {
	if m, ok := s.AgentInRuntime[runtimeID]; ok {
		return m[agentID], nil
	}
	return false, nil
}
func (s *stubRealm) RealmOfRuntime(_ context.Context, runtimeID string) (string, error) {
	return s.RealmOfRuntimeByID[runtimeID], nil
}
func (s *stubRealm) RealmAdminOf(_ context.Context, realmID, userID string) (bool, error) {
	return s.RealmAdminByUserRealm[userID+":"+realmID], nil
}

func newPolicy() *DefaultPolicy {
	return NewDefaultPolicy(&stubRealm{
		RuntimeOwnerByID:   map[string]string{"rt-1": "u-owner"},
		AgentInRuntime:     map[string]map[string]bool{"rt-1": {"a-1": true}},
		RealmOfRuntimeByID: map[string]string{"rt-1": "realm-1"},
	})
}

func TestAllowAppend_RuntimeRules(t *testing.T) {
	p := newPolicy()
	ctx := context.Background()

	cases := []struct {
		name  string
		actor Actor
		evt   EnvelopeDraft
		want  bool
	}{
		{
			"R-RUNTIME-1 allow user submitted",
			Actor{Type: ActorUser, ID: "u-1", RealmID: "realm-1"},
			EnvelopeDraft{Partition: "runtime:rt-1", Type: "task.submitted"}, true,
		},
		{
			"R-RUNTIME-1 deny system submitted",
			Actor{Type: ActorSystem, ID: "sys"},
			EnvelopeDraft{Partition: "runtime:rt-1", Type: "task.submitted"}, false,
		},
		{
			"R-RUNTIME-1 allow agent claim in runtime",
			Actor{Type: ActorAgent, ID: "a-1", RealmID: "realm-1"},
			EnvelopeDraft{Partition: "runtime:rt-1", Type: "task.claimed"}, true,
		},
		{
			"R-RUNTIME-1 deny agent claim wrong realm",
			Actor{Type: ActorAgent, ID: "a-1", RealmID: "realm-other"},
			EnvelopeDraft{Partition: "runtime:rt-1", Type: "task.claimed"}, false,
		},
		{
			"R-RUNTIME-1 deny user claim",
			Actor{Type: ActorUser, ID: "u-1"},
			EnvelopeDraft{Partition: "runtime:rt-1", Type: "task.claimed"}, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec, err := p.AllowAppend(ctx, c.actor, c.evt)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if dec.Allow != c.want {
				t.Fatalf("want %v got %v (%s)", c.want, dec.Allow, dec.Reason)
			}
		})
	}
}

func TestAllowAppend_CardWebhookCronRealmActor(t *testing.T) {
	p := newPolicy()
	ctx := context.Background()
	cases := []struct {
		name  string
		actor Actor
		evt   EnvelopeDraft
		want  bool
	}{
		{"R-CARD-1 allow user chat", Actor{Type: ActorUser, ID: "u"}, EnvelopeDraft{Partition: "card:c1", Type: "chat.message.sent"}, true},
		{"R-CARD-1 deny system chat", Actor{Type: ActorSystem, ID: "sys"}, EnvelopeDraft{Partition: "card:c1", Type: "chat.message.sent"}, false},
		{"R-CARD-1 allow agent stream", Actor{Type: ActorAgent, ID: "a"}, EnvelopeDraft{Partition: "card:c1", Type: "agent.stream.delta"}, true},
		{"R-WEBHOOK-1 allow webhook inbound", Actor{Type: ActorWebhook, ID: "wh"}, EnvelopeDraft{Partition: "webhook:e1", Type: "webhook.inbound.received"}, true},
		{"R-WEBHOOK-1 deny user inbound", Actor{Type: ActorUser, ID: "u"}, EnvelopeDraft{Partition: "webhook:e1", Type: "webhook.inbound.received"}, false},
		{"R-WEBHOOK-1 allow system outbound", Actor{Type: ActorSystem, ID: "sys"}, EnvelopeDraft{Partition: "webhook:e1", Type: "webhook.outbound.queued"}, true},
		{"R-CRON-1 allow user create rule", Actor{Type: ActorUser, ID: "u"}, EnvelopeDraft{Partition: "cron:r1", Type: "cron.rule.created"}, true},
		{"R-CRON-1 allow system fire rule", Actor{Type: ActorSystem, ID: "sys"}, EnvelopeDraft{Partition: "cron:r1", Type: "cron.rule.fired"}, true},
		{"R-CRON-1 deny user fire rule", Actor{Type: ActorUser, ID: "u"}, EnvelopeDraft{Partition: "cron:r1", Type: "cron.rule.fired"}, false},
		{"R-REALM-1 allow user member added", Actor{Type: ActorUser, ID: "u"}, EnvelopeDraft{Partition: "realm:r1", Type: "realm.member.added"}, true},
		{"R-REALM-1 deny api_key realm", Actor{Type: ActorAPIKey, ID: "k"}, EnvelopeDraft{Partition: "realm:r1", Type: "realm.member.added"}, false},
		{"R-ACTOR-1 allow super audit", Actor{Type: ActorUser, ID: "su", Super: true}, EnvelopeDraft{Partition: "actor:u-1", Type: "audit.action.performed"}, true},
		{"R-ACTOR-1 allow system audit", Actor{Type: ActorSystem, ID: "sys"}, EnvelopeDraft{Partition: "actor:u-1", Type: "audit.action.performed"}, true},
		{"R-ACTOR-1 deny normal user audit", Actor{Type: ActorUser, ID: "u"}, EnvelopeDraft{Partition: "actor:u-1", Type: "audit.action.performed"}, false},
		{"unknown partition format", Actor{Type: ActorUser, ID: "u"}, EnvelopeDraft{Partition: "bogus:x", Type: "task.submitted"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec, err := p.AllowAppend(ctx, c.actor, c.evt)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if dec.Allow != c.want {
				t.Fatalf("want %v got %v (%s)", c.want, dec.Allow, dec.Reason)
			}
		})
	}
}

func TestAllowSubscribe_AdminAndPerPartition(t *testing.T) {
	p := newPolicy()
	ctx := context.Background()

	cases := []struct {
		name  string
		actor Actor
		opts  SubscribeOptions
		want  bool
	}{
		{"R-ADMIN-1 deny no partition for normal user", Actor{Type: ActorUser, ID: "u"}, SubscribeOptions{}, false},
		{"R-ADMIN-1 allow no partition for super", Actor{Type: ActorUser, Super: true, ID: "su"}, SubscribeOptions{}, true},
		{"R-RUNTIME-1 allow user runtime", Actor{Type: ActorUser, ID: "u"}, SubscribeOptions{Partitions: []string{"runtime:rt-1"}}, true},
		{"R-RUNTIME-1 deny system runtime", Actor{Type: ActorSystem, ID: "sys"}, SubscribeOptions{Partitions: []string{"runtime:rt-1"}}, false},
		{"R-APIKEY-1 allow scoped runtime", Actor{Type: ActorAPIKey, ID: "k", Scopes: []string{"read:runtime"}}, SubscribeOptions{Partitions: []string{"runtime:rt-1"}}, true},
		{"R-APIKEY-1 deny unscoped runtime", Actor{Type: ActorAPIKey, ID: "k"}, SubscribeOptions{Partitions: []string{"runtime:rt-1"}}, false},
		{"R-CARD-1 allow user card", Actor{Type: ActorUser, ID: "u"}, SubscribeOptions{Partitions: []string{"card:c1"}}, true},
		{"R-CARD-1 deny api_key without scope", Actor{Type: ActorAPIKey, ID: "k"}, SubscribeOptions{Partitions: []string{"card:c1"}}, false},
		{"R-WEBHOOK-1 deny api_key", Actor{Type: ActorAPIKey, ID: "k"}, SubscribeOptions{Partitions: []string{"webhook:e1"}}, false},
		{"R-CRON-1 allow user", Actor{Type: ActorUser, ID: "u"}, SubscribeOptions{Partitions: []string{"cron:r1"}}, true},
		{"R-REALM-1 allow user", Actor{Type: ActorUser, ID: "u"}, SubscribeOptions{Partitions: []string{"realm:rl1"}}, true},
		{"R-ACTOR-1 deny normal user", Actor{Type: ActorUser, ID: "u"}, SubscribeOptions{Partitions: []string{"actor:u-1"}}, false},
		{"R-ACTOR-1 allow super", Actor{Type: ActorUser, Super: true, ID: "su"}, SubscribeOptions{Partitions: []string{"actor:u-1"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec, err := p.AllowSubscribe(ctx, c.actor, c.opts)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if dec.Allow != c.want {
				t.Fatalf("want %v got %v (%s)", c.want, dec.Allow, dec.Reason)
			}
		})
	}
}

func TestAllowRead_DelegatesToSubscribe(t *testing.T) {
	p := newPolicy()
	ctx := context.Background()
	dec, err := p.AllowRead(ctx, Actor{Type: ActorUser, ID: "u"}, ReadOptions{Partitions: []string{"runtime:rt-1"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !dec.Allow {
		t.Fatalf("expected allow, got: %s", dec.Reason)
	}
}

func TestSystemActorWhitelist(t *testing.T) {
	if err := ValidateSystemActor("cron_scheduler"); err != nil {
		t.Fatal("expected cron_scheduler to be allowed")
	}
	if err := ValidateSystemActor("evil_actor"); err == nil {
		t.Fatal("expected unknown system actor to be rejected")
	}
}

func TestActorScopes(t *testing.T) {
	a := Actor{Type: ActorAPIKey, ID: "k", Scopes: []string{"read:card"}}
	if !a.HasScope("read:card") {
		t.Fatal("expected scope to be present")
	}
	if a.HasScope("read:runtime") {
		t.Fatal("expected unrelated scope to be absent")
	}
	user := Actor{Type: ActorUser, ID: "u"}
	if !user.HasScope("anything") {
		t.Fatal("user actor should have all scopes implicitly")
	}
}
