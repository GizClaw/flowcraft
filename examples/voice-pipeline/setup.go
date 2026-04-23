package main

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/adapter"
	"github.com/GizClaw/flowcraft/sdk/graph/executor"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/GizClaw/flowcraft/voice"
	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/stt"
	"github.com/GizClaw/flowcraft/voice/stt/bytedance"
	"github.com/GizClaw/flowcraft/voice/tts"
	minimaxTTS "github.com/GizClaw/flowcraft/voice/tts/minimax"
)

// setupWorkflow creates a workflow.Runtime + Agent backed by the graph definition.
func setupWorkflow(def *graph.GraphDefinition, mmAPIKey, minimaxModelRef string) (workflow.Runtime, workflow.Agent) {
	store := &StaticProviderStore{Provider: "minimax", APIKey: mmAPIKey, Model: minimaxShortModel(minimaxModelRef)}
	resolver := llm.DefaultResolver(store, llm.WithFallbackModel(minimaxModelRef))
	nodeFactory := node.NewFactory(
		node.WithLLMResolver(resolver),
		node.WithScriptRuntime(jsrt.New()),
	)

	strategy := adapter.FromDefinition(def)
	agent := workflow.NewAgent("voice-agent", strategy)
	deps := workflow.NewDependencies()
	workflow.SetDep(deps, adapter.DepNodeFactory, nodeFactory)
	workflow.SetDep(deps, adapter.DepExecutor, executor.NewLocalExecutor())
	rt := workflow.NewRuntime(
		workflow.WithDependencies(deps),
	)
	return rt, agent
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
	rt workflow.Runtime,
	agent workflow.Agent,
	voiceID *string,
) *voice.Pipeline {
	return voice.NewPipeline(
		sttProvider,
		ttsProvider,
		rt,
		agent,
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
