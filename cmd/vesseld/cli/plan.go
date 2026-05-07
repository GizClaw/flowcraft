package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/loader"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
)

// cmdPlan loads + resolves and prints a JSON projection of the
// resolved Plan with secrets redacted. Used for CI diffing of
// configuration changes and for incident debugging.
func cmdPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	configs := newRepeatedFlag(fs, "config", "path to a config file or directory (repeatable)")
	recursive := fs.Bool("R", false, "descend into subdirectories of --config dirs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	objs, err := loader.Load(*configs, loader.Options{Recursive: *recursive})
	if err != nil {
		return err
	}
	plan, errs := resolver.Resolve(objs, catalog.Builtin(), resolver.ResolveOptions{})
	if errs.Len() > 0 {
		return fmt.Errorf("%w", errs)
	}
	redacted := plan.MarshalRedacted()

	type vesselSummary struct {
		Name    string   `json:"name"`
		History string   `json:"history,omitempty"`
		Agents  []string `json:"agents"`
	}
	out := struct {
		Daemon  string          `json:"daemon"`
		Vessels []vesselSummary `json:"vessels"`
	}{Daemon: redacted.Daemon.Name}
	for _, vp := range redacted.Vessels {
		entry := vesselSummary{Name: vp.Name, History: vp.HistoryName}
		for _, a := range vp.Spec.Agents {
			entry.Agents = append(entry.Agents, a.Name)
		}
		out.Vessels = append(out.Vessels, entry)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
