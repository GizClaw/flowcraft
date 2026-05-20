package governance

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

type rejectAll struct{}

func (rejectAll) Apply(domain.TemporalFact) (domain.TemporalFact, bool) {
	return domain.TemporalFact{}, false
}

func TestDefault_AllowsFacts(t *testing.T) {
	g := Default()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	f := domain.TemporalFact{Kind: domain.KindNote, Content: "ok"}
	_, ok := g.ApplyWrite(context.Background(), scope, f, time.Now())
	if !ok {
		t.Fatal("default governance must not block")
	}
}

func TestGovernance_WritePolicyReject(t *testing.T) {
	g := Default()
	g.Write = rejectAll{}
	_, ok := g.ApplyWrite(context.Background(), domain.Scope{RuntimeID: "rt"}, domain.TemporalFact{Kind: domain.KindNote}, time.Now())
	if ok {
		t.Fatal("reject write policy must block fact")
	}
}
