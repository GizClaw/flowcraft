package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Secret is an owned, non-serializable resolved credential value. Bytes
// returns a copy so callers cannot mutate the value retained by a provider
// factory.
type Secret struct {
	value []byte
}

func NewSecret(value []byte) (Secret, error) {
	if len(value) == 0 {
		return Secret{}, fmt.Errorf("resolved secret is empty")
	}
	return Secret{value: bytes.Clone(value)}, nil
}

func (s Secret) Bytes() []byte {
	return bytes.Clone(s.value)
}

func (Secret) String() string   { return "<redacted>" }
func (Secret) GoString() string { return "<redacted>" }

func (Secret) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("resolved secrets cannot be serialized")
}

func (Secret) MarshalText() ([]byte, error) {
	return nil, fmt.Errorf("resolved secrets cannot be serialized")
}

type SecretResolver interface {
	Resolve(context.Context, string) (Secret, error)
}

type SecretResolverFunc func(context.Context, string) (Secret, error)

func (f SecretResolverFunc) Resolve(
	ctx context.Context,
	key string,
) (Secret, error) {
	return f(ctx, key)
}

var _ json.Marshaler = Secret{}
