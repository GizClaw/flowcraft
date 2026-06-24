package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHelpAdvertisesSuites(t *testing.T) {
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
	if !strings.Contains(help, "eval locomo") {
		t.Fatalf("root help does not advertise locomo:\n%s", help)
	}
	for _, removedSection := range []string{"Memory / dialog", "Tool use"} {
		if strings.Contains(help, removedSection) {
			t.Fatalf("root help still advertises removed section %q:\n%s", removedSection, help)
		}
	}
}

func TestLocomoRequiresDatasetBeforeCredentials(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"locomo"})
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--dataset is required") {
		t.Fatalf("locomo error = %v, want --dataset validation", err)
	}
}

func TestLocomoHelpAdvertisesLoCoMoTuningFlags(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"locomo", "--help"})
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("locomo help: %v", err)
	}
	help := out.String()
	for _, flag := range []string{"--per-call-timeout", "--qa-top-k", "--qa-graph-expanded-max-source", "--run-id"} {
		if !strings.Contains(help, flag) {
			t.Fatalf("locomo help does not advertise %s:\n%s", flag, help)
		}
	}
}
