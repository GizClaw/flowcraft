//go:build !windows

package mcp

import (
	"context"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect keeps the SDK's CommandTransport on unix: Close closes
// stdin, waits, then escalates SIGTERM -> SIGKILL per the MCP stdio
// spec.
func (t *reconnectableStdio) connect(ctx context.Context) (mcpsdk.Connection, error) {
	return (&mcpsdk.CommandTransport{Command: t.newCommand()}).Connect(ctx)
}
