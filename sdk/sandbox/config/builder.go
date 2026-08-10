package config

import (
	"context"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	coresandbox "github.com/GizClaw/flowcraft/sdk/sandbox"
	workspaceconfig "github.com/GizClaw/flowcraft/sdk/workspace/config"
)

// Deps supplies application-owned resources that the document cannot
// express.
type Deps struct {
	Workspaces *workspaceconfig.Registry
	Approver   coresandbox.ApprovalFunc
}

// FactoryInput is the host workspace root plus backend-owned settings.
type FactoryInput struct {
	sdkconfig.Input
	Root string
}

// Factory constructs one backend runner.
type Factory = sdkconfig.Func[FactoryInput, coresandbox.Runner]

// Builder owns an instance-local backend factory catalog.
type Builder struct {
	deps     Deps
	backends *sdkconfig.Registry[FactoryInput, coresandbox.Runner]
}

// NewBuilder creates a builder with the local backend registered.
// Platform backends (seatbelt, bubblewrap, ...) register themselves from
// their own packages.
func NewBuilder(deps Deps) *Builder {
	b := &Builder{
		deps:     deps,
		backends: sdkconfig.NewRegistry[FactoryInput, coresandbox.Runner](),
	}
	b.registerBuiltins()
	return b
}

// RegisterFactory adds a custom backend factory.
func (b *Builder) RegisterFactory(backend string, factory Factory) error {
	if b == nil {
		return errdefs.Validationf("sandbox config: builder is nil")
	}
	if err := b.backends.Register(backend, factory); err != nil {
		return errdefs.Validationf("sandbox config: %v", err)
	}
	return nil
}

// Build constructs every sandbox and returns an immutable registry.
func (b *Builder) Build(ctx context.Context, doc Document) (_ *Registry, err error) {
	if b == nil {
		return nil, errdefs.Validationf("sandbox config: builder is nil")
	}
	if ctx == nil {
		return nil, errdefs.Validationf("sandbox config: context is nil")
	}
	if b.deps.Workspaces == nil {
		return nil, errdefs.Validationf(
			"sandbox config: Workspaces dependency is required")
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}

	runners := make(map[string]coresandbox.Runner, len(doc.Sandboxes))
	var closers []func() error
	defer func() {
		if err != nil {
			err = joinCloseError(err, closeAll(closers))
		}
	}()
	for _, name := range sortedKeys(doc.Sandboxes) {
		entry := doc.Sandboxes[name]
		if _, ok := b.deps.Workspaces.Get(entry.Workspace); !ok {
			return nil, errdefs.Validationf(
				"sandbox config sandboxes[%q]: unknown workspace %q",
				name, entry.Workspace)
		}
		root, ok := b.deps.Workspaces.Root(entry.Workspace)
		if !ok || root == "" {
			return nil, errdefs.Validationf(
				"sandbox config sandboxes[%q]: workspace %q has no host root",
				name, entry.Workspace)
		}
		if entry.Approval.hasPredicates() && b.deps.Approver == nil {
			return nil, errdefs.Validationf(
				"sandbox config sandboxes[%q]: approval predicates require an Approver in config.Deps",
				name)
		}

		backend, err := b.backends.Build(ctx, entry.Backend, FactoryInput{
			Input: sdkconfig.Input{Settings: entry.Settings},
			Root:  root,
		})
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil, errdefs.Validationf(
					"sandbox config sandboxes[%q]: unknown backend %q",
					name, entry.Backend)
			}
			return nil, classifyFactoryError(fmt.Errorf(
				"sandbox config sandboxes[%q] (%s): %w",
				name, entry.Backend, err))
		}
		if isNilRunner(backend) {
			return nil, errdefs.Validationf(
				"sandbox config sandboxes[%q] (%s): factory returned nil runner",
				name, entry.Backend)
		}
		if closer, ok := backend.(io.Closer); ok {
			closers = append(closers, closer.Close)
		}

		defaults := toExecOptions(entry.Defaults)
		if err := validateEnforcement(name, backend, defaults); err != nil {
			return nil, err
		}
		policy := coresandbox.LocalPolicy{
			Defaults:        defaults,
			AllowedCommands: slices.Clone(entry.AllowedCommands),
			Approval:        b.deps.Approver,
			Predicates:      approvalPredicates(root, entry.Approval),
		}
		runners[name] = coresandbox.ComposeLocal(backend, policy)
	}
	return newRegistry(runners, closers), nil
}

func validateEnforcement(
	name string,
	runner coresandbox.Runner,
	defaults coresandbox.ExecOptions,
) error {
	enforcement := coresandbox.EnforcementOf(runner)
	if defaults.Env.Allow != nil && !enforcement.EnvAllowList {
		return unsupportedPolicy(name, "environment allow-list")
	}
	if defaults.Net.Mode != coresandbox.NetDefault &&
		!slices.Contains(enforcement.NetModes, defaults.Net.Mode) {
		return unsupportedPolicy(name,
			fmt.Sprintf("network mode %q", netModeName(defaults.Net.Mode)))
	}
	if strings.HasPrefix(defaults.Net.Proxy, "socks5://") && !enforcement.Socks5 {
		return unsupportedPolicy(name, "socks5 upstream")
	}
	if defaults.Net.MITM != nil && defaults.Net.MITM.Enabled && !enforcement.MITM {
		return unsupportedPolicy(name, "MITM")
	}
	if len(defaults.Net.UnixSockets) > 0 && !enforcement.UnixSocketPolicy {
		return unsupportedPolicy(name, "unix socket allow-list")
	}
	if defaults.Resources.MemoryBytes > 0 && !enforcement.MemoryCap {
		return unsupportedPolicy(name, "memory cap")
	}
	if defaults.Resources.CPUMillicores > 0 && !enforcement.CPUCap {
		return unsupportedPolicy(name, "CPU cap")
	}
	if defaults.Resources.DiskBytes > 0 && !enforcement.DiskCap {
		return unsupportedPolicy(name, "disk cap")
	}
	return nil
}

func unsupportedPolicy(name, policy string) error {
	return errdefs.NotAvailablef(
		"sandbox config sandboxes[%q]: backend cannot enforce configured %s",
		name, policy)
}

func netModeName(mode coresandbox.NetMode) string {
	switch mode {
	case coresandbox.NetDefault:
		return NetModeDefault
	case coresandbox.NetDenyAll:
		return NetModeDenyAll
	case coresandbox.NetAllowList:
		return NetModeAllowList
	case coresandbox.NetProxy:
		return NetModeProxy
	default:
		return fmt.Sprintf("%d", mode)
	}
}

func toExecOptions(value Defaults) coresandbox.ExecOptions {
	return coresandbox.ExecOptions{
		Timeout: time.Duration(value.Timeout),
		Env: coresandbox.EnvPolicy{
			Allow:  slices.Clone(value.Env.Allow),
			Inject: maps.Clone(value.Env.Inject),
		},
		Net: coresandbox.NetPolicy{
			Mode:        toNetMode(value.Net.Mode),
			AllowHosts:  slices.Clone(value.Net.AllowHosts),
			Rules:       toNetRules(value.Net.Rules),
			Proxy:       value.Net.Proxy,
			UnixSockets: slices.Clone(value.Net.UnixSockets),
			MITM:        toMITMPolicy(value.Net.MITM),
		},
		Resources: coresandbox.ResourceLimits{
			CPUMillicores:  value.Resources.CPUMillicores,
			MemoryBytes:    value.Resources.MemoryBytes,
			DiskBytes:      value.Resources.DiskBytes,
			MaxOutputBytes: value.Resources.MaxOutputBytes,
		},
	}
}

func toNetRules(rules []NetRuleJSON) []coresandbox.NetRule {
	out := make([]coresandbox.NetRule, 0, len(rules))
	for _, r := range rules {
		action := coresandbox.NetAllow
		if r.Action == "deny" {
			action = coresandbox.NetDeny
		}
		out = append(out, coresandbox.NetRule{
			Action: action,
			Host:   r.Host,
			Port:   r.Port,
		})
	}
	return out
}

func toMITMPolicy(m *MITMJSON) *coresandbox.MITMPolicy {
	if m == nil {
		return nil
	}
	return &coresandbox.MITMPolicy{
		Enabled:       m.Enabled,
		InspectBodies: m.InspectBodies,
		MaxBodyBytes:  m.MaxBodyBytes,
		Hosts:         slices.Clone(m.Hosts),
		ExcludeHosts:  slices.Clone(m.ExcludeHosts),
	}
}

func toNetMode(mode string) coresandbox.NetMode {
	switch mode {
	case "", NetModeDefault:
		return coresandbox.NetDefault
	case NetModeDenyAll:
		return coresandbox.NetDenyAll
	case NetModeAllowList:
		return coresandbox.NetAllowList
	case NetModeProxy:
		return coresandbox.NetProxy
	default:
		panic("validated network mode reached builder: " + mode)
	}
}

func approvalPredicates(root string, approval *ApprovalConfig) []coresandbox.Predicate {
	if approval == nil {
		return nil
	}
	var predicates []coresandbox.Predicate
	if approval.OutsideWorkDir {
		predicates = append(predicates, coresandbox.WorkDirOutsideRoot(root))
	}
	if approval.NonDefaultNetwork {
		predicates = append(predicates, coresandbox.NetNonDefault())
	}
	if approval.Interactive {
		predicates = append(predicates, coresandbox.Interactive())
	}
	if len(approval.SensitiveCommands) > 0 {
		predicates = append(predicates,
			coresandbox.CommandPatterns(
				slices.Clone(approval.SensitiveCommands)...))
	}
	return predicates
}

func classifyFactoryError(err error) error {
	err = errdefs.FromContext(err)
	if errdefs.HasClassification(err) {
		return err
	}
	return errdefs.Validation(err)
}

func isNilRunner(runner coresandbox.Runner) bool {
	if runner == nil {
		return true
	}
	value := reflect.ValueOf(runner)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
