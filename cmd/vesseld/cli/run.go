package cli

import (
	"context"
	"flag"

	"github.com/GizClaw/flowcraft/cmd/vesseld/runtime"
)

// cmdRun dispatches the `vesseld run` sub-command. Flag parsing
// uses the standard flag package — no third-party CLI library
// needed for four sub-commands with a stable surface.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configs := newRepeatedFlag(fs, "config", "path to a config file or directory (repeatable)")
	recursive := fs.Bool("R", false, "descend into subdirectories of --config dirs")
	// mTLS overrides. Each accepts the same URL-keyed reference
	// the YAML field would (env://NAME, file:///abs/path,
	// vault://...). Empty (the default) leaves the YAML value
	// untouched; non-empty replaces it. Operators set these for
	// "drop a different cert in dev without editing the manifest"
	// workflows; production deployments keep everything in YAML.
	cert := fs.String("cert", "", "mTLS server cert ref (overrides spec.control.auth.mtls.cert)")
	key := fs.String("key", "", "mTLS server key ref (overrides spec.control.auth.mtls.key)")
	clientCA := fs.String("client-ca", "", "mTLS client CA bundle ref (overrides spec.control.auth.mtls.clientCA)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runtime.Run(context.Background(), runtime.RunOptions{
		Config:       *configs,
		Recursive:    *recursive,
		Version:      Version,
		MTLSCert:     *cert,
		MTLSKey:      *key,
		MTLSClientCA: *clientCA,
	})
}
