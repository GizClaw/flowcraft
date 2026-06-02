package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHelpOmitsRemovedHistorySuite(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"--help"})
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("help: %v", err)
	}
	if strings.Contains(out.String(), "eval history") {
		t.Fatalf("root help still advertises removed history suite:\n%s", out.String())
	}
}
