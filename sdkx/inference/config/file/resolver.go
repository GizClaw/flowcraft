// Package file resolves secret references from mounted secret files, the
// layout used by Kubernetes secret volumes, Docker secrets, and systemd
// credentials. File bytes are returned exactly as stored — trailing
// newlines are not trimmed, because trimming could corrupt binary
// credentials; provider factories decide whether to trim.
package file

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/sdkx/inference/config"
)

// Resolver reads secret values from files. A nil root accepts any path;
// a non-empty root confines keys to that directory after symlink
// resolution, so a compromised configuration cannot exfiltrate arbitrary
// host files through SecretRef keys.
type Resolver struct {
	root     string
	readFile func(string) ([]byte, error)
}

// New returns a resolver that reads absolute or working-directory-relative
// paths. Prefer NewInDir for deployments with a fixed secret mount.
func New() Resolver {
	return Resolver{readFile: os.ReadFile}
}

// NewInDir returns a resolver that confines keys to root: keys may be
// absolute or relative, but the cleaned, symlink-resolved target must stay
// inside the resolved root.
func NewInDir(root string) (Resolver, error) {
	if root == "" {
		return Resolver{}, fmt.Errorf("file secret root directory is required")
	}
	resolved, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return Resolver{}, fmt.Errorf("resolve file secret root: %w", err)
	}
	if evaluated, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
		resolved = evaluated
	}
	return Resolver{root: resolved, readFile: os.ReadFile}, nil
}

// NewWithReader supports deterministic tests and application-owned file
// abstractions.
func NewWithReader(readFile func(string) ([]byte, error)) Resolver {
	return Resolver{readFile: readFile}
}

func (r Resolver) Resolve(
	ctx context.Context,
	key string,
) (config.Secret, error) {
	if err := ctx.Err(); err != nil {
		return config.Secret{}, err
	}
	if key == "" {
		return config.Secret{}, fmt.Errorf("file secret path is required")
	}
	if r.readFile == nil {
		return config.Secret{}, fmt.Errorf(
			"file secret resolver has no read function",
		)
	}
	path, err := r.confine(key)
	if err != nil {
		return config.Secret{}, err
	}
	data, err := r.readFile(path)
	if err != nil {
		return config.Secret{}, fmt.Errorf("read file secret %q: %w", key, err)
	}
	secret, err := config.NewSecret(data)
	if err != nil {
		return config.Secret{}, fmt.Errorf("file secret %q: %w", key, err)
	}
	return secret, nil
}

func (r Resolver) confine(key string) (string, error) {
	if r.root == "" {
		return key, nil
	}
	path := key
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.root, path)
	}
	resolved, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve file secret %q: %w", key, err)
	}
	if evaluated, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
		resolved = evaluated
	}
	// A missing file keeps its cleaned path: the read below reports the
	// not-found error, and the confinement check still applies to it.
	if resolved != r.root &&
		!strings.HasPrefix(resolved, r.root+string(filepath.Separator)) {
		return "", fmt.Errorf(
			"file secret %q escapes the secret root directory",
			key,
		)
	}
	return resolved, nil
}

var _ config.SecretResolver = Resolver{}
