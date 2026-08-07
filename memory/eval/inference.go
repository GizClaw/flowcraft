package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
	inferenceconfig "github.com/GizClaw/flowcraft/sdk/inference/config"
	envresolver "github.com/GizClaw/flowcraft/sdk/inference/config/env"
	"github.com/GizClaw/flowcraft/sdkx/inference/azure"
	"github.com/GizClaw/flowcraft/sdkx/inference/bytedance"
	"github.com/GizClaw/flowcraft/sdkx/inference/deepseek"
	"github.com/GizClaw/flowcraft/sdkx/inference/minimax"
	"github.com/GizClaw/flowcraft/sdkx/inference/openai"
	"github.com/GizClaw/flowcraft/sdkx/inference/qwen"
)

// buildInferenceRuntime assembles the inference runtime from an
// inference.yaml document plus the standard provider factories.
func buildInferenceRuntime(ctx context.Context, path string) (*inference.Runtime, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read inference config: %w", err)
	}
	document, err := inferenceconfig.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse inference config: %w", err)
	}
	builder, err := inferenceconfig.NewBuilder(
		providerFactories(),
		map[string]inferenceconfig.SecretResolver{"env": envresolver.New()},
	)
	if err != nil {
		return nil, fmt.Errorf("inference builder: %w", err)
	}
	runtime, err := builder.NewRuntime(ctx, document)
	if err != nil {
		return nil, fmt.Errorf("build inference runtime: %w", err)
	}
	return runtime, nil
}

func providerFactories() map[string]inferenceconfig.Factory {
	return map[string]inferenceconfig.Factory{
		"openai":    openai.Factory(),
		"azure":     azure.Factory(),
		"deepseek":  deepseek.Factory(),
		"qwen":      qwen.Factory(),
		"bytedance": bytedance.Factory(),
		"minimax":   minimax.Factory(),
	}
}

// parseModelRef parses provider:model[:profile].
func parseModelRef(spec string) (inference.ModelRef, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return inference.ModelRef{}, fmt.Errorf("model spec %q must be provider:model[:profile]", spec)
	}
	ref := inference.ModelRef{
		ID: inference.ModelID{Provider: parts[0], Name: parts[1]},
	}
	if len(parts) == 3 {
		ref.Profile = parts[2]
	}
	if err := ref.Validate(); err != nil {
		return inference.ModelRef{}, err
	}
	return ref, nil
}
