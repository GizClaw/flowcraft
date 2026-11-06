package runtime

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	toolmcp "github.com/GizClaw/flowcraft/core/tool-mcp"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

// Backend names accepted by [SandboxConfig.Backend].
const (
	BackendLocal    = "local"
	BackendSeatbelt = "seatbelt"
	BackendBwrap    = "bwrap"
	BackendWindows  = "windows"
)

// Tool names accepted by [ToolsConfig.Enable].
const (
	ToolFiles  = "files"
	ToolExec   = "exec"
	ToolSkills = "skills"
)

// Config is one runtime's configuration document.
//
// It is a mirror of the contract types, not a serialization of them: the values
// are the ones a person writes (mebibytes, mode names, paths), and [Config]
// converts them. That keeps YAML out of the contracts' public API and puts the
// validation where the units are still visible.
type Config struct {
	// Version is the schema version. It is required so a future format change
	// has an anchor to reject or migrate against, instead of being discovered
	// as a field that silently means something else.
	Version   int             `yaml:"version"`
	Name      string          `yaml:"name"`
	Sandbox   SandboxConfig   `yaml:"sandbox"`
	Files     FilesConfig     `yaml:"files"`
	Inference InferenceConfig `yaml:"inference"`
	Tools     ToolsConfig     `yaml:"tools"`
	Skills    SkillsConfig    `yaml:"skills"`
}

// SandboxConfig describes the process boundary: which backend enforces it, what
// it may write, how it reaches the network, and what it may consume.
type SandboxConfig struct {
	// Backend is one of local, seatbelt, bwrap, windows. There is no default:
	// a boundary that gets picked by guessing is a boundary nobody chose.
	Backend string `yaml:"backend"`
	// WorkDir is the directory commands run in. Empty means the first files
	// root: a deployment that lists one root should not have to say it twice.
	WorkDir string `yaml:"workdir"`
	// Writes lists absolute paths a command may write. Empty means read-only.
	Writes []string `yaml:"writes"`
	// Net is the outbound posture.
	Net NetConfig `yaml:"net"`
	// CPU is a millicore budget; zero means unbounded.
	CPU int `yaml:"cpu_millicores"`
	// MemoryMiB caps address space; zero means unbounded.
	MemoryMiB int `yaml:"memory_mib"`
	// OutputMiB caps captured output per call; zero means the backend default.
	OutputMiB int `yaml:"output_mib"`
	// Journal records filesystem events under the runner root.
	Journal bool `yaml:"journal"`
}

// NetConfig is the outbound posture in the words a person uses.
type NetConfig struct {
	// Mode is one of default, none, allow_list, proxy.
	Mode string `yaml:"mode"`
	// AllowHosts is meaningful for allow_list: hostname suffixes and IP
	// literals, optionally with a port.
	AllowHosts []string `yaml:"allow_hosts"`
	// Proxy is the address used by proxy mode.
	Proxy string `yaml:"proxy"`
}

// FilesConfig describes the file boundary the tools work inside.
type FilesConfig struct {
	// Roots are absolute directories writes may touch. They must exist when the
	// runtime is built: a handle that cannot be opened cannot enforce anything.
	Roots []string `yaml:"roots"`
}

// InferenceConfig declares the models this runtime may call and how it fails
// over between them.
//
// The list is ordered: position is priority, so the deployment states its
// preference once instead of encoding it in weights or in a selector. A model
// appears once per deployment it can run in — the same API model twice, with two
// credentials, is the ordinary failover shape.
type InferenceConfig struct {
	// Providers are reusable construction blocks: one driver, one set of
	// credentials and endpoints, many models. Declaring them separately keeps a
	// deployment from repeating the same api_key and base_url for every model.
	Providers []ProviderConfig `yaml:"providers"`
	// Models are callable endpoints: a provider, an API model name, and the
	// facts the driver's catalog does not declare. Order is priority.
	Models []ModelConfig `yaml:"models"`
	Route  RouteConfig   `yaml:"route"`
}

// ProviderConfig is one construction block: which driver, with which settings,
// under which name the deployment refers to it.
type ProviderConfig struct {
	// Name is the deployment identity: it becomes model.ModelRef.Profile and the
	// target name, and it is what distinguishes the same vendor model deployed
	// through two accounts.
	Name string `yaml:"name"`
	// Driver is the constructor, resolved by the host's driver table. Its name is
	// also the public vendor identity, model.ModelID.Provider.
	Driver string `yaml:"driver"`
	// Settings is passed to the driver verbatim.
	Settings map[string]any `yaml:"settings"`
}

// ModelConfig is one callable model: which API model, through which driver, with
// which credentials.
type ModelConfig struct {
	// Provider names the construction block this model runs on. The block owns
	// the driver, the credentials and the endpoints; one block serves many
	// models.
	Provider string `yaml:"provider"`
	// Model is the API model name: model.ModelID.Name.
	Model string `yaml:"model"`
	// Weight is used by selectors that weigh targets; the order of this list is
	// the default preference.
	Weight int `yaml:"weight"`
	// Capabilities and Limits fill in what the driver's catalog does not
	// declare. A declaration that contradicts the catalog fails assembly.
	Capabilities *CapabilitiesConfig `yaml:"capabilities"`
	Limits       *LimitsConfig       `yaml:"limits"`
}

// CapabilitiesConfig is the written form of model.ModelCapabilities. Only the
// content kinds are expressible: the remaining bits are driver facts.
type CapabilitiesConfig struct {
	Inputs  []string `yaml:"inputs"`
	Outputs []string `yaml:"outputs"`
}

// LimitsConfig is the written form of model.ModelLimits; zero means undeclared.
type LimitsConfig struct {
	MaxInputTokens  int   `yaml:"max_input_tokens"`
	MaxOutputTokens int   `yaml:"max_output_tokens"`
	MaxInputBytes   int64 `yaml:"max_input_bytes"`
}

// RouteConfig mirrors route.RetryPolicy plus a selector name.
type RouteConfig struct {
	// Selector names a selector from the host's table. Empty means "every
	// candidate, in the order models declares".
	Selector string `yaml:"selector"`
	// SelectorSettings is handed to the named selector's factory verbatim; the
	// runtime does not interpret it.
	SelectorSettings map[string]any `yaml:"selector_settings"`
	// AttemptsPerTarget is how often one model is retried on a transient
	// failure; zero means once.
	AttemptsPerTarget int `yaml:"attempts_per_target"`
	// MaxTargets caps how many models are tried; zero means every candidate.
	MaxTargets int `yaml:"max_targets"`
	// BaseDelay is the first backoff, doubling per attempt; empty means no wait.
	BaseDelay string `yaml:"base_delay"`
	// MaxDelay caps the backoff; empty means no cap.
	MaxDelay string `yaml:"max_delay"`
}

// ToolsConfig selects which built-in tools are wired.
type ToolsConfig struct {
	// Enable lists files, exec and skills. Empty means all three.
	Enable []string `yaml:"enable"`
	// TTY publishes pseudo-terminal parameters on the session tool.
	TTY bool `yaml:"tty"`
	// Dynamic defers tools instead of sending all of them every round. Omitted
	// means static: every wired tool is visible to the model from the first
	// round, which is the behaviour a small tool set wants.
	Dynamic *DynamicConfig `yaml:"dynamic"`
	// Images lets read_file return an image part. It belongs here because it
	// describes what a tool may hand the model, not what the filesystem is or
	// what the driver supports: a deployment that lists it wrong gets refused
	// parts, and the fix is a tool setting.
	Images bool `yaml:"images"`
	// MCP declares MCP servers, which are one more tool source. Declaring a
	// server is what enables it; there is no separate name in enable.
	MCP MCPConfig `yaml:"mcp"`
	// Middleware lists the dispatch chain, outermost first. Omitted means
	// [recover, telemetry]: a tool call should produce a span and must not be
	// able to stop the process.
	Middleware []MiddlewareConfig `yaml:"middleware"`
}

// MiddlewareConfig names one entry of the dispatch chain and its settings.
type MiddlewareConfig struct {
	Name     string         `yaml:"name"`
	Settings map[string]any `yaml:"settings"`
}

// DynamicConfig enables deferred tool exposure: the model sees a bounded set
// each round and calls tool_search to load what else it needs.
//
// It mirrors engine.Policy rather than exposing it: a policy is a contract
// value, and giving its fields serialization tags would make YAML part of its
// public API. It also omits the policy's deprecated fields, so there is exactly
// one way to express the idle horizon.
type DynamicConfig struct {
	// Default is the exposure for tools with no explicit entry: always, direct,
	// deferred or hidden. Omitted means deferred, the engine's default.
	Default string `yaml:"default"`
	// Always names tools that stay visible every round.
	Always []string `yaml:"always"`
	// Hidden names tools that never appear; tool_search cannot surface them.
	Hidden []string `yaml:"hidden"`
	// Budget caps how many definitions one round can carry.
	Budget int `yaml:"budget"`
	// MaxBytes caps the serialized size of one round's definitions.
	MaxBytes int `yaml:"max_bytes"`
	// IdleRounds evicts a discovered tool that goes unused this long.
	IdleRounds int `yaml:"idle_rounds"`
	// SearchWithLoad makes tool_search load every deferred source before
	// ranking. Off by default: a search must not spawn processes it does not
	// need.
	SearchWithLoad bool `yaml:"search_with_load"`
}

// SkillsConfig points at SKILL.md trees.
type SkillsConfig struct {
	Roots []string `yaml:"roots"`
}

// Validate rejects a configuration that cannot be built, with the YAML path in
// every error so the reader knows which line to change.
func (c Config) Validate() error {
	if err := c.Sandbox.validate(); err != nil {
		return err
	}
	if err := c.Files.validate(); err != nil {
		return err
	}
	if err := c.validateVersion(); err != nil {
		return err
	}
	if err := c.Tools.validate(); err != nil {
		return err
	}

	if err := c.Inference.validate(); err != nil {
		return err
	}
	for _, root := range c.Skills.Roots {
		if !filepath.IsAbs(root) {
			return errdefs.Validationf("skills.roots: %q is not an absolute path", root)
		}
	}
	return nil
}

func (c SandboxConfig) validate() error {
	switch c.Backend {
	case BackendLocal, BackendSeatbelt, BackendBwrap, BackendWindows:
	case "":
		return errdefs.Validationf(
			"sandbox.backend is required; one of %s, %s, %s, %s",
			BackendLocal, BackendSeatbelt, BackendBwrap, BackendWindows)
	default:
		return errdefs.Validationf("sandbox.backend: unknown backend %q", c.Backend)
	}
	if c.WorkDir != "" && !filepath.IsAbs(c.WorkDir) {
		return errdefs.Validationf("sandbox.workdir: %q is not an absolute path", c.WorkDir)
	}
	if err := (permission.Writes{Paths: c.Writes}).Validate(); err != nil {
		return errdefs.Validationf("sandbox.writes: %w", err)
	}
	if c.CPU < 0 || c.MemoryMiB < 0 || c.OutputMiB < 0 {
		return errdefs.Validationf(
			"sandbox: cpu_millicores, memory_mib and output_mib must not be negative")
	}
	return c.Net.validate()
}

func (n NetConfig) validate() error {
	if _, err := n.mode(); err != nil {
		return err
	}
	if len(n.AllowHosts) > 0 && n.mode0() != permission.NetAllowList {
		return errdefs.Validationf(
			"net.allow_hosts is only meaningful with net.mode: allow_list (got %q)", n.Mode)
	}
	return nil
}

func (n NetConfig) mode0() permission.NetMode {
	mode, _ := n.mode()
	return mode
}

// mode maps the written name to the contract value.
func (n NetConfig) mode() (permission.NetMode, error) {
	switch strings.ToLower(strings.TrimSpace(n.Mode)) {
	case "", "default":
		return permission.NetDefault, nil
	case "none", "deny", "deny_all":
		return permission.NetDenyAll, nil
	case "allow_list", "allowlist":
		return permission.NetAllowList, nil
	case "proxy", "proxies":
		return permission.NetProxy, nil
	default:
		return permission.NetDefault, errdefs.Validationf(
			"net.mode: unknown mode %q; one of default, none, allow_list, proxy", n.Mode)
	}
}

// policy converts the written network posture into the contract value.
func (n NetConfig) policy() (permission.NetPolicy, error) {
	mode, err := n.mode()
	if err != nil {
		return permission.NetPolicy{}, err
	}
	policy := permission.NetPolicy{Mode: mode, AllowHosts: n.AllowHosts, Proxy: n.Proxy}
	if err := policy.Validate(); err != nil {
		return permission.NetPolicy{}, errdefs.Validationf("net: %w", err)
	}
	return policy, nil
}

func (f FilesConfig) validate() error {
	if len(f.Roots) == 0 {
		return errdefs.Validationf(
			"files.roots is required: at least one absolute directory the tools may write")
	}
	for _, root := range f.Roots {
		if !filepath.IsAbs(root) {
			return errdefs.Validationf("files.roots: %q is not an absolute path", root)
		}
	}
	if err := (permission.Writes{Paths: f.Roots}).Validate(); err != nil {
		return errdefs.Validationf("files.roots: %w", err)
	}
	return nil
}

func (t ToolsConfig) validate() error {
	if t.Dynamic != nil {
		if err := t.Dynamic.validate(); err != nil {
			return err
		}
	}
	if err := t.MCP.validate(); err != nil {
		return err
	}
	for _, name := range t.Enable {
		switch name {
		case ToolFiles, ToolExec, ToolSkills:
		default:
			return errdefs.Validationf(
				"tools.enable: unknown tool %q; one of %s, %s, %s",
				name, ToolFiles, ToolExec, ToolSkills)
		}
	}
	return nil
}

// enabled reports whether one built-in tool set is wired. An empty list means
// every set: a runtime assembled with no tools would be a configuration that
// looks complete and does nothing.
func (t ToolsConfig) enabled(name string) bool {
	if len(t.Enable) == 0 {
		return true
	}
	for _, candidate := range t.Enable {
		if candidate == name {
			return true
		}
	}
	return false
}

// workDir is the directory commands run in.
func (c Config) workDir() string {
	if c.Sandbox.WorkDir != "" {
		return c.Sandbox.WorkDir
	}
	if len(c.Files.Roots) > 0 {
		return c.Files.Roots[0]
	}
	return ""
}

// permission converts the sandbox section into the contract policy.
func (c SandboxConfig) permission() (permission.Policy, error) {
	writes := permission.Writes{Paths: c.Writes}
	if err := writes.Validate(); err != nil {
		return permission.Policy{}, errdefs.Validationf("sandbox.writes: %w", err)
	}
	net, err := c.Net.policy()
	if err != nil {
		return permission.Policy{}, err
	}
	return permission.Policy{Writes: writes, Net: net}, nil
}

// limits converts the written budgets into the contract limits.
func (c SandboxConfig) limits() (sandbox.ResourceLimits, error) {
	if c.CPU < 0 || c.MemoryMiB < 0 || c.OutputMiB < 0 {
		return sandbox.ResourceLimits{}, errdefs.Validationf(
			"sandbox: cpu_millicores, memory_mib and output_mib must not be negative")
	}
	return sandbox.ResourceLimits{
		CPUMillicores:  c.CPU,
		MemoryBytes:    int64(c.MemoryMiB) << 20,
		MaxOutputBytes: int64(c.OutputMiB) << 20,
	}, nil
}

func (d DynamicConfig) validate() error {
	if d.Default != "" {
		if _, err := parseExposure(d.Default); err != nil {
			return errdefs.Validationf("tools.dynamic.default: %w", err)
		}
	}
	if d.Budget < 0 || d.MaxBytes < 0 || d.IdleRounds < 0 {
		return errdefs.Validationf(
			"tools.dynamic: budget, max_bytes and idle_rounds must not be negative")
	}
	always := make(map[string]struct{}, len(d.Always))
	for _, name := range d.Always {
		always[name] = struct{}{}
	}
	for _, name := range d.Hidden {
		if _, both := always[name]; both {
			return errdefs.Validationf(
				"tools.dynamic: %q is listed in both always and hidden", name)
		}
	}
	return nil
}

// parseExposure maps the written name to the engine's exposure level.
func parseExposure(name string) (engine.Exposure, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case string(engine.ExposureAlways):
		return engine.ExposureAlways, nil
	case string(engine.ExposureDirect):
		return engine.ExposureDirect, nil
	case string(engine.ExposureDeferred):
		return engine.ExposureDeferred, nil
	case string(engine.ExposureHidden):
		return engine.ExposureHidden, nil
	default:
		return "", errdefs.Validationf(
			"unknown exposure %q; one of always, direct, deferred, hidden", name)
	}
}

// policy converts the written block into the engine's policy. Zero fields are
// left zero on purpose: the engine normalizes them against DefaultPolicy, and a
// second default here would drift from it.
func (d DynamicConfig) policy() (engine.Policy, error) {
	policy := engine.Policy{
		Budget:         engine.Budget{MaxDefinitions: d.Budget, MaxBytes: int64(d.MaxBytes)},
		Discovery:      engine.DiscoveryPolicy{IdleRounds: d.IdleRounds},
		SearchWithLoad: d.SearchWithLoad,
	}
	if d.Default != "" {
		exposure, err := parseExposure(d.Default)
		if err != nil {
			return engine.Policy{}, errdefs.Validationf("tools.dynamic.default: %w", err)
		}
		policy.Default = exposure
	}
	if len(d.Always) > 0 || len(d.Hidden) > 0 {
		policy.Exposures = make(map[string]engine.Exposure, len(d.Always)+len(d.Hidden))
		for _, name := range d.Always {
			policy.Exposures[name] = engine.ExposureAlways
		}
		for _, name := range d.Hidden {
			policy.Exposures[name] = engine.ExposureHidden
		}
	}
	return policy, nil
}

// MCPConfig declares the MCP servers a runtime attaches.
type MCPConfig struct {
	// Defer starts a server only when a tool from it is loaded. Omitted means
	// true: a configured server that the model never uses should not cost a
	// process or a connection.
	Defer   *bool             `yaml:"defer"`
	Servers []MCPServerConfig `yaml:"servers"`
}

// deferred reports whether servers start on demand.
func (m MCPConfig) deferred() bool {
	if m.Defer == nil {
		return true
	}
	return *m.Defer
}

// MCPServerConfig is one server attachment. The pointers keep "absent" distinct
// from "false": an absent value keeps the package default, an explicit value
// overrides it.
type MCPServerConfig struct {
	Name        string            `yaml:"name"`
	Transport   string            `yaml:"transport"`
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	Env         map[string]string `yaml:"env"`
	URL         string            `yaml:"url"`
	Headers     map[string]string `yaml:"headers"`
	HTTPTimeout string            `yaml:"http_timeout"`
	Liveness    string            `yaml:"liveness"`
	Prefix      string            `yaml:"prefix"`
	// Description is what a model sees before the server is connected. It is
	// the only thing search can rank on, because the real tools are unknown
	// until the server starts.
	Description string `yaml:"description"`
	Resources   *bool  `yaml:"resources"`
	Required    *bool  `yaml:"required"`
}

func (m MCPConfig) validate() error {
	if len(m.Servers) == 0 {
		return nil
	}
	if _, err := m.spec(); err != nil {
		return err
	}
	return nil
}

// spec converts the written servers into the MCP package's own spec.
//
// Validation is delegated: the MCP package already rejects the combinations it
// cannot honour (stdio without a command, http with one), and duplicating those
// rules here would let the two copies drift.
func (m MCPConfig) spec() (toolmcp.Spec, error) {
	spec := toolmcp.Spec{Servers: make([]toolmcp.ServerSpec, 0, len(m.Servers))}
	for _, server := range m.Servers {
		converted := toolmcp.ServerSpec{
			Name:        server.Name,
			Transport:   server.Transport,
			Command:     server.Command,
			Args:        server.Args,
			Env:         server.Env,
			URL:         server.URL,
			Headers:     server.Headers,
			Resources:   server.Resources,
			Required:    server.Required,
			Description: server.Description,
		}
		if server.HTTPTimeout != "" {
			value := server.HTTPTimeout
			converted.HTTPTimeout = &value
		}
		if server.Liveness != "" {
			value := server.Liveness
			converted.Liveness = &value
		}
		if server.Prefix != "" {
			value := server.Prefix
			converted.Prefix = &value
		}
		spec.Servers = append(spec.Servers, converted)
	}
	if err := spec.Validate(); err != nil {
		return toolmcp.Spec{}, errdefs.Validationf("mcp: %w", err)
	}
	return spec, nil
}

func (c Config) validateVersion() error {
	switch c.Version {
	case 1:
		return nil
	case 0:
		return errdefs.Validationf(
			"version is required; this build understands version 1")
	default:
		return errdefs.Validationf(
			"unsupported version %d; this build understands version 1", c.Version)
	}
}

func (c InferenceConfig) validate() error {
	if len(c.Models) == 0 {
		if c.Route.Selector != "" {
			return errdefs.Validationf(
				"inference.route.selector: no models are declared, so there is nothing to select")
		}
		return nil
	}
	providers := make(map[string]ProviderConfig, len(c.Providers))
	for i, provider := range c.Providers {
		if strings.TrimSpace(provider.Name) == "" {
			return errdefs.Validationf("inference.providers[%d]: name is required", i)
		}
		if _, duplicate := providers[provider.Name]; duplicate {
			return errdefs.Validationf(
				"inference.providers[%d]: duplicate name %q", i, provider.Name)
		}
		if strings.TrimSpace(provider.Driver) == "" {
			return errdefs.Validationf(
				"inference.providers[%s].driver is required", provider.Name)
		}
		providers[provider.Name] = provider
	}

	used := make(map[string]struct{}, len(c.Providers))
	seen := make(map[string]struct{}, len(c.Models))
	for i, entry := range c.Models {
		provider, declared := providers[entry.Provider]
		if !declared {
			return errdefs.Validationf(
				"inference.models[%d].provider: %q is not a declared provider; declared: %s",
				i, entry.Provider, strings.Join(providerNames(c.Providers), ", "))
		}
		used[entry.Provider] = struct{}{}
		key := entry.Provider + "|" + entry.Model
		if _, duplicate := seen[key]; duplicate {
			return errdefs.Validationf(
				"inference.models[%d]: provider %q already serves model %q",
				i, entry.Provider, entry.Model)
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(entry.Model) == "" {
			return errdefs.Validationf("inference.models[%d]: model is required", i)
		}
		if entry.Weight < 0 {
			return errdefs.Validationf(
				"inference.models[%d]: weight must not be negative", i)
		}
		if _, err := entry.descriptor(provider); err != nil {
			return errdefs.Validationf("inference.models[%d]: %w", i, err)
		}
	}
	for _, provider := range c.Providers {
		if _, ok := used[provider.Name]; !ok {
			return errdefs.Validationf(
				"inference.providers[%s]: declared but serves no model", provider.Name)
		}
	}
	return c.Route.validate()
}

// lookupProvider finds a declared block by name.
func (c InferenceConfig) lookupProvider(name string) (ProviderConfig, bool) {
	for _, provider := range c.Providers {
		if provider.Name == name {
			return provider, true
		}
	}
	return ProviderConfig{}, false
}

func providerNames(providers []ProviderConfig) []string {
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, provider.Name)
	}
	sort.Strings(names)
	return names
}

func (r RouteConfig) validate() error {
	if r.AttemptsPerTarget < 0 || r.MaxTargets < 0 {
		return errdefs.Validationf(
			"inference.route: attempts_per_target and max_targets must not be negative")
	}
	for name, value := range map[string]string{"base_delay": r.BaseDelay, "max_delay": r.MaxDelay} {
		if value == "" {
			continue
		}
		if _, err := time.ParseDuration(value); err != nil {
			return errdefs.Validationf("inference.route.%s: %v", name, err)
		}
	}
	return nil
}

// retry converts the written backoff into the dispatcher's policy.
func (r RouteConfig) retry() (route.RetryPolicy, error) {
	policy := route.RetryPolicy{
		AttemptsPerTarget: r.AttemptsPerTarget,
		MaxTargets:        r.MaxTargets,
	}
	if r.BaseDelay != "" {
		delay, err := time.ParseDuration(r.BaseDelay)
		if err != nil {
			return route.RetryPolicy{}, errdefs.Validationf("inference.route.base_delay: %v", err)
		}
		policy.BaseDelay = delay
	}
	if r.MaxDelay != "" {
		delay, err := time.ParseDuration(r.MaxDelay)
		if err != nil {
			return route.RetryPolicy{}, errdefs.Validationf("inference.route.max_delay: %v", err)
		}
		policy.MaxDelay = delay
	}
	return policy, nil
}

// descriptor converts one entry into the model facts routing and the compiler
// consult.
func (m ModelConfig) descriptor(provider ProviderConfig) (model.ModelDescriptor, error) {
	// The driver name is the public vendor identity, so the descriptor needs no
	// restamping later: it is already what a catalog reports.
	id := model.ModelID{Provider: provider.Driver, Name: m.Model}
	descriptor := model.ModelDescriptor{ID: id}
	if m.Capabilities != nil {
		inputs, err := parsePartKinds(m.Capabilities.Inputs)
		if err != nil {
			return model.ModelDescriptor{}, errdefs.Validationf("capabilities.inputs: %w", err)
		}
		outputs, err := parsePartKinds(m.Capabilities.Outputs)
		if err != nil {
			return model.ModelDescriptor{}, errdefs.Validationf("capabilities.outputs: %w", err)
		}
		descriptor.Capabilities.Inputs = inputs
		descriptor.Capabilities.Outputs = outputs
	}
	if m.Limits != nil {
		if m.Limits.MaxInputTokens > 0 {
			value := m.Limits.MaxInputTokens
			descriptor.Limits.MaxInputTokens = &value
		}
		if m.Limits.MaxOutputTokens > 0 {
			value := m.Limits.MaxOutputTokens
			descriptor.Limits.MaxOutputTokens = &value
		}
		if m.Limits.MaxInputBytes > 0 {
			value := m.Limits.MaxInputBytes
			descriptor.Limits.MaxInputBytes = &value
		}
	}
	return descriptor, nil
}

// targetName is the deployment identity shown in traces and errors:
// profile/vendor:model, with the profile omitted when there is only one
// deployment of that model.
func (m ModelConfig) targetName() string {
	return m.Provider + ":" + m.Model
}

// parsePartKinds maps the written content kinds to the contract vocabulary.
func parsePartKinds(names []string) ([]message.PartKind, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]message.PartKind, 0, len(names))
	for _, name := range names {
		var kind message.PartKind
		switch strings.ToLower(strings.TrimSpace(name)) {
		case string(message.PartText):
			kind = message.PartText
		case string(message.PartImage):
			kind = message.PartImage
		case string(message.PartAudio):
			kind = message.PartAudio
		case string(message.PartVideo):
			kind = message.PartVideo
		case string(message.PartFile):
			kind = message.PartFile
		case string(message.PartData):
			kind = message.PartData
		default:
			return nil, errdefs.Validationf("unknown content kind %q", name)
		}
		out = append(out, kind)
	}
	return out, nil
}
