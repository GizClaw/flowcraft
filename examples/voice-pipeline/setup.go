package main

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/node/llmnode"
	"github.com/GizClaw/flowcraft/sdk/graph/node/scriptnode"
	"github.com/GizClaw/flowcraft/sdk/graph/runner"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/voice"
	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/stt"
	"github.com/GizClaw/flowcraft/voice/stt/bytedance"
	"github.com/GizClaw/flowcraft/voice/tts"
	minimaxTTS "github.com/GizClaw/flowcraft/voice/tts/minimax"
)

// setupEngine builds the graph runner (engine.Engine) and the agent value
// that the voice pipeline drives on every turn.
func setupEngine(def *graph.GraphDefinition, mmAPIKey, minimaxModelRef string) (engine.Engine, agent.Agent, error) {
	store := &StaticProviderStore{Provider: "minimax", APIKey: mmAPIKey, Model: minimaxShortModel(minimaxModelRef)}
	resolver := llm.DefaultResolver(store, llm.WithFallbackModel(minimaxModelRef))
	nodeFactory := node.NewFactory()
	llmnode.Register(nodeFactory, resolver, nil)
	scriptnode.Register(nodeFactory, scriptnode.Deps{ScriptRuntime: jsrt.New()})

	eng, err := runner.New(def, nodeFactory)
	if err != nil {
		return nil, agent.Agent{}, fmt.Errorf("build graph runner: %w", err)
	}
	return eng, agent.Agent{ID: "voice-agent"}, nil
}

// setupCloudVoice creates STT/TTS providers and fetches the TTS voice list for the TUI.
func setupCloudVoice(ctx context.Context, bdAppID, bdToken, mmAPIKey string) (stt.STT, tts.TTS, []voiceInfo, error) {
	sttProvider, err := bytedance.New(
		bytedance.WithAppID(bdAppID),
		bytedance.WithToken(bdToken),
	)
	if err != nil {
		return nil, nil, nil, err
	}
	ttsProvider, err := minimaxTTS.New(minimaxTTS.WithAPIKey(mmAPIKey))
	if err != nil {
		return nil, nil, nil, err
	}
	voiceList, _ := ttsProvider.Voices(ctx)
	voices := make([]voiceInfo, 0, len(voiceList))
	for _, v := range voiceList {
		voices = append(voices, voiceInfo{ID: v.ID, Name: v.Name, Lang: v.Lang})
	}
	return sttProvider, ttsProvider, voices, nil
}

// setupVoicePipeline wires STT -> Runtime.Run -> TTS into a voice.Pipeline.
func setupVoicePipeline(
	sttProvider stt.STT,
	ttsProvider tts.TTS,
	eng engine.Engine,
	ag agent.Agent,
	voiceID *string,
) *voice.Pipeline {
	return voice.NewPipeline(
		sttProvider,
		ttsProvider,
		eng,
		ag,
		voice.WithSTTOptions(stt.WithLanguage("zh"), stt.WithTargetSampleRate(16000)),
		voice.WithTTSOptions(tts.WithCodec(audio.CodecMP3)),
		voice.WithDynamicTTSOptions(func() []tts.TTSOption {
			return []tts.TTSOption{tts.WithVoice(*voiceID)}
		}),
		voice.WithSegmenterOptions(tts.EagerMode(), tts.WithMinChars(4), tts.WithForceBreakRunes(12)),
		voice.WithTimeouts(voice.PipelineTimeouts{
			STTFirstPartial:  10 * time.Second,
			STTFinal:         30 * time.Second,
			RunnerFirstToken: 15 * time.Second,
			TTSFirstAudio:    10 * time.Second,
		}),
	)
}
