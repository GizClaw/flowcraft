//go:build linux

package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/internal/bootstrap"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/spf13/cobra"
	otellog "go.opentelemetry.io/otel/log"

	// LLM provider auto-registration via init().
	_ "github.com/GizClaw/flowcraft/sdkx/llm/anthropic"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/mock"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/ollama"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"

	// Default node builders (llm, etc.).
	_ "github.com/GizClaw/flowcraft/sdk/graph/node"
	// Built-in script node types (gate, context, …).
	_ "github.com/GizClaw/flowcraft/sdk/graph/node/scriptnode"
)

func init() {
	rootCmd.AddCommand(serverCmd)
}

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Run the FlowCraft HTTP API server in foreground",
	Run:   runServer,
}

func runServer(cmd *cobra.Command, args []string) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	_, server, cleanup, err := bootstrap.Run(ctx)
	if err != nil {
		telemetry.Error(ctx, "bootstrap failed", otellog.String("error", err.Error()))
		os.Exit(1)
	}
	defer cleanup()

	go func() {
		<-ctx.Done()
		shutCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = server.Shutdown(shutCtx)
	}()

	if err := server.ListenAndServe(ctx); err != nil && err != http.ErrServerClosed {
		telemetry.Error(ctx, "server stopped", otellog.String("error", err.Error()))
		os.Exit(1)
	}
}
