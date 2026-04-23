package voice

import (
	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/tts"
)

const (
	ExtraKeyLanguage = "speech.language"
	ExtraKeyEmotion  = "speech.emotion"
	ExtraKeyVolume   = "speech.volume"
	ExtraKeyScene    = "speech.scene"
)

type VoiceProfileScene string

const (
	VoiceProfileSceneCustomerService  VoiceProfileScene = "customer_service"
	VoiceProfileSceneCompanion        VoiceProfileScene = "companion"
	VoiceProfileSceneCommandAssistant VoiceProfileScene = "command_assistant"
)

type VoiceProfile struct {
	Language string
	Voice    string
	Speed    float64
	Emotion  string
	Volume   float64
	Codec    audio.Codec
	Rate     int
	Scene    VoiceProfileScene
}

func CustomerServiceVoiceProfile() VoiceProfile {
	return VoiceProfile{
		Speed:  1.0,
		Volume: 1.0,
		Scene:  VoiceProfileSceneCustomerService,
	}
}

func CompanionVoiceProfile() VoiceProfile {
	return VoiceProfile{
		Speed:  0.95,
		Volume: 1.0,
		Scene:  VoiceProfileSceneCompanion,
	}
}

func CommandAssistantVoiceProfile() VoiceProfile {
	return VoiceProfile{
		Speed:  1.05,
		Volume: 1.0,
		Scene:  VoiceProfileSceneCommandAssistant,
	}
}

func (p VoiceProfile) TTSOptions() []tts.TTSOption {
	opts := make([]tts.TTSOption, 0, 7)
	if p.Voice != "" {
		opts = append(opts, tts.WithVoice(p.Voice))
	}
	if p.Speed > 0 {
		opts = append(opts, tts.WithSpeed(p.Speed))
	}
	if p.Codec != 0 {
		opts = append(opts, tts.WithCodec(p.Codec))
	}
	if p.Rate > 0 {
		opts = append(opts, tts.WithRate(p.Rate))
	}
	if p.Language != "" {
		opts = append(opts, tts.WithExtra(ExtraKeyLanguage, p.Language))
	}
	if p.Emotion != "" {
		opts = append(opts, tts.WithExtra(ExtraKeyEmotion, p.Emotion))
	}
	if p.Volume > 0 {
		opts = append(opts, tts.WithExtra(ExtraKeyVolume, p.Volume))
	}
	if p.Scene != "" {
		opts = append(opts, tts.WithExtra(ExtraKeyScene, string(p.Scene)))
	}
	return opts
}
