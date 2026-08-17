package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/backends/plugin/service"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils"
)

// Option configures a Loader.
type Option func(*Loader)

// WithCoreVersion sets the host core version used to satisfy
// requires.core constraints. The version may be bare ("0.4.0") or
// v-prefixed ("v0.4.0").
func WithCoreVersion(version string) Option {
	return func(l *Loader) { l.coreVersion = version }
}

// WithTarget sets the registry used for load-time capability conflict
// detection. Defaults to a fresh registry.
func WithTarget(target *Target) Option {
	return func(l *Loader) { l.target = target }
}

// ServicePluginBuilder builds the plugin values backing a service
// artifact during Load. The builder receives the plugin manifest and
// the transport spec derived from the artifact; each returned plugin
// participates in Set.Apply and Set.Close.
type ServicePluginBuilder func(Manifest, service.Spec) ([]Plugin, error)

// WithServicePluginBuilder installs the builder that turns service
// artifacts into runnable plugins (e.g. RPC-backed proxy factories).
func WithServicePluginBuilder(builder ServicePluginBuilder) Option {
	return func(l *Loader) { l.serviceBuilder = builder }
}

// Loader scans plugin directories, parses and validates manifests,
// checks dependencies and capability conflicts, and assembles a Set.
// A Loader is instance-scoped with no global state.
type Loader struct {
	coreVersion    string
	target         *Target
	serviceBuilder ServicePluginBuilder

	mu       sync.Mutex
	loaded   bool
	lastCfg  PluginsConfig
	lastDirs []string
	lastSet  *Set
}

// NewLoader returns a Loader with the given options.
func NewLoader(opts ...Option) *Loader {
	l := &Loader{target: NewTarget()}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// candidate is one discovered plugin directory plus its manifest.
type candidate struct {
	dir      string
	manifest Manifest
}

// Load scans dirs (plus config.Dirs), validates every discovered
// manifest, applies the enabled whitelist when present, checks
// dependencies and (Kind, Impl) conflicts, and returns the assembled
// set. Plugins are ordered by name; layers are sorted by ascending
// priority. The load inputs are retained so Reconcile can re-run
// against the same roots.
func (l *Loader) Load(ctx context.Context, cfg PluginsConfig, dirs ...string) (*Set, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	set, err := l.loadLocked(ctx, cfg, dirs...)
	if err != nil {
		return nil, err
	}
	l.loaded = true
	l.lastCfg = cfg
	l.lastDirs = append([]string(nil), dirs...)
	l.lastSet = set
	return set, nil
}

// Changes describes what a Reconcile detected since the previous
// load, by plugin name.
type Changes struct {
	Added   []string
	Removed []string
	Changed []string
}

// Any reports whether the change set is non-empty.
func (c Changes) Any() bool {
	return len(c.Added) > 0 || len(c.Removed) > 0 || len(c.Changed) > 0
}

// Reconcile re-runs the load against the same plugin directories and
// config and returns a fresh projection. On failure the previous
// projection is retained — the error is returned and the last Set
// stays usable unchanged (the MCP "keep the old projection" pattern).
// When nothing changed, the previous Set is returned with zero
// changes. Configuration changes (dirs, whitelist, loader options) go
// through a fresh Load.
func (l *Loader) Reconcile(ctx context.Context) (*Set, Changes, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.loaded {
		return nil, Changes{}, errdefs.Validationf(
			"plugin: Reconcile requires a prior Load")
	}
	set, err := l.loadLocked(ctx, l.lastCfg, l.lastDirs...)
	if err != nil {
		return nil, Changes{}, err
	}
	changes := diffSets(l.lastSet, set)
	if !changes.Any() {
		return l.lastSet, changes, nil
	}
	l.lastSet = set
	return set, changes, nil
}

func (l *Loader) loadLocked(ctx context.Context, cfg PluginsConfig, dirs ...string) (*Set, error) {
	if err := validatePluginsConfig(cfg); err != nil {
		return nil, err
	}

	var coreVersion string
	if l.coreVersion != "" {
		normalized, err := normalizeVersion(l.coreVersion)
		if err != nil {
			return nil, errdefs.Validationf(
				"plugin: host core version %q: %v", l.coreVersion, err)
		}
		coreVersion = normalized
	}

	enabled, err := parseEnabled(cfg.Enabled)
	if err != nil {
		return nil, err
	}
	if enabled == nil {
		// Explicit-enable semantics: an absent whitelist enables
		// nothing, and no plugin directory is touched.
		return &Set{}, nil
	}

	rootDirs := append(append([]string(nil), cfg.Dirs...), dirs...)
	rootDirs = dedupe(rootDirs)

	candidates, err := l.scan(ctx, rootDirs)
	if err != nil {
		return nil, err
	}

	active := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		if enabled.matches(c.manifest) {
			active = append(active, c)
		}
	}
	if err := checkEnabledMatches(enabled, candidates); err != nil {
		return nil, err
	}

	if err := checkDuplicateNames(active); err != nil {
		return nil, err
	}
	byName := make(map[string]candidate, len(active))
	for _, c := range active {
		byName[c.manifest.Name] = c
	}
	if err := checkDependencies(active, byName, coreVersion); err != nil {
		return nil, err
	}
	if err := checkConflicts(active, l.target.Resources); err != nil {
		return nil, err
	}

	sort.Slice(active, func(i, j int) bool {
		return active[i].manifest.Name < active[j].manifest.Name
	})

	set := &Set{}
	for _, c := range active {
		built, err := l.buildPlugin(ctx, c)
		if err != nil {
			return nil, err
		}
		for _, p := range built {
			set.Plugins = append(set.Plugins, p)
			set.Layers = append(set.Layers, p.Layers()...)
		}
	}
	sort.SliceStable(set.Layers, func(i, j int) bool {
		return set.Layers[i].Priority < set.Layers[j].Priority
	})
	return set, nil
}

func dedupe(dirs []string) []string {
	seen := make(map[string]struct{}, len(dirs))
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		dir = filepath.Clean(dir)
		if _, dup := seen[dir]; dup {
			continue
		}
		seen[dir] = struct{}{}
		out = append(out, dir)
	}
	return out
}

// checkEnabledMatches fails when a whitelist entry names no discovered
// plugin: a name typo must not silently disable the whole plugin set.
// A discovered plugin whose version fails the constraint is simply not
// enabled (the "waiting for upgrade" state) so Reconcile can pick it up
// once the version satisfies the constraint.
func checkEnabledMatches(enabled enabledSet, candidates []candidate) error {
	byName := make(map[string]candidate, len(candidates))
	for _, c := range candidates {
		if _, dup := byName[c.manifest.Name]; !dup {
			byName[c.manifest.Name] = c
		}
	}
	for name := range enabled {
		if _, ok := byName[name]; ok {
			continue
		}
		return errdefs.NotFoundf(
			"plugin: plugins.enabled %q: no plugin with that name was discovered", name)
	}
	return nil
}

func (l *Loader) scan(ctx context.Context, rootDirs []string) ([]candidate, error) {
	var candidates []candidate
	for _, rootDir := range rootDirs {
		pluginDirs, err := discoverPlugins(rootDir)
		if err != nil {
			return nil, err
		}
		for _, pluginDir := range pluginDirs {
			manifest, err := l.loadManifest(ctx, pluginDir)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, candidate{
				dir:      pluginDir,
				manifest: manifest,
			})
		}
	}
	return candidates, nil
}

// discoverPlugins returns the plugin directories under root. A root
// that is itself a plugin directory (has plugin.yaml) is returned
// directly; otherwise its immediate subdirectories containing
// plugin.yaml are returned.
func discoverPlugins(root string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, "plugin.yaml")); err == nil {
		return []string{root}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, errdefs.NotFoundf("plugin: scan plugin dir %s: %v", root, err)
	}
	var pluginDirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pluginDir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(pluginDir, "plugin.yaml")); err != nil {
			continue
		}
		pluginDirs = append(pluginDirs, pluginDir)
	}
	return pluginDirs, nil
}

func (l *Loader) loadManifest(ctx context.Context, pluginDir string) (Manifest, error) {
	data, err := resource.NewLoader(
		resource.WithBaseDir(pluginDir),
	).Load(ctx, resource.Source{File: "plugin.yaml"})
	if err != nil {
		return Manifest{}, errdefs.Validationf(
			"plugin: %s: read plugin.yaml: %v", filepath.Base(pluginDir), err)
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		return Manifest{}, errdefs.Validationf(
			"plugin: %s: %v", filepath.Base(pluginDir), err)
	}
	return manifest, nil
}

// parseEnabled builds the whitelist map. nil means nothing is enabled
// (the explicit-enable default).
func parseEnabled(entries []string) (enabledSet, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	enabled := make(enabledSet, len(entries))
	for _, entry := range entries {
		spec, err := parseNamedConstraint(entry)
		if err != nil {
			return nil, errdefs.Validationf(
				"plugin: plugins.enabled entry %q: %v", entry, err)
		}
		enabled[spec.name] = spec
	}
	return enabled, nil
}

// enabledSet is the explicit whitelist from plugins.enabled.
type enabledSet map[string]namedConstraint

func (m enabledSet) matches(manifest Manifest) bool {
	spec, ok := m[manifest.Name]
	if !ok {
		return false
	}
	return spec.matches(manifest.Version)
}

func checkDuplicateNames(active []candidate) error {
	seen := make(map[string]struct{}, len(active))
	for _, c := range active {
		if _, dup := seen[c.manifest.Name]; dup {
			return errdefs.Conflictf(
				"plugin: duplicate plugin name %q", c.manifest.Name)
		}
		seen[c.manifest.Name] = struct{}{}
	}
	return nil
}

// pluginFingerprint returns the plugin's content fingerprint: the
// manifest plus, for layer plugins, the layer file contents.
func pluginFingerprint(p Plugin) string {
	if f, ok := p.(interface{ fingerprint() string }); ok {
		return f.fingerprint()
	}
	return ManifestFingerprint(p.Manifest())
}

// diffSets compares two projections by plugin name and fingerprint.
func diffSets(previous, next *Set) Changes {
	prevByName := make(map[string]Plugin, len(previous.Plugins))
	for _, p := range previous.Plugins {
		prevByName[p.Manifest().Name] = p
	}
	nextByName := make(map[string]Plugin, len(next.Plugins))
	for _, p := range next.Plugins {
		nextByName[p.Manifest().Name] = p
	}
	var changes Changes
	for name, nextPlugin := range nextByName {
		prevPlugin, ok := prevByName[name]
		switch {
		case !ok:
			changes.Added = append(changes.Added, name)
		case pluginFingerprint(prevPlugin) != pluginFingerprint(nextPlugin):
			changes.Changed = append(changes.Changed, name)
		}
	}
	for name := range prevByName {
		if _, ok := nextByName[name]; !ok {
			changes.Removed = append(changes.Removed, name)
		}
	}
	sort.Strings(changes.Added)
	sort.Strings(changes.Removed)
	sort.Strings(changes.Changed)
	return changes
}

func layerFingerprint(base string, layer deploy.Layer, data []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(base))
	_, _ = h.Write([]byte(layer.Name))
	_, _ = h.Write([]byte(layer.Source.File))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// checkDependencies verifies requires.core against the host core
// version (when configured) and plugin-to-plugin dependencies with
// cycle detection.
func checkDependencies(active []candidate, byName map[string]candidate, coreVersion string) error {
	for _, c := range active {
		if c.manifest.Requires.Core == "" || coreVersion == "" {
			continue
		}
		constraint, err := parseConstraint(c.manifest.Requires.Core)
		if err != nil {
			return errdefs.Validationf(
				"plugin %s: requires.core %q: %v",
				c.manifest.Name, c.manifest.Requires.Core, err)
		}
		if !constraint.Match(coreVersion) {
			return errdefs.Validationf(
				"plugin %s: requires.core %q not satisfied by host core %s",
				c.manifest.Name, c.manifest.Requires.Core, coreVersion)
		}
	}

	state := make(map[string]uint8, len(byName))
	var visit func(name string, stack []string) error
	visit = func(name string, stack []string) error {
		if state[name] == 2 {
			return nil
		}
		if state[name] == 1 {
			return errdefs.Validationf(
				"plugin: dependency cycle: %s", strings.Join(append(stack, name), " -> "))
		}
		c, ok := byName[name]
		if !ok {
			return errdefs.NotFoundf(
				"plugin %s: requires missing plugin %q", stack[len(stack)-1], name)
		}
		state[name] = 1
		for _, raw := range c.manifest.Requires.Plugins {
			spec, err := parseNamedConstraint(raw)
			if err != nil {
				return errdefs.Validationf("plugin %s: %v", name, err)
			}
			dep, ok := byName[spec.name]
			if !ok {
				return errdefs.NotFoundf(
					"plugin %s: requires missing plugin %q", name, spec.name)
			}
			if !spec.matches(dep.manifest.Version) {
				return errdefs.Validationf(
					"plugin %s: requires %q but %s is version %s",
					name, raw, spec.name, dep.manifest.Version)
			}
			if err := visit(spec.name, append(stack, name)); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	for _, c := range active {
		if err := visit(c.manifest.Name, nil); err != nil {
			return err
		}
	}
	return nil
}

// checkConflicts rejects (Kind, Impl) pairs already registered in the
// loader's target or provided by more than one plugin. Provides and
// service capabilities both participate.
func checkConflicts(active []candidate, registry *resource.Registry) error {
	providers := make(map[resource.Key]string)
	claim := func(owner string, spec resource.Spec) error {
		key := resource.Key{Kind: spec.Kind, Impl: spec.Impl}
		if _, ok := registry.Lookup(spec.Kind, spec.Impl); ok {
			return errdefs.Conflictf(
				"plugin %s: provides %s/%s conflicts with an already registered factory",
				owner, spec.Kind, spec.Impl)
		}
		if previous, dup := providers[key]; dup {
			return errdefs.Conflictf(
				"plugin %s: provides %s/%s conflicts with plugin %s",
				owner, spec.Kind, spec.Impl, previous)
		}
		providers[key] = owner
		return nil
	}
	for _, c := range active {
		for _, spec := range c.manifest.Provides {
			if err := claim(c.manifest.Name, spec); err != nil {
				return err
			}
		}
		for _, artifact := range c.manifest.Artifacts {
			for _, spec := range artifact.Capabilities {
				if err := claim(c.manifest.Name, spec); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (l *Loader) buildPlugin(ctx context.Context, c candidate) ([]Plugin, error) {
	var plugins []Plugin
	var layers []deploy.Layer
	var layerData [][]byte
	sawService := false
	for _, artifact := range c.manifest.Artifacts {
		switch ArtifactType(artifact.Type) {
		case ArtifactLayer:
			data, err := loadLayerFile(ctx, c.dir, artifact)
			if err != nil {
				return nil, err
			}
			layer := deploy.Layer{
				Name:     c.manifest.Name,
				Priority: artifact.Priority,
				Source:   resource.Source{File: artifact.Path},
				BaseDir:  c.dir,
			}
			layers = append(layers, layer)
			layerData = append(layerData, data)
		case ArtifactService:
			sawService = true
			if l.serviceBuilder == nil {
				return nil, errdefs.Validationf(
					"plugin %s: service artifact requires a service plugin builder",
					c.manifest.Name)
			}
			spec, err := artifact.ServiceSpec()
			if err != nil {
				return nil, errdefs.Validationf("plugin %s: %v", c.manifest.Name, err)
			}
			spec.HostCoreVersion = l.coreVersion
			built, err := l.serviceBuilder(c.manifest, spec)
			if err != nil {
				return nil, err
			}
			plugins = append(plugins, built...)
		}
	}
	if len(layers) > 0 || !sawService {
		fingerprint := ManifestFingerprint(c.manifest)
		for i, layer := range layers {
			fingerprint = layerFingerprint(fingerprint, layer, layerData[i])
		}
		plugins = append(plugins, &layerPlugin{
			manifest: c.manifest,
			layers:   layers,
			fp:       fingerprint,
		})
	}
	return plugins, nil
}

// loadLayerFile checks that the artifact path stays inside the plugin
// directory, is readable within the resource loader's size cap, and
// strictly decodes as a deployment document fragment. It returns the
// layer bytes so the caller can fingerprint them without a second
// read.
func loadLayerFile(
	ctx context.Context, pluginDir string, artifact Artifact,
) ([]byte, error) {
	data, err := resource.NewLoader(
		resource.WithBaseDir(pluginDir),
	).Load(ctx, resource.Source{File: artifact.Path})
	if err != nil {
		return nil, errdefs.Validationf(
			"plugin: %s: layer %s: %v", filepath.Base(pluginDir), artifact.Path, err)
	}
	if _, err := utils.Decode[deploy.Document](data); err != nil {
		return nil, errdefs.Validationf(
			"plugin: %s: layer %s: %v", filepath.Base(pluginDir), artifact.Path, err)
	}
	return data, nil
}
