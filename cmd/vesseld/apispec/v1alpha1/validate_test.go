package v1alpha1

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func tmDaemon() TypeMeta { return TypeMeta{APIVersion: APIVersion, Kind: KindDaemon} }
func tmVessel() TypeMeta { return TypeMeta{APIVersion: APIVersion, Kind: KindVessel} }
func tmAgent() TypeMeta  { return TypeMeta{APIVersion: APIVersion, Kind: KindAgent} }
func tmLLM() TypeMeta    { return TypeMeta{APIVersion: APIVersion, Kind: KindLLMProfile} }

func TestObjectMeta_RejectsEmptyName(t *testing.T) {
	t.Parallel()
	if err := (ObjectMeta{}).Validate(KindDaemon); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestObjectMeta_RejectsForbiddenChar(t *testing.T) {
	t.Parallel()
	if err := (ObjectMeta{Name: "a/b"}).Validate(KindDaemon); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestValueRef_RequiresExactlyOneSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		v    ValueRef
		want bool // want validation error
	}{
		{"nil", ValueRef{}, true},
		{"empty", ValueRef{ValueFrom: &ValueSource{}}, true},
		{"env-only", ValueRef{ValueFrom: &ValueSource{Env: "X"}}, false},
		{"file-only", ValueRef{ValueFrom: &ValueSource{File: "/tmp/x"}}, false},
		{"secretRef-ok", ValueRef{ValueFrom: &ValueSource{SecretRef: &SecretReference{Name: "n", Key: "k"}}}, false},
		{"secretRef-no-key", ValueRef{ValueFrom: &ValueSource{SecretRef: &SecretReference{Name: "n"}}}, true},
		{"two-sources", ValueRef{ValueFrom: &ValueSource{Env: "X", File: "/tmp/y"}}, true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.v.Validate("test.field")
			if tc.want && !errdefs.IsValidation(err) {
				t.Fatalf("expected Validation, got %v", err)
			}
			if !tc.want && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDaemon_ValidateOK(t *testing.T) {
	t.Parallel()
	d := Daemon{
		TypeMeta:   tmDaemon(),
		ObjectMeta: ObjectMeta{Name: "vesseld-default"},
		Spec: DaemonSpec{
			Control:  DaemonControl{Socket: "/tmp/v.sock"},
			Shutdown: DaemonShutdown{DrainTimeout: 30 * time.Second},
		},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDaemon_TCPRequiresToken(t *testing.T) {
	t.Parallel()
	d := Daemon{
		TypeMeta:   tmDaemon(),
		ObjectMeta: ObjectMeta{Name: "d"},
		Spec:       DaemonSpec{Control: DaemonControl{Listen: "0.0.0.0:8080"}},
	}
	if err := d.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestVessel_RequiresAgents(t *testing.T) {
	t.Parallel()
	v := Vessel{TypeMeta: tmVessel(), ObjectMeta: ObjectMeta{Name: "x"}}
	if err := v.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestAgent_SidecarRequiresSubscribe(t *testing.T) {
	t.Parallel()
	a := Agent{
		TypeMeta:   tmAgent(),
		ObjectMeta: ObjectMeta{Name: "a"},
		Spec:       AgentSpec{Sidecar: true, Engine: AgentEngine{Ref: "graph-llm"}},
	}
	if err := a.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestAgent_RequiresEngineRef(t *testing.T) {
	t.Parallel()
	a := Agent{TypeMeta: tmAgent(), ObjectMeta: ObjectMeta{Name: "a"}}
	if err := a.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestLLMProfile_RequiresAuth(t *testing.T) {
	t.Parallel()
	l := LLMProfile{
		TypeMeta:   tmLLM(),
		ObjectMeta: ObjectMeta{Name: "openai"},
		Spec:       LLMProfileSpec{Provider: "openai"},
	}
	if err := l.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestSecret_AcceptsBase64Data(t *testing.T) {
	t.Parallel()
	s := Secret{
		TypeMeta:   TypeMeta{APIVersion: APIVersion, Kind: KindSecret},
		ObjectMeta: ObjectMeta{Name: "s"},
		Spec: SecretSpec{
			Data: map[string]string{"k": "aGVsbG8="}, // "hello"
		},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	merged, err := s.MergedData()
	if err != nil {
		t.Fatalf("MergedData: %v", err)
	}
	if merged["k"] != "hello" {
		t.Fatalf("merged[k] = %q, want hello", merged["k"])
	}
}

func TestSecret_RejectsBadBase64(t *testing.T) {
	t.Parallel()
	s := Secret{
		TypeMeta:   TypeMeta{APIVersion: APIVersion, Kind: KindSecret},
		ObjectMeta: ObjectMeta{Name: "s"},
		Spec:       SecretSpec{Data: map[string]string{"k": "!!!"}},
	}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestSecret_StringDataMergesIntoData(t *testing.T) {
	t.Parallel()
	s := Secret{
		TypeMeta:   TypeMeta{APIVersion: APIVersion, Kind: KindSecret},
		ObjectMeta: ObjectMeta{Name: "s"},
		Spec:       SecretSpec{StringData: map[string]string{"plain": "value"}},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	merged, err := s.MergedData()
	if err != nil {
		t.Fatalf("MergedData: %v", err)
	}
	if merged["plain"] != "value" {
		t.Fatalf("merged[plain] = %q", merged["plain"])
	}
}
