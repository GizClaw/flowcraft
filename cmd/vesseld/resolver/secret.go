package resolver

import (
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// SecretLookup mirrors catalog.SecretLookup; both sides hold the
// interface so the resolver can construct one and the catalog can
// consume it without an import cycle.
type SecretLookup interface {
	Get(secretName, key string) (string, error)
}

// secretIndex is the resolver-internal SecretLookup implementation.
// Built once during Resolve from every Secret document; consumed
// by valueRef resolution + handed to factories via Deps.
type secretIndex struct {
	data map[string]map[string]string // secretName → key → plainValue
}

// newSecretIndex builds the lookup from the loaded Secret list.
// Returns errdefs.Validation when any Secret's MergedData is
// invalid (decoder normally caught this; defence in depth).
func newSecretIndex(secrets []v1alpha1.Secret) (*secretIndex, error) {
	idx := &secretIndex{data: make(map[string]map[string]string, len(secrets))}
	for _, s := range secrets {
		merged, err := s.MergedData()
		if err != nil {
			return nil, err
		}
		idx.data[s.Name] = merged
	}
	return idx, nil
}

func (s *secretIndex) Get(secretName, key string) (string, error) {
	bucket, ok := s.data[secretName]
	if !ok {
		return "", errdefs.NotFoundf("vesseld: Secret %q not loaded", secretName)
	}
	v, ok := bucket[key]
	if !ok {
		return "", errdefs.NotFoundf("vesseld: Secret %q has no key %q", secretName, key)
	}
	return v, nil
}

// resolveValueRef turns a v1alpha1.ValueRef into the plain-text
// value it points at. opts.AllowFile / opts.AllowSecret control
// which sources are honoured; `vesseld validate` sets both to
// false to keep validation IO-free.
func resolveValueRef(ref v1alpha1.ValueRef, lookup SecretLookup, opts ResolveOptions, fieldName string) (string, error) {
	if err := ref.Validate(fieldName); err != nil {
		return "", err
	}
	src := ref.ValueFrom
	switch {
	case src.Env != "":
		// In validate-only mode (no IO), skip the env lookup so
		// `vesseld validate` runs cleanly on a CI box that does
		// not carry production credentials.
		if !opts.AllowSecret && !opts.AllowFile {
			return "", nil
		}
		v, ok := os.LookupEnv(src.Env)
		if !ok {
			return "", errdefs.Validationf("vesseld: %s.valueFrom.env %q is not set in the daemon environment", fieldName, src.Env)
		}
		return v, nil
	case src.File != "":
		if !opts.AllowFile {
			return "", nil // validate-only mode: skip the read but do not error
		}
		raw, err := os.ReadFile(src.File)
		if err != nil {
			return "", errdefs.Validationf("vesseld: %s.valueFrom.file %q: %v", fieldName, src.File, err)
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	case src.SecretRef != nil:
		if !opts.AllowSecret {
			return "", nil
		}
		if lookup == nil {
			return "", errdefs.Validationf("vesseld: %s.valueFrom.secretRef requires loaded Secret docs", fieldName)
		}
		return lookup.Get(src.SecretRef.Name, src.SecretRef.Key)
	default:
		// Validate above already rejected this; defence in depth.
		return "", errdefs.Validationf("vesseld: %s.valueFrom is empty", fieldName)
	}
}

// ResolveOptions controls how Resolve handles IO-touching value
// sources. The defaults (everything true) suit `vesseld run`;
// `vesseld validate` sets AllowFile / AllowSecret false so it
// finishes quickly even when files are missing.
type ResolveOptions struct {
	AllowFile   bool
	AllowSecret bool
}

// DefaultResolveOptions returns the runtime-friendly options.
func DefaultResolveOptions() ResolveOptions {
	return ResolveOptions{AllowFile: true, AllowSecret: true}
}
