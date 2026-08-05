package commands

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
	"github.com/GizClaw/flowcraft/examples/forge/internal/debugapi"
)

func serveCmd(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	workspaceDir := flags.String("workspace", "workspace", "workspace directory")
	addr := flags.String("addr", "127.0.0.1:8787", "listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*workspaceDir) == "" {
		return fmt.Errorf("serve requires --workspace\n\n%s", usage())
	}
	a, err := app.Open(context.Background(), *workspaceDir)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	fmt.Printf("serving forge workspace %s at http://%s\n", *workspaceDir, *addr)
	return http.ListenAndServe(*addr, debugapi.NewHandler(a))
}
