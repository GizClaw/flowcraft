package commands

import (
	"path/filepath"
	"strings"
	"time"
)

// testRunDir builds the timestamped output directory for a test or
// simulation run.
func testRunDir(name string, now time.Time) string {
	return filepath.Join(".out", strings.ReplaceAll(name, "/", "_")+"_-_"+now.Format("20060102_150405"))
}
