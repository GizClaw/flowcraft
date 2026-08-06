package scheduler

import (
	"context"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	schedulerconfig "github.com/GizClaw/flowcraft/sdk/scheduler/config"
)

// BackendName is the scheduler server implementation name.
const BackendName = "local"

// Register adds the process-local scheduler server to a scheduler
// config builder. It takes no settings: any settings subtree is a
// configuration error.
func Register(b *schedulerconfig.Builder) error {
	return b.RegisterFactory(BackendName, func(
		_ context.Context,
		in sdkconfig.Input,
	) (sdkscheduler.Server, error) {
		var settings struct{}
		if err := in.Settings.Decode(&settings); err != nil {
			return nil, errdefs.Validationf(
				"decode local scheduler settings: %v", err)
		}
		return NewLocalServer()
	})
}
