package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	retrievalns "github.com/GizClaw/flowcraft/sdk/retrieval/namespace"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/retrieval/postgres"
	"github.com/GizClaw/flowcraft/sdkx/retrieval/sqlite"
	wsretrieval "github.com/GizClaw/flowcraft/sdkx/retrieval/workspace"
)

type config struct {
	backend            string
	sqlitePath         string
	postgresDSN        string
	workspaceRoot      string
	workspaceIndexRoot string

	mode       string
	from       string
	to         string
	prefix     string
	runtimeID  string
	userID     string
	batchSize  int
	dropSource bool
	timeout    time.Duration
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "retrieval-namespace-migrate: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.backend, "backend", "", "backend: sqlite, postgres, or workspace")
	flag.StringVar(&cfg.sqlitePath, "sqlite-path", "", "path to sqlite database")
	flag.StringVar(&cfg.postgresDSN, "postgres-dsn", "", "postgres DSN")
	flag.StringVar(&cfg.workspaceRoot, "workspace-root", "", "local workspace root for workspace backend")
	flag.StringVar(&cfg.workspaceIndexRoot, "workspace-index-root", "", "workspace retrieval index root")

	flag.StringVar(&cfg.mode, "mode", "explicit", "namespace plan mode: explicit, recall-user-v1, recall-entities-v1")
	flag.StringVar(&cfg.from, "from", "", "source namespace for explicit mode")
	flag.StringVar(&cfg.to, "to", "", "destination namespace for explicit mode")
	flag.StringVar(&cfg.prefix, "prefix", "ltm", "namespace prefix for derived modes")
	flag.StringVar(&cfg.runtimeID, "runtime", "", "runtime id for derived modes")
	flag.StringVar(&cfg.userID, "user", "", "user id for derived modes")
	flag.IntVar(&cfg.batchSize, "batch-size", 256, "copy batch size")
	flag.BoolVar(&cfg.dropSource, "drop-source", false, "drop/delete source namespace after successful copy")
	flag.DurationVar(&cfg.timeout, "timeout", 10*time.Minute, "migration timeout")
	flag.Parse()
	return cfg
}

func run(cfg config) error {
	from, to, err := planNamespaces(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	idx, err := openIndex(ctx, cfg)
	if err != nil {
		return err
	}
	defer idx.Close()
	res, err := retrievalns.CopyNamespace(ctx, idx, from, to, retrievalns.CopyOptions{
		BatchSize:  cfg.batchSize,
		DropSource: cfg.dropSource,
	})
	if err != nil {
		return err
	}
	fmt.Printf("copied=%d from=%q to=%q drop_source=%v\n", res.Copied, res.From, res.To, cfg.dropSource)
	return nil
}

func planNamespaces(cfg config) (string, string, error) {
	switch cfg.mode {
	case "explicit":
		if cfg.from == "" || cfg.to == "" {
			return "", "", fmt.Errorf("-from and -to are required in explicit mode")
		}
		return cfg.from, cfg.to, nil
	case "recall-user-v1", "recall-entities-v1":
		if cfg.runtimeID == "" || cfg.userID == "" {
			return "", "", fmt.Errorf("-runtime and -user are required in %s mode", cfg.mode)
		}
		p, err := retrievalns.Register(cfg.prefix)
		if err != nil {
			return "", "", err
		}
		from := p.LegacyUserScopeV1(cfg.runtimeID, cfg.userID)
		to := p.UserScope(cfg.runtimeID, cfg.userID)
		if cfg.mode == "recall-entities-v1" {
			from += "__entities"
			to = p.SuffixedScope(cfg.runtimeID, cfg.userID, "entities")
		}
		return from, to, nil
	default:
		return "", "", fmt.Errorf("unknown -mode %q", cfg.mode)
	}
}

func openIndex(ctx context.Context, cfg config) (retrieval.Index, error) {
	switch cfg.backend {
	case "sqlite":
		if cfg.sqlitePath == "" {
			return nil, fmt.Errorf("-sqlite-path is required for sqlite backend")
		}
		return sqlite.Open(cfg.sqlitePath)
	case "postgres":
		if cfg.postgresDSN == "" {
			return nil, fmt.Errorf("-postgres-dsn is required for postgres backend")
		}
		return postgres.Open(ctx, cfg.postgresDSN)
	case "workspace":
		if cfg.workspaceRoot == "" {
			return nil, fmt.Errorf("-workspace-root is required for workspace backend")
		}
		local, err := sdkworkspace.NewLocalWorkspace(cfg.workspaceRoot)
		if err != nil {
			return nil, err
		}
		var ws sdkworkspace.Workspace = local
		if cfg.workspaceIndexRoot != "" {
			ws = sdkworkspace.Sub(ws, cfg.workspaceIndexRoot)
		}
		return wsretrieval.New(ws)
	default:
		return nil, fmt.Errorf("-backend must be sqlite, postgres, or workspace")
	}
}
