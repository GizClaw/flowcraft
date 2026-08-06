// Package env resolves secret references from process environment variables.
package env

import (
	"context"
	"fmt"
	"os"
	"regexp"

	"github.com/GizClaw/flowcraft/sdk/inference/config"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Resolver struct {
	lookup func(string) (string, bool)
}

func New() Resolver {
	return Resolver{lookup: os.LookupEnv}
}

// NewWithLookup supports deterministic tests and application-owned
// environment abstractions.
func NewWithLookup(lookup func(string) (string, bool)) Resolver {
	return Resolver{lookup: lookup}
}

func (r Resolver) Resolve(
	ctx context.Context,
	key string,
) (config.Secret, error) {
	if err := ctx.Err(); err != nil {
		return config.Secret{}, err
	}
	if !keyPattern.MatchString(key) {
		return config.Secret{}, fmt.Errorf(
			"environment secret key %q is invalid",
			key,
		)
	}
	if r.lookup == nil {
		return config.Secret{}, fmt.Errorf(
			"environment secret resolver has no lookup function",
		)
	}
	value, ok := r.lookup(key)
	if !ok {
		return config.Secret{}, fmt.Errorf(
			"environment secret %q is not set",
			key,
		)
	}
	secret, err := config.NewSecret([]byte(value))
	if err != nil {
		return config.Secret{}, fmt.Errorf(
			"environment secret %q: %w",
			key,
			err,
		)
	}
	return secret, nil
}

var _ config.SecretResolver = Resolver{}
