package backends

import (
	"context"
	goruntime "runtime"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-remote"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
)

// Remote builds the runner whose boundary is a server: the command runs there,
// under that host's policy, and this side only carries the session.
func Remote() runtime.SandboxFactory {
	return func(ctx context.Context, in runtime.SandboxBuild) (sandbox.Runner, error) {
		params, err := in.Config.Remote.BackendParams()
		if err != nil {
			return nil, err
		}
		opts := remoteOptions(params)
		if params.Unix {
			if params.Token == "" && goruntime.GOOS == "windows" {
				// os.Chmod is advisory on Windows and the directory ACL is the
				// real boundary, so a socket there is not a credential by itself.
				telemetry.Warn(ctx,
					"sandbox.remote speaks over a unix socket without a token; on Windows "+
						"the socket's permissions are advisory, so keep a token or protect the directory",
					attribute.String(telemetry.AttrSandboxEndpoint, params.Socket))
			}
			runner, err := remote.NewUnix(ctx, params.Socket, opts...)
			if err != nil {
				return nil, errdefs.Validationf("sandbox.remote.socket: %w", err)
			}
			return runner, nil
		}
		return remote.New(ctx, params.Endpoint, opts...)
	}
}

// remoteOptions renders the parsed block as client options. The parsing itself
// lives in runtime.RemoteConfig.BackendParams, beside the validation that already
// checks the same values.
func remoteOptions(params runtime.RemoteBackendParams) []remote.Option {
	var opts []remote.Option
	if params.Token != "" {
		opts = append(opts, remote.WithToken(params.Token))
	}
	var httpOpts []remote.HTTPOption
	if params.InsecureSkipVerify {
		httpOpts = append(httpOpts, remote.WithHTTPInsecureTLS())
	}
	if params.Timeout > 0 {
		httpOpts = append(httpOpts, remote.WithHTTPTimeout(params.Timeout))
	}
	if len(httpOpts) > 0 {
		opts = append(opts, remote.WithHTTPOptions(httpOpts...))
	}
	if params.PollHold > 0 {
		opts = append(opts, remote.WithPollHold(params.PollHold))
	}
	return opts
}
