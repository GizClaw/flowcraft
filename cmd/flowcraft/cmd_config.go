package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/internal/paths"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/spf13/cobra"
	otellog "go.opentelemetry.io/otel/log"
	"gopkg.in/yaml.v3"
)

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configGetCmd, configSetCmd, configListCmd)
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage FlowCraft configuration",
	Long:  "View and modify settings in ~/.flowcraft/config.yaml.",
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a configuration value",
	Long:  "Read a dot-separated key from config.yaml, e.g. 'server.port'.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		data, err := readConfigMap()
		if err != nil {
			return err
		}
		val, ok := getNestedValue(data, args[0])
		if !ok {
			return fmt.Errorf("key %q not found in config", args[0])
		}
		telemetry.Info(context.Background(), "config: value",
			otellog.String("key", args[0]), otellog.String("value", formatValue(val)))
		return nil
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value",
	Long:  "Write a dot-separated key to config.yaml, e.g. 'server.port 9090'.",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		data, err := readConfigMap()
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if data == nil {
			data = make(map[string]any)
		}
		setNestedValue(data, args[0], parseValue(args[1]))
		return writeConfigMap(data)
	},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configuration values",
	RunE: func(cmd *cobra.Command, args []string) error {
		data, err := readConfigMap()
		if err != nil {
			if os.IsNotExist(err) {
				telemetry.Info(context.Background(), "config: no config.yaml found, using defaults")
				return nil
			}
			return err
		}
		printFlat("", data)
		return nil
	},
}

func readConfigMap() (map[string]any, error) {
	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse config.yaml: %w", err)
	}
	return m, nil
}

func writeConfigMap(m map[string]any) error {
	if err := paths.EnsureLayout(); err != nil {
		return err
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(paths.ConfigFile(), out, 0o644)
}

func getNestedValue(m map[string]any, key string) (any, bool) {
	parts := strings.Split(key, ".")
	var current any = m
	for _, p := range parts {
		cm, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = cm[p]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func setNestedValue(m map[string]any, key string, value any) {
	parts := strings.Split(key, ".")
	current := m
	for i, p := range parts {
		if i == len(parts)-1 {
			current[p] = value
			return
		}
		sub, ok := current[p]
		if !ok {
			sub = make(map[string]any)
			current[p] = sub
		}
		subMap, ok := sub.(map[string]any)
		if !ok {
			subMap = make(map[string]any)
			current[p] = subMap
		}
		current = subMap
	}
}

func parseValue(s string) any {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	if v, err := strconv.ParseBool(s); err == nil {
		return v
	}
	return s
}

func formatValue(v any) string {
	switch t := v.(type) {
	case map[string]any:
		out, _ := yaml.Marshal(t)
		return strings.TrimSpace(string(out))
	default:
		return fmt.Sprintf("%v", t)
	}
}

func printFlat(prefix string, m map[string]any) {
	for k, v := range m {
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		switch sub := v.(type) {
		case map[string]any:
			printFlat(full, sub)
		default:
			telemetry.Info(context.Background(), "config: entry",
				otellog.String("key", full), otellog.String("value", fmt.Sprintf("%v", v)))
		}
	}
}
