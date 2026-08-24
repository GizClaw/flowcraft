package windows

import (
	"flag"
	"fmt"
)

// HelperArgvMarker is the reserved argv[1] that selects sandbox-helper
// mode when the host application is re-executed elevated. It is
// deliberately namespaced so it cannot collide with a normal CLI
// flag. Host applications embed this package and call [MaybeHelper]
// as the very first statement of main, mirroring
// core/sandbox/bwrap's bridge hook.
const HelperArgvMarker = "--flowcraft-windows-sandbox-helper"

// helperMode installs the sandbox accounts / WFP filters; helperModeServe
// serves elevated spawn requests over a named pipe.
const (
	helperModeInstall = "install"
	helperModeServe   = "serve"
)

// helperArgs is the parsed helper invocation.
type helperArgs struct {
	mode   string
	pipe   string
	config string
	root   string
}

// parseHelperArgs parses the arguments that follow HelperArgvMarker:
//
//	install --config <dir>
//	serve --pipe <name> --config <dir> --root <dir>
//
// It is pure so it can be tested on every platform.
func parseHelperArgs(args []string) (helperArgs, error) {
	if len(args) == 0 {
		return helperArgs{}, fmt.Errorf("sandbox-helper: missing mode (want %q or %q)", helperModeInstall, helperModeServe)
	}
	mode := args[0]
	fs := flag.NewFlagSet("sandbox-helper "+mode, flag.ContinueOnError)
	pipe := fs.String("pipe", "", "named pipe to serve")
	config := fs.String("config", "", "sandbox config dir")
	root := fs.String("root", "", "workspace root the server will accept")
	if err := fs.Parse(args[1:]); err != nil {
		return helperArgs{}, err
	}
	if fs.NArg() != 0 {
		return helperArgs{}, fmt.Errorf("sandbox-helper: unexpected arguments %v", fs.Args())
	}
	switch mode {
	case helperModeInstall:
		if *config == "" {
			return helperArgs{}, fmt.Errorf("sandbox-helper: --config is required")
		}
	case helperModeServe:
		if *pipe == "" || *config == "" || *root == "" {
			return helperArgs{}, fmt.Errorf("sandbox-helper: --pipe, --config and --root are required")
		}
	default:
		return helperArgs{}, fmt.Errorf("sandbox-helper: unknown mode %q (want %q or %q)", mode, helperModeInstall, helperModeServe)
	}
	return helperArgs{mode: mode, pipe: *pipe, config: *config, root: *root}, nil
}
