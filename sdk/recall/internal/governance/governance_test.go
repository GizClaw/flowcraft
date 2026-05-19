package governance

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

type rejectAll struct{}

func (rejectAll) Apply(model.TemporalFact) (model.TemporalFact, bool) {
	return model.TemporalFact{}, false
}

func TestDefault_AllowsFacts(t *testing.T) {
	g := Default()
	scope := model.Scope{RuntimeID: "rt", UserID: "u1"}
	f := model.TemporalFact{Kind: model.KindNote, Content: "ok"}
	_, ok := g.ApplyWrite(context.Background(), scope, f, time.Now())
	if !ok {
		t.Fatal("default governance must not block")
	}
}

func TestGovernance_WritePolicyReject(t *testing.T) {
	g := Default()
	g.Write = rejectAll{}
	_, ok := g.ApplyWrite(context.Background(), model.Scope{RuntimeID: "rt"}, model.TemporalFact{Kind: model.KindNote}, time.Now())
	if ok {
		t.Fatal("reject write policy must block fact")
	}
}
