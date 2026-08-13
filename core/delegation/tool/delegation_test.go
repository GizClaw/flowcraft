package delegation_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/core/delegation"
	tooldelegation "github.com/GizClaw/flowcraft/core/delegation/tool"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

type toolTestSource struct {
	tools []tool.Tool
}

func (s toolTestSource) Tools() []tool.Tool         { return s.tools }
func (s toolTestSource) LazyTools() []tool.LazyTool { return nil }

func toolCatalog(t *testing.T, tools ...tool.Tool) *tool.Registry {
	t.Helper()
	reg, err := tool.NewRegistry([]tool.Source{toolTestSource{tools: tools}})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestDelegateDefinitionUsesCurrentDirectoryTargets(t *testing.T) {
	directory := &fakeDirectory{targets: []sdkdelegation.Target{{
		ID:          "billing",
		Description: "Refunds and invoices",
	}}}
	delegate := tooldelegation.NewDelegate(directory)
	registry := toolCatalog(t, delegate)
	definition := registry.Definitions()[0]
	assertTargetEnum(t, definition, []string{"billing"})
	if !strings.Contains(definition.Description, "Refunds and invoices") {
		t.Fatalf("description = %q", definition.Description)
	}

	directory.targets = []sdkdelegation.Target{{ID: "technical"}}
	assertTargetEnum(t, registry.Definitions()[0], []string{"technical"})
}

func TestDelegateDefinitionFallsBackWhenDirectoryUnavailable(t *testing.T) {
	for name, directory := range map[string]*fakeDirectory{
		"unbound": {listErr: errdefs.NotAvailablef("not bound")},
		"empty":   {},
	} {
		t.Run(name, func(t *testing.T) {
			definition := tooldelegation.NewDelegate(directory).Definition()
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			var target map[string]any
			if err := json.Unmarshal(schema.Properties["target"], &target); err != nil {
				t.Fatal(err)
			}
			if _, hasEnum := target["enum"]; hasEnum {
				t.Fatalf("fallback target schema contains enum: %s", schema.Properties["target"])
			}
		})
	}
}

type blockingDirectory struct {
	*fakeDirectory
}

func (b *blockingDirectory) List(ctx context.Context) ([]sdkdelegation.Target, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDelegateDefinitionBoundsDirectoryLookup(t *testing.T) {
	directory := &blockingDirectory{fakeDirectory: &fakeDirectory{
		targets: []sdkdelegation.Target{{ID: "billing"}},
	}}
	delegate := tooldelegation.NewDelegate(directory)

	start := time.Now()
	definition := delegate.Definition()
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("Definition blocked for %v, want bounded lookup", elapsed)
	}
	if definition.Name != sdkdelegation.ToolName {
		t.Fatalf("definition = %q", definition.Name)
	}
}

func TestDelegateExecuteModesAndNoteMetadata(t *testing.T) {
	service := &fakeService{delegate: func(req sdkdelegation.Request) (sdkdelegation.Response, error) {
		switch req.Mode {
		case sdkdelegation.ModeSync:
			return sdkdelegation.Response{
				ID:       "sync-1",
				Status:   sdkdelegation.StatusSucceeded,
				Output:   "done",
				Metadata: map[string]string{"worker": "billing"},
			}, nil
		case sdkdelegation.ModeAsync:
			return sdkdelegation.Response{ID: "async-1", Status: sdkdelegation.StatusAccepted}, nil
		default:
			return sdkdelegation.Response{}, errors.New("unexpected mode")
		}
	}}
	ctx := serviceContext(service)
	delegate := tooldelegation.NewDelegate(&fakeDirectory{})

	tests := []struct {
		name string
		args string
		want string
	}{
		{
			name: "sync",
			args: `{"mode":"sync","target":"billing","input":"refund","metadata":{"trace":"t1"},"note":"urgent"}`,
			want: `{"delegation_id":"sync-1","status":"succeeded","output":"done","metadata":{"worker":"billing"}}`,
		},
		{
			name: "async",
			args: `{"mode":"async","target":"billing","input":"refund"}`,
			want: `{"delegation_id":"async-1","status":"accepted"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := delegate.Execute(ctx, test.args)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got != test.want {
				t.Fatalf("output = %s, want %s", got, test.want)
			}
		})
	}

	if got := service.requests[0].Metadata; !reflect.DeepEqual(got, map[string]string{
		"trace":                        "t1",
		tooldelegation.NoteMetadataKey: "urgent",
	}) {
		t.Fatalf("metadata = %#v", got)
	}
	if _, err := delegate.Execute(ctx, `{"mode":"sync","target":"billing","input":"refund","note":""}`); err != nil {
		t.Fatalf("Execute empty note: %v", err)
	}
	if got, ok := service.requests[2].Metadata[tooldelegation.NoteMetadataKey]; !ok || got != "" {
		t.Fatalf("empty note metadata = %q, found = %v", got, ok)
	}
}

func TestDelegateRejectsMissingHostUnknownTargetAndStrictArguments(t *testing.T) {
	delegate := tooldelegation.NewDelegate(&fakeDirectory{})
	if _, err := delegate.Execute(context.Background(), `{"mode":"sync","target":"billing","input":"refund"}`); !errdefs.IsNotAvailable(err) {
		t.Fatalf("missing host error = %v, want NotAvailable", err)
	}
	hostContext := agent.ContextWithHost(context.Background(), agent.NoopHost{})
	if _, err := delegate.Execute(hostContext, `{"mode":"sync","target":"billing","input":"refund"}`); !errdefs.IsNotAvailable(err) {
		t.Fatalf("missing service error = %v, want NotAvailable", err)
	}

	service := &fakeService{delegate: func(req sdkdelegation.Request) (sdkdelegation.Response, error) {
		return sdkdelegation.Response{}, sdkdelegation.TargetNotFound(req.Target)
	}}
	if _, err := delegate.Execute(serviceContext(service), `{"mode":"sync","target":"unknown","input":"refund"}`); !errdefs.IsNotFound(err) {
		t.Fatalf("unknown target error = %v, want NotFound", err)
	}
	for _, arguments := range []string{
		`{"mode":"sync","target":"billing","input":"refund","extra":true}`,
		`{"mode":"sync","target":"billing","input":"refund"} {}`,
		`{"mode":"sync","target":"billing","input":"refund","metadata":{"attempt":1}}`,
	} {
		if _, err := delegate.Execute(serviceContext(service), arguments); !errdefs.IsValidation(err) {
			t.Fatalf("strict arguments %q error = %v, want Validation", arguments, err)
		}
	}
}

func TestDelegationStatusMapsEveryStatus(t *testing.T) {
	statusTool := tooldelegation.NewStatus()
	statuses := []sdkdelegation.Status{
		sdkdelegation.StatusAccepted,
		sdkdelegation.StatusRunning,
		sdkdelegation.StatusSucceeded,
		sdkdelegation.StatusFailed,
		sdkdelegation.StatusCanceled,
	}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			response := sdkdelegation.Response{ID: "job-1", Status: status}
			switch status {
			case sdkdelegation.StatusSucceeded:
				response.Output = "done"
			case sdkdelegation.StatusFailed:
				response.Error = "failed"
			case sdkdelegation.StatusCanceled:
				response.Error = "canceled"
			}
			service := &fakeService{get: func(string) (sdkdelegation.Response, error) {
				return response, nil
			}}
			output, err := statusTool.Execute(serviceContext(service), `{"delegation_id":"job-1"}`)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(output), &got); err != nil {
				t.Fatal(err)
			}
			if got["delegation_id"] != "job-1" || got["status"] != string(status) {
				t.Fatalf("output = %s", output)
			}
			for _, forbidden := range []string{"card", "board", "claim"} {
				if _, found := got[forbidden]; found {
					t.Fatalf("output exposes %q: %s", forbidden, output)
				}
			}
		})
	}
}

func TestDelegationStatusRequiresHostAndStrictArguments(t *testing.T) {
	statusTool := tooldelegation.NewStatus()
	if _, err := statusTool.Execute(context.Background(), `{"delegation_id":"job-1"}`); !errdefs.IsNotAvailable(err) {
		t.Fatalf("missing host error = %v, want NotAvailable", err)
	}
	service := &fakeService{get: func(string) (sdkdelegation.Response, error) {
		return sdkdelegation.Response{}, sdkdelegation.RequestNotFound("job-1")
	}}
	for _, arguments := range []string{
		`{"delegation_id":"job-1","extra":true}`,
		`{"delegation_id":"job-1"} {}`,
		`{"delegation_id":""}`,
	} {
		if _, err := statusTool.Execute(serviceContext(service), arguments); !errdefs.IsValidation(err) {
			t.Fatalf("strict arguments %q error = %v, want Validation", arguments, err)
		}
	}
}

func assertTargetEnum(t *testing.T, definition message.ToolDefinition, want []string) {
	t.Helper()
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties["target"].Enum; !reflect.DeepEqual(got, want) {
		t.Fatalf("target enum = %v, want %v", got, want)
	}
}

type fakeDirectory struct {
	targets []sdkdelegation.Target
	listErr error
}

func (d *fakeDirectory) List(context.Context) ([]sdkdelegation.Target, error) {
	return slices.Clone(d.targets), d.listErr
}

func (d *fakeDirectory) Get(_ context.Context, id string) (sdkdelegation.Target, error) {
	for _, target := range d.targets {
		if target.ID == id {
			return target, nil
		}
	}
	return sdkdelegation.Target{}, sdkdelegation.TargetNotFound(id)
}

type fakeService struct {
	delegate func(sdkdelegation.Request) (sdkdelegation.Response, error)
	get      func(string) (sdkdelegation.Response, error)
	requests []sdkdelegation.Request
}

func (s *fakeService) Delegate(_ context.Context, req sdkdelegation.Request) (sdkdelegation.Response, error) {
	s.requests = append(s.requests, req)
	return s.delegate(req)
}

func (s *fakeService) Get(_ context.Context, id string) (sdkdelegation.Response, error) {
	return s.get(id)
}

func serviceContext(service sdkdelegation.Service) context.Context {
	host := sdkdelegation.WithService(agent.NoopHost{}, service)
	return agent.ContextWithHost(context.Background(), host)
}

func TestTargetsToolListsCurrentTargets(t *testing.T) {
	directory := &fakeDirectory{targets: []sdkdelegation.Target{
		{ID: "billing", Description: "Refunds and invoices", Modes: []sdkdelegation.Mode{sdkdelegation.ModeSync, sdkdelegation.ModeAsync}},
		{ID: "technical", Description: "Infra"},
	}}
	targets := tooldelegation.NewTargets(directory)
	out, err := targets.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, `"billing"`) || !strings.Contains(out, `"technical"`) {
		t.Fatalf("output = %s, want both targets", out)
	}
	if !strings.Contains(out, `"modes"`) {
		t.Fatalf("output = %s, want mode info", out)
	}
}

func TestTargetsToolRejectsArgumentsAndMissingDirectory(t *testing.T) {
	if _, err := (tooldelegation.NewTargets(&fakeDirectory{})).Execute(
		context.Background(), `{}`); !errdefs.IsValidation(err) {
		t.Fatalf("Execute with args = %v, want Validation", err)
	}
	if _, err := (tooldelegation.NewTargets(nil)).Execute(
		context.Background(), ""); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Execute without directory = %v, want NotAvailable", err)
	}
}
