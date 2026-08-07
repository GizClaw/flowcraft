package config

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/route"
)

// Factory is implemented by a provider package. It owns strict decoding
// of provider/profile Spec, model rules, compiler construction, and
// validation of provider-specific secret names.
type Factory interface {
	Build(context.Context, ProviderInput) (inference.ProviderDefinition, error)
}

type FactoryFunc func(
	context.Context,
	ProviderInput,
) (inference.ProviderDefinition, error)

func (f FactoryFunc) Build(
	ctx context.Context,
	input ProviderInput,
) (inference.ProviderDefinition, error) {
	return f(ctx, input)
}

type ProviderInput struct {
	ID       string
	Profiles []ResolvedProfile
	Spec     json.RawMessage
}

type ResolvedProfile struct {
	ID         string
	Operations []inference.Operation
	Secrets    map[string]Secret
	Spec       json.RawMessage
}

func (i ProviderInput) Clone() ProviderInput {
	i.Spec = append(json.RawMessage(nil), i.Spec...)
	profiles := make([]ResolvedProfile, len(i.Profiles))
	for index, profile := range i.Profiles {
		profiles[index] = profile.Clone()
	}
	i.Profiles = profiles
	return i
}

func (p ResolvedProfile) Clone() ResolvedProfile {
	p.Operations = append([]inference.Operation(nil), p.Operations...)
	p.Spec = append(json.RawMessage(nil), p.Spec...)
	secrets := make(map[string]Secret, len(p.Secrets))
	maps.Copy(secrets, p.Secrets)
	p.Secrets = secrets
	return p
}

// Builder is immutable after construction and safe for concurrent use
// when its factories and resolvers are safe for concurrent use.
// Catalogs are instance-owned; package-global provider registration is
// intentionally absent.
type Builder struct {
	factories map[string]Factory
	resolvers map[string]SecretResolver
}

func NewBuilder(
	factories map[string]Factory,
	resolvers map[string]SecretResolver,
) (*Builder, error) {
	builder := &Builder{
		factories: make(map[string]Factory, len(factories)),
		resolvers: make(map[string]SecretResolver, len(resolvers)),
	}
	for name, factory := range factories {
		if !identifierPattern.MatchString(name) {
			return nil, fmt.Errorf("invalid factory name %q", name)
		}
		if isNilInterface(factory) {
			return nil, fmt.Errorf("factory %q is nil", name)
		}
		builder.factories[name] = factory
	}
	for name, resolver := range resolvers {
		if !identifierPattern.MatchString(name) {
			return nil, fmt.Errorf("invalid resolver name %q", name)
		}
		if isNilInterface(resolver) {
			return nil, fmt.Errorf("resolver %q is nil", name)
		}
		builder.resolvers[name] = resolver
	}
	return builder, nil
}

func (b *Builder) Build(
	ctx context.Context,
	document Document,
) ([]inference.ProviderDefinition, error) {
	definitions, err := b.buildDefinitions(ctx, document)
	if err != nil {
		return nil, err
	}
	// Runtime construction is local and performs no provider I/O.
	// Running it here catches malformed factory output before definitions
	// escape Builder.
	if _, err := inference.NewRuntime(definitions); err != nil {
		return nil, fmt.Errorf("validate provider definitions: %w", err)
	}
	return definitions, nil
}

func (b *Builder) buildDefinitions(
	ctx context.Context,
	document Document,
) ([]inference.ProviderDefinition, error) {
	if b == nil {
		return nil, fmt.Errorf("inference config builder is nil")
	}
	if err := document.Validate(); err != nil {
		return nil, err
	}
	document = document.Clone()
	definitions := make(
		[]inference.ProviderDefinition,
		0,
		len(document.Providers),
	)
	for _, provider := range document.Providers {
		factory, ok := b.factories[provider.Driver]
		if !ok {
			return nil, fmt.Errorf(
				"provider %q: factory %q is not configured",
				provider.ID,
				provider.Driver,
			)
		}
		input, err := b.resolveProvider(ctx, provider)
		if err != nil {
			return nil, err
		}
		definition, err := factory.Build(ctx, input)
		if err != nil {
			return nil, safeCause(
				fmt.Sprintf(
					"provider %q factory %q failed",
					provider.ID,
					provider.Driver,
				),
				err,
			)
		}
		if definition.ID != provider.ID {
			return nil, fmt.Errorf(
				"provider %q factory returned identity %q",
				provider.ID,
				definition.ID,
			)
		}
		definition.Profiles, err = scopeProfiles(
			provider.Profiles,
			definition.Profiles,
		)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", provider.ID, err)
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func (b *Builder) NewRuntime(
	ctx context.Context,
	document Document,
	options ...inference.RuntimeOption,
) (*inference.Runtime, error) {
	definitions, err := b.buildDefinitions(ctx, document)
	if err != nil {
		return nil, err
	}
	runtime, err := inference.NewRuntime(definitions, options...)
	if err != nil {
		return nil, err
	}
	if document.Route != nil {
		if err := document.Route.ValidateFor(runtime); err != nil {
			return nil, fmt.Errorf("validate route targets: %w", err)
		}
	}
	return runtime, nil
}

// Assembly is the fully built output of one Document: the runtime plus,
// when the document declares a route section, a Router composed above
// it.
type Assembly struct {
	Runtime *inference.Runtime
	// Router is nil when the document has no route section; callers then
	// address exact models through Runtime directly.
	Router *route.Router
}

// ResolveItem exposes the exact inference runtime as "runtime" while
// keeping the Assembly as the deploy-owned resource and optional router
// container.
func (a *Assembly) ResolveItem(ref string) (any, bool) {
	if a == nil || ref != "runtime" || a.Runtime == nil {
		return nil, false
	}
	return a.Runtime, true
}

// NewAssembly builds the runtime and, when document.Route is set,
// composes the deployment Router above it. The Router is driven by the
// route policy in declared order (see Policy.Selectors); callers that
// need score-aware or request-aware selection build route.New
// themselves with custom Selectors. Route targets were already
// validated against the runtime, so Router construction cannot fail on
// target resolution.
func (b *Builder) NewAssembly(
	ctx context.Context,
	document Document,
	options ...inference.RuntimeOption,
) (Assembly, error) {
	runtime, err := b.NewRuntime(ctx, document, options...)
	if err != nil {
		return Assembly{}, err
	}
	assembly := Assembly{Runtime: runtime}
	if document.Route != nil {
		options, err := document.Route.Options()
		if err != nil {
			return Assembly{}, fmt.Errorf("build route options: %w", err)
		}
		router, err := route.New(runtime, document.Route.Selectors(), options...)
		if err != nil {
			return Assembly{}, fmt.Errorf("build route router: %w", err)
		}
		assembly.Router = router
	}
	return assembly, nil
}

func (b *Builder) resolveProvider(
	ctx context.Context,
	provider ProviderConfig,
) (ProviderInput, error) {
	input := ProviderInput{
		ID:       provider.ID,
		Profiles: make([]ResolvedProfile, len(provider.Profiles)),
		Spec:     append(json.RawMessage(nil), provider.Spec...),
	}
	for index, profile := range provider.Profiles {
		resolved := ResolvedProfile{
			ID: profile.ID,
			Operations: append(
				[]inference.Operation(nil),
				profile.Operations...,
			),
			Secrets: make(map[string]Secret, len(profile.Secrets)),
			Spec:    append(json.RawMessage(nil), profile.Spec...),
		}
		for name, ref := range profile.Secrets {
			resolver, ok := b.resolvers[ref.Resolver]
			if !ok {
				return ProviderInput{}, fmt.Errorf(
					"provider %q profile %q secret %q: resolver %q is not configured",
					provider.ID,
					profile.ID,
					name,
					ref.Resolver,
				)
			}
			secret, err := resolver.Resolve(ctx, ref.Key)
			if err != nil {
				return ProviderInput{}, safeCause(
					fmt.Sprintf(
						"provider %q profile %q secret %q: resolver %q failed",
						provider.ID,
						profile.ID,
						name,
						ref.Resolver,
					),
					err,
				)
			}
			if len(secret.value) == 0 {
				return ProviderInput{}, fmt.Errorf(
					"provider %q profile %q secret %q: resolver %q returned an empty secret",
					provider.ID,
					profile.ID,
					name,
					ref.Resolver,
				)
			}
			resolved.Secrets[name] = secret
		}
		input.Profiles[index] = resolved
	}
	return input, nil
}

func scopeProfiles(
	configured []ProfileConfig,
	provided []inference.ProfileDefinition,
) ([]inference.ProfileDefinition, error) {
	if len(configured) == 0 {
		return cloneProfileDefinitions(provided), nil
	}
	providedByID := make(
		map[string]inference.ProfileDefinition,
		len(provided),
	)
	for _, profile := range provided {
		if _, exists := providedByID[profile.ID]; exists {
			return nil, fmt.Errorf(
				"factory returned duplicate profile %q",
				profile.ID,
			)
		}
		providedByID[profile.ID] = profile
	}
	result := make([]inference.ProfileDefinition, 0, len(configured))
	for _, profile := range configured {
		actual, exists := providedByID[profile.ID]
		if len(provided) > 0 && !exists {
			return nil, fmt.Errorf(
				"factory did not return configured profile %q",
				profile.ID,
			)
		}
		operations := intersectOperations(
			profile.Operations,
			actual.Operations,
		)
		if len(profile.Operations) > 0 &&
			len(actual.Operations) > 0 &&
			len(operations) == 0 {
			return nil, fmt.Errorf(
				"profile %q has no operation supported by its factory",
				profile.ID,
			)
		}
		result = append(result, inference.ProfileDefinition{
			ID:         profile.ID,
			Operations: operations,
		})
		delete(providedByID, profile.ID)
	}
	if len(providedByID) > 0 {
		return nil, fmt.Errorf("factory returned an unconfigured profile")
	}
	return result, nil
}

func intersectOperations(
	configured []inference.Operation,
	provided []inference.Operation,
) []inference.Operation {
	if len(configured) == 0 {
		return append([]inference.Operation(nil), provided...)
	}
	if len(provided) == 0 {
		return append([]inference.Operation(nil), configured...)
	}
	allowed := make(map[inference.Operation]struct{}, len(provided))
	for _, operation := range provided {
		allowed[operation] = struct{}{}
	}
	result := make([]inference.Operation, 0, len(configured))
	for _, operation := range configured {
		if _, ok := allowed[operation]; ok {
			result = append(result, operation)
		}
	}
	return result
}

func cloneProfileDefinitions(
	profiles []inference.ProfileDefinition,
) []inference.ProfileDefinition {
	cloned := make([]inference.ProfileDefinition, len(profiles))
	for index, profile := range profiles {
		profile.Operations = append(
			[]inference.Operation(nil),
			profile.Operations...,
		)
		cloned[index] = profile
	}
	return cloned
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type redactedCause struct {
	message string
	cause   error
}

func safeCause(message string, cause error) error {
	return redactedCause{message: message, cause: cause}
}

func (e redactedCause) Error() string { return e.message }
func (e redactedCause) Unwrap() error { return e.cause }
