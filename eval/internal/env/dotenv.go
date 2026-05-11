package env

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// dotEnvOnce guards LoadDotEnv. We intentionally allow only one .env
// source to win per process: a second call (e.g. from a different
// integration test file) is a no-op, so a test cannot accidentally
// override a value the operator set on the command line.
var dotEnvOnce sync.Once

// LoadDotEnv reads a repo-root `.env` file into the process
// environment. The lookup walks up from the working directory so it
// works regardless of which package directory `go test` is invoked
// from. Existing env vars are never overwritten (operator > .env >
// nothing). Idempotent and concurrency safe.
//
// This helper exists for integration tests that want to be ergonomic
// (`cd eval/<suite> && go test -tags=integration`) without forcing
// the operator to `set -a && source .env && set +a` every time. The
// production CLI binaries do NOT call this — they rely on the
// surrounding shell or the `run-eval.sh` supervisor to have already
// sourced the file, which is the explicit + auditable path.
//
// A near-identical copy lives at sdkx/internal/testenv. They cannot
// share code because Go's internal/ visibility rules forbid imports
// across module boundaries (sdkx/ and eval/ are separate modules);
// keeping each copy minimal and well-tested is the agreed cost.
func LoadDotEnv() {
	dotEnvOnce.Do(func() {
		candidates := []string{
			".env",
			filepath.Join("..", ".env"),
			filepath.Join("..", "..", ".env"),
			filepath.Join("..", "..", "..", ".env"),
			filepath.Join("..", "..", "..", "..", ".env"),
		}
		for _, p := range candidates {
			if loadDotEnvFile(p) {
				return
			}
		}
	})
}

// LoadFile is the single-file analogue of LoadDotEnv: it reads the
// supplied path (no auto-discovery) and sets every key into the
// process environment without overwriting existing values. Used by
// the cli `--env-file` flag so the operator can point at an
// alternative dotenv (e.g. `.env.prod`, `.env.eval`) without renaming
// the local checkout. Silently no-ops when the file is missing — the
// command is free to continue if the user expected to rely solely on
// the ambient shell.
func LoadFile(path string) bool {
	return loadDotEnvFile(path)
}

func loadDotEnvFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return true
}
