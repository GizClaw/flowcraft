package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// getenvFirst returns the first non-empty env among keys.
func getenvFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func loadDotEnv() {
	candidates := []string{
		filepath.Join("..", "..", ".env"),
		".env",
		filepath.Join("examples", "voice-pipeline", ".env"),
	}
	merged := map[string]string{}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				merged[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	for k, v := range merged {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// Voice credentials: prefer FLOWCRAFT_VOICE_*, then legacy names.

func getenvBytedanceAppID() string {
	return getenvFirst("FLOWCRAFT_VOICE_BYTEDANCE_APP_ID", "BYTEDANCE_APP_ID", "ANIMUS_BYTEDANCE_APP_ID")
}

func getenvBytedanceAccessToken() string {
	return getenvFirst("FLOWCRAFT_VOICE_BYTEDANCE_ACCESS_TOKEN", "BYTEDANCE_ACCESS_TOKEN", "ANIMUS_BYTEDANCE_ACCESS_TOKEN")
}

func getenvMinimaxAPIKey() string {
	if v := getenvFirst("FLOWCRAFT_VOICE_MINIMAX_API_KEY", "MINIMAX_API_KEY", "ANIMUS_MINIMAX_API_KEY"); v != "" {
		return v
	}
	return jsonStringField(os.Getenv("FLOWCRAFT_TEST_MINIMAX"), "api_key")
}

func getenvMinimaxModelRef() string {
	if v := strings.TrimSpace(os.Getenv("FLOWCRAFT_VOICE_MINIMAX_MODEL")); v != "" {
		return normalizeMinimaxModelRef(v)
	}
	if m := jsonStringField(os.Getenv("FLOWCRAFT_TEST_MINIMAX"), "model"); m != "" {
		return normalizeMinimaxModelRef(m)
	}
	return "minimax/MiniMax-M2.5-highspeed"
}

func getenvMinimaxVoiceID() string {
	return getenvFirst("FLOWCRAFT_VOICE_MINIMAX_VOICE_ID", "MINIMAX_VOICE_ID")
}

func normalizeMinimaxModelRef(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "minimax/MiniMax-M2.5-highspeed"
	}
	if strings.Contains(s, "/") {
		return s
	}
	return "minimax/" + s
}

func minimaxShortModel(ref string) string {
	return strings.TrimPrefix(strings.TrimSpace(ref), "minimax/")
}

func jsonStringField(raw, field string) string {
	if raw == "" {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	rm, ok := m[field]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(rm, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}
