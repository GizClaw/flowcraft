package cli

import (
	"flag"
	"fmt"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/sdkx/claw"
)

func serveCmd(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	workspaceDir := flags.String("workspace", "workspace", "workspace directory")
	workspaceDirTypo := flags.String("worksapce", "", "workspace directory")
	addr := flags.String("addr", "127.0.0.1:8787", "listen address")
	flags.Parse(args)
	if strings.TrimSpace(*workspaceDirTypo) != "" {
		workspaceDir = workspaceDirTypo
	}
	if strings.TrimSpace(*workspaceDir) == "" {
		return fmt.Errorf("serve requires --workspace\n\n%s", usage())
	}
	if strings.TrimSpace(*addr) == "" {
		return fmt.Errorf("serve requires --addr")
	}

	handler, closeFn, err := serveHandler(*workspaceDir)
	if err != nil {
		return err
	}
	defer func() { _ = closeFn() }()

	fmt.Printf("serving claw workspace %s at http://%s\n", *workspaceDir, *addr)
	return http.ListenAndServe(*addr, handler)
}

func serveHandler(workspaceDir string) (http.Handler, func() error, error) {
	app, err := openApp(workspaceDir)
	if err != nil {
		return nil, nil, err
	}
	return claw.NewDebugHTTPHandler(app), app.Close, nil
}
