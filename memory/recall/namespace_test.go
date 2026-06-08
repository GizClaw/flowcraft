package recall

import "testing"

func TestNamespaceFor(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
		want  string
	}{
		{
			name:  "global scope",
			scope: Scope{RuntimeID: "rt"},
			want:  "recall_rt__global",
		},
		{
			name:  "user scope",
			scope: Scope{RuntimeID: "rt", UserID: "alice"},
			want:  "recall_rt__u5_alice",
		},
		{
			name:  "sanitized runtime and user",
			scope: Scope{RuntimeID: "prod/east", UserID: "bob@example.com"},
			want:  "recall_prod_east__u15_bob_example_com",
		},
		{
			name:  "empty runtime global",
			scope: Scope{},
			want:  "recall_anon__global",
		},
		{
			name:  "empty runtime user",
			scope: Scope{UserID: "alice"},
			want:  "recall_anon__u5_alice",
		},
		{
			name:  "delimiter containing user",
			scope: Scope{RuntimeID: "rt", UserID: "bob__u_alice"},
			want:  "recall_rt__u12_bob__u_alice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NamespaceFor(tt.scope); got != tt.want {
				t.Fatalf("NamespaceFor(%+v) = %q, want %q", tt.scope, got, tt.want)
			}
		})
	}
}
