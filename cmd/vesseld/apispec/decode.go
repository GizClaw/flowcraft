// Package apispec is the version-router shim around the per-version
// wire schema sub-packages. apispec/decode.go is the only place
// callers should reach for at the top level: they pass in raw YAML
// bytes (or an io.Reader of YAML / JSON), apispec routes by
// apiVersion+kind, and returns a typed Object the resolver can
// switch over.
//
// Why the indirection: when v1alpha2 lands, only this file changes
// (one new case in the apiVersion switch). Callers — loader,
// resolver, validate CLI — never touch a version literal directly
// once they hold the Object interface.
package apispec

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/sdk/errdefs"

	"gopkg.in/yaml.v3"
)

// Object is the version-agnostic interface every wire-level kind
// implements. Re-exported from v1alpha1.Object for caller
// convenience: holding apispec.Object means callers do not need to
// import the per-version sub-package just to type-assert.
type Object = v1alpha1.Object

// DecodeAll reads YAML / JSON from r, splits multi-document YAML
// streams on `---` boundaries, and returns each parsed Object plus
// a per-document source location string ("file:doc-index" — the
// loader prefixes the file path before display).
//
// docName is the human label used when the input has no filename
// (e.g. STDIN / inline test data); the loader passes the actual
// path so error messages can point at the file directly.
func DecodeAll(r io.Reader, docName string) ([]Object, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("vesseld: read %s: %w", docName, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var out []Object
	for idx := 0; ; idx++ {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, errdefs.Validationf("vesseld: parse %s doc[%d]: %v", docName, idx, err)
		}
		// yaml.v3 yields a Document node for an empty `---`
		// separator (Kind == DocumentNode, no content). Skip
		// these so callers can author readable files with leading
		// / trailing separators without spurious decode errors.
		if isEmptyNode(&node) {
			continue
		}
		obj, err := decodeNode(&node)
		if err != nil {
			return nil, fmt.Errorf("vesseld: %s doc[%d]: %w", docName, idx, err)
		}
		out = append(out, obj)
	}
	return out, nil
}

// isEmptyNode returns true for a yaml node that carries no
// meaningful content (Kind 0, or a document/mapping/sequence wrapper
// with zero children). The decoder calls this before attempting to
// extract apiVersion+kind so leading / trailing `---` separators do
// not surface as "missing apiVersion" errors.
func isEmptyNode(node *yaml.Node) bool {
	if node == nil || node.Kind == 0 {
		return true
	}
	if len(node.Content) == 0 {
		return true
	}
	if node.Kind == yaml.DocumentNode {
		// A document wraps exactly one child; empty doc has no
		// children or a single null scalar child.
		if len(node.Content) == 0 {
			return true
		}
		c := node.Content[0]
		if c.Kind == yaml.ScalarNode && (c.Tag == "!!null" || c.Value == "") {
			return true
		}
	}
	return false
}

// decodeNode dispatches on apiVersion + kind. Unknown apiVersion
// is a hard error so users do not silently get a stale schema
// applied to a future configuration. Unknown kind under a known
// apiVersion is also a hard error for the same reason.
func decodeNode(node *yaml.Node) (Object, error) {
	var probe struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := node.Decode(&probe); err != nil {
		return nil, errdefs.Validationf("vesseld: cannot read apiVersion/kind: %v", err)
	}
	if probe.APIVersion == "" {
		return nil, errdefs.Validationf("vesseld: document missing apiVersion")
	}
	if probe.Kind == "" {
		return nil, errdefs.Validationf("vesseld: document missing kind")
	}
	switch probe.APIVersion {
	case v1alpha1.APIVersion:
		return decodeV1Alpha1(node, probe.Kind)
	default:
		return nil, errdefs.Validationf("vesseld: unsupported apiVersion %q (known: %s)", probe.APIVersion, v1alpha1.APIVersion)
	}
}

// decodeV1Alpha1 is the per-version dispatcher. Adding a new kind
// only needs a new case here plus its struct file under v1alpha1/.
func decodeV1Alpha1(node *yaml.Node, kind string) (Object, error) {
	switch kind {
	case v1alpha1.KindDaemon:
		var d v1alpha1.Daemon
		if err := node.Decode(&d); err != nil {
			return nil, errdefs.Validationf("vesseld: decode Daemon: %v", err)
		}
		return d, nil
	case v1alpha1.KindVessel:
		var v v1alpha1.Vessel
		if err := node.Decode(&v); err != nil {
			return nil, errdefs.Validationf("vesseld: decode Vessel: %v", err)
		}
		return v, nil
	case v1alpha1.KindAgent:
		var a v1alpha1.Agent
		if err := node.Decode(&a); err != nil {
			return nil, errdefs.Validationf("vesseld: decode Agent: %v", err)
		}
		return a, nil
	case v1alpha1.KindLLMProfile:
		var l v1alpha1.LLMProfile
		if err := node.Decode(&l); err != nil {
			return nil, errdefs.Validationf("vesseld: decode LLMProfile: %v", err)
		}
		return l, nil
	case v1alpha1.KindProbe:
		var p v1alpha1.Probe
		if err := node.Decode(&p); err != nil {
			return nil, errdefs.Validationf("vesseld: decode Probe: %v", err)
		}
		return p, nil
	case v1alpha1.KindToolPack:
		var t v1alpha1.ToolPack
		if err := node.Decode(&t); err != nil {
			return nil, errdefs.Validationf("vesseld: decode ToolPack: %v", err)
		}
		return t, nil
	case v1alpha1.KindHistoryStore:
		var h v1alpha1.HistoryStore
		if err := node.Decode(&h); err != nil {
			return nil, errdefs.Validationf("vesseld: decode HistoryStore: %v", err)
		}
		return h, nil
	case v1alpha1.KindSecret:
		var s v1alpha1.Secret
		if err := node.Decode(&s); err != nil {
			return nil, errdefs.Validationf("vesseld: decode Secret: %v", err)
		}
		return s, nil
	case v1alpha1.KindSandbox:
		var sb v1alpha1.Sandbox
		if err := node.Decode(&sb); err != nil {
			return nil, errdefs.Validationf("vesseld: decode Sandbox: %v", err)
		}
		return sb, nil
	default:
		return nil, errdefs.Validationf("vesseld: unsupported kind %q under %s (known: %v)", kind, v1alpha1.APIVersion, v1alpha1.AllKinds())
	}
}
