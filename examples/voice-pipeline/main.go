// Command voice-pipeline is a minimal local demo: microphone → ByteDance STT →
// Flowcraft graph (MiniMax LLM) → MiniMax TTS → speakers, with optional text input
// and barge-in. No persona / OCEAN / character logic — only the voice stack.
//
// Prerequisites (macOS): brew install portaudio
//
// LLM parameters are defined in the react_agent graph's llm node config (matching
// FlowCraft's GraphDefinition). Credentials are read from environment variables
// (optionally sourced from a repo-root `.env` file); see README for the list.
//
// Usage: go run .   (from this directory)
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gordonklaus/portaudio"

	_ "github.com/GizClaw/flowcraft/sdk/graph/node/scriptnode"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
)

func main() {
	loadDotEnv()
	bdAppID, bdToken, mmAPIKey := requireVoiceCredentials()

	def, err := loadReactAgent()
	if err != nil {
		fatal("react_agent:", err)
	}
	fmt.Fprintf(os.Stderr, "react_agent: %s\n", reactAgentYAML)

	minimaxModelRef := getenvMinimaxModelRef()
	voiceID := getenvMinimaxVoiceID()
	if voiceID == "" {
		voiceID = "male-qn-qingse"
	}
	voicePtr := &voiceID

	if err := portaudio.Initialize(); err != nil {
		fatal("portaudio init:", err)
	}
	defer portaudio.Terminate()

	rt, agent := setupWorkflow(def, mmAPIKey, minimaxModelRef)

	ctx, stop := context.WithCancel(context.Background())

	sttProvider, ttsProvider, voices, err := setupCloudVoice(ctx, bdAppID, bdToken, mmAPIKey)
	if err != nil {
		fatal("create STT/TTS:", err)
	}

	pipeline := setupVoicePipeline(sttProvider, ttsProvider, rt, agent, voicePtr)

	source, err := NewPortAudioSource()
	if err != nil {
		fatal("create mic:", err)
	}
	if err := source.Start(ctx); err != nil {
		fatal("start mic:", err)
	}
	sink := NewPortAudioSink()

	sessionDone := runVoiceUI(ctx, pipeline, source, sink, voices, voicePtr)

	stop()
	<-sessionDone
	_ = source.Close()
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "error: %s %v\n", msg, err)
	os.Exit(1)
}

// requireVoiceCredentials reads and validates ByteDance STT / MiniMax credentials; exits on failure.
func requireVoiceCredentials() (bdAppID, bdToken, mmAPIKey string) {
	bdAppID = getenvBytedanceAppID()
	bdToken = getenvBytedanceAccessToken()
	mmAPIKey = getenvMinimaxAPIKey()
	if bdAppID == "" || bdToken == "" || mmAPIKey == "" {
		fmt.Fprintf(os.Stderr, "error: set FLOWCRAFT_VOICE_BYTEDANCE_APP_ID, FLOWCRAFT_VOICE_BYTEDANCE_ACCESS_TOKEN, "+
			"FLOWCRAFT_VOICE_MINIMAX_API_KEY, or legacy BYTEDANCE_* / ANIMUS_* / FLOWCRAFT_TEST_MINIMAX\n")
		os.Exit(1)
	}
	return bdAppID, bdToken, mmAPIKey
}
