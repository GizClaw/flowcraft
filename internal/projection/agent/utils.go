// Package agent contains projectors for agent run and trace events.
package agent

import "time"

// parseTs parses an RFC3339Nano timestamp string into time.Time.
func parseTs(ts string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, ts)
	return t
}
