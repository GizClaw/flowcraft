package env

import (
	"context"
	"errors"
	"testing"
)

func TestResolverReadsOwnedSecret(t *testing.T) {
	resolver := NewWithLookup(func(key string) (string, bool) {
		if key != "OPENAI_API_KEY" {
			t.Fatalf("key = %q", key)
		}
		return "secret", true
	})
	secret, err := resolver.Resolve(t.Context(), "OPENAI_API_KEY")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(secret.Bytes()) != "secret" {
		t.Fatalf("secret = %q", secret.Bytes())
	}
}

func TestResolverRejectsMissingEmptyAndCancelled(t *testing.T) {
	tests := []struct {
		name     string
		resolver Resolver
		ctx      context.Context
		key      string
	}{
		{
			name: "missing",
			resolver: NewWithLookup(func(string) (string, bool) {
				return "", false
			}),
			ctx: context.Background(),
			key: "MISSING",
		},
		{
			name: "empty",
			resolver: NewWithLookup(func(string) (string, bool) {
				return "", true
			}),
			ctx: context.Background(),
			key: "EMPTY",
		},
		{
			name:     "invalid key",
			resolver: New(),
			ctx:      context.Background(),
			key:      "not valid",
		},
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests = append(tests, struct {
		name     string
		resolver Resolver
		ctx      context.Context
		key      string
	}{
		name:     "cancelled",
		resolver: New(),
		ctx:      cancelled,
		key:      "OPENAI_API_KEY",
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.resolver.Resolve(tt.ctx, tt.key); err == nil {
				t.Fatal("Resolve accepted invalid environment secret")
			} else if tt.name == "cancelled" &&
				!errors.Is(err, context.Canceled) {
				t.Fatalf("Resolve error = %v", err)
			}
		})
	}
}
