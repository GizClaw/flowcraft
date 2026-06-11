package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHelpOnlyAdvertisesSimpleQA(t *testing.T) {
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
	help := out.String()
	if !strings.Contains(help, "eval simpleqa") {
		t.Fatalf("root help does not advertise simpleqa:\n%s", help)
	}
	for _, removedSection := range []string{"Memory / dialog", "Tool use"} {
		if strings.Contains(help, removedSection) {
			t.Fatalf("root help still advertises removed section %q:\n%s", removedSection, help)
		}
	}
}
