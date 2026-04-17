package gateway

import (
	"fmt"

	"github.com/GizClaw/flowcraft/internal/model"
)

// BuildChannel constructs a Channel instance from a ChannelBinding.
func BuildChannel(binding model.ChannelBinding) (Channel, error) {
	switch binding.Type {
	case "slack":
		return NewSlackChannel(
			bindStr(binding.Config, "signing_secret"),
			bindStr(binding.Config, "bot_token"),
		), nil
	case "dingtalk":
		return NewDingTalkChannel(
			bindStr(binding.Config, "app_secret"),
			bindStr(binding.Config, "webhook_url"),
		), nil
	case "feishu":
		return NewFeishuChannelFromBinding(binding.Config), nil
	default:
		return nil, fmt.Errorf("unknown channel type %q", binding.Type)
	}
}

func bindStr(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
