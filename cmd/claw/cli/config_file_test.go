package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/claw"
)

func TestWriteConfigCompilesYAMLTemplatesToJSON(t *testing.T) {
	dir := t.TempDir()
	if err := WriteConfig(templateFS, "examples/raids/chat.yaml", dir); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	path := filepath.Join(dir, configFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("missing %s: %v", path, err)
	}

	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	setExampleEnv(t)
	app, err := claw.New(ws)
	if err != nil {
		t.Fatalf("New compiled config: %v", err)
	}
	defer app.Close()
	cfg := app.Config()
	if cfg.Models.Chat != "generate_model" {
		t.Fatalf("chat model alias = %q, want generate_model", cfg.Models.Chat)
	}
	if cfg.Models.Extractor != "extract_model" {
		t.Fatalf("extract model alias = %q, want extract_model", cfg.Models.Extractor)
	}
	if cfg.Settings.GenerateModel != "doubao-seed-2-0-lite-260215" {
		t.Fatalf("generate setting = %q, want doubao-seed-2-0-lite-260215", cfg.Settings.GenerateModel)
	}
	if cfg.Settings.ExtractModel != "MiniMax-M2.7-highspeed" {
		t.Fatalf("extract setting = %q, want MiniMax-M2.7-highspeed", cfg.Settings.ExtractModel)
	}
	if cfg.Models.LLM["doubao-seed-2-0-lite-260215"].Provider != "bytedance" {
		t.Fatalf("chat provider = %q, want bytedance", cfg.Models.LLM["doubao-seed-2-0-lite-260215"].Provider)
	}
	if cfg.Models.LLM["doubao-seed-2-0-lite-260215"].Model != "doubao-seed-2-0-lite-260215" {
		t.Fatalf("chat model = %q, want doubao-seed-2-0-lite-260215", cfg.Models.LLM["doubao-seed-2-0-lite-260215"].Model)
	}
	if cfg.Models.LLM["MiniMax-M2.7-highspeed"].Provider != "minimax-extract" {
		t.Fatalf("extract provider = %q, want minimax-extract", cfg.Models.LLM["MiniMax-M2.7-highspeed"].Provider)
	}
	if cfg.Models.LLM["MiniMax-M2.7-highspeed"].Model != "MiniMax-M2.7-highspeed" {
		t.Fatalf("extract model = %q, want MiniMax-M2.7-highspeed", cfg.Models.LLM["MiniMax-M2.7-highspeed"].Model)
	}
	if cfg.Models.LLM["MiniMax-M2.7-highspeed"].Spec.Defaults.Thinking != nil {
		t.Fatalf("extract template should not configure provider-specific thinking")
	}
	if cfg.Models.LLM["MiniMax-M2.7-highspeed"].BaseURL != "https://api.minimaxi.com/v1" {
		t.Fatalf("extract base_url = %q, want minimax env value", cfg.Models.LLM["MiniMax-M2.7-highspeed"].BaseURL)
	}
}

func TestWriteEmbeddedRaids(t *testing.T) {
	raids, err := listRaids()
	if err != nil {
		t.Fatalf("listRaids: %v", err)
	}
	if len(raids) == 0 {
		t.Fatal("no embedded raids")
	}
	for _, raid := range raids {
		t.Run(raid, func(t *testing.T) {
			dir := t.TempDir()
			if err := WriteConfig(templateFS, raid, dir); err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			setExampleEnv(t)
			path := filepath.Join(dir, configFileName)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("missing %s: %v", path, err)
			}
			ws, err := workspace.NewLocalWorkspace(dir)
			if err != nil {
				t.Fatalf("NewLocalWorkspace: %v", err)
			}
			app, err := claw.New(ws)
			if err != nil {
				t.Fatalf("New example config: %v", err)
			}
			defer app.Close()
		})
	}
}

func TestEmbeddedPersonas(t *testing.T) {
	personas, err := listPersonas()
	if err != nil {
		t.Fatalf("listPersonas: %v", err)
	}
	for _, persona := range []string{"girl_7_Momo", "boy_14_Tom", "man_24_Alex"} {
		if !contains(personas, persona) {
			t.Fatalf("personas = %v, want %s", personas, persona)
		}
	}
	for _, persona := range personas {
		t.Run(persona, func(t *testing.T) {
			dir := t.TempDir()
			if err := WriteConfig(templateFS, persona, dir); err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			setExampleEnv(t)
			ws, err := workspace.NewLocalWorkspace(dir)
			if err != nil {
				t.Fatalf("NewLocalWorkspace: %v", err)
			}
			app, err := claw.New(ws)
			if err != nil {
				t.Fatalf("New persona config: %v", err)
			}
			defer app.Close()
		})
	}
}

func setExampleEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GENERATE_MODEL", "doubao-seed-2-0-lite-260215")
	t.Setenv("EXTRACT_MODEL", "MiniMax-M2.7-highspeed")
	t.Setenv("EMBEDDING_MODEL", "qwen-v4")
	t.Setenv("BYTEDANCE_API_KEY", "test-chat-key")
	t.Setenv("MINIMAX_API_KEY", "test-extract-key")
	t.Setenv("MINIMAX_BASE_URL", "https://api.minimaxi.com/v1")
	t.Setenv("QWEN_API_KEY", "test-qwen-key")
	t.Setenv("AZURE_OPENAI_API_KEY", "test-azure-key")
	t.Setenv("AZURE_OPENAI_BASE_URL", "https://example.openai.azure.com")
	t.Setenv("DEEPSEEK_API_KEY", "test-deepseek-key")
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	t.Setenv("OHMYGPT_API_KEY", "test-ohmygpt-key")
}
