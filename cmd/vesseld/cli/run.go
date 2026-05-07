package cli

import (
	"context"
	"flag"

	"github.com/GizClaw/flowcraft/cmd/vesseld/runtime"
)

// cmdRun dispatches the `vesseld run` sub-command. Flag parsing
// uses the standard flag package — no third-party CLI library
// needed for four sub-commands with a stable surface.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configs := newRepeatedFlag(fs, "config", "path to a config file or directory (repeatable)")
	recursive := fs.Bool("R", false, "descend into subdirectories of --config dirs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runtime.Run(context.Background(), runtime.RunOptions{
		Config:    *configs,
		Recursive: *recursive,
		Version:   Version,
	})
}
