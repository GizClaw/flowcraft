package cli

import (
	"flag"
	"fmt"

	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/loader"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
)

// cmdValidate runs loader → resolver in IO-free mode and prints
// every problem it finds. Exit code is non-zero (returned via the
// error) when any error is found, so CI pipelines can gate on it.
func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	configs := newRepeatedFlag(fs, "config", "path to a config file or directory (repeatable)")
	recursive := fs.Bool("R", false, "descend into subdirectories of --config dirs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	objs, err := loader.Load(*configs, loader.Options{Recursive: *recursive})
	if err != nil {
		return err
	}
	_, errs := resolver.Resolve(objs, catalog.Builtin(), resolver.ResolveOptions{
		// validate stays IO-free: no env reads, no file reads
		// for valueFrom.file, no live LLM client construction.
	})
	if errs.Len() > 0 {
		return fmt.Errorf("%w", errs)
	}
	fmt.Println("ok")
	return nil
}
