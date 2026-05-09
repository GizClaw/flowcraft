// Package testenv provides shared environment loading for the
// knowledge e2e integration tests. A near-identical copy lives at
// sdkx/internal/testenv; both are intentionally tiny and independent
// because they sit in different "internal" trees and can't share code
// without breaking the package boundaries that justify either copy.
package testenv

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var once sync.Once

// Load reads a repo-root `.env` file into the process environment. The
// lookup walks up from the test's working directory so it works
// regardless of which package directory `go test` is invoked from.
// Existing env vars are never overwritten. Idempotent and concurrency
// safe.
func Load() {
	once.Do(func() {
		candidates := []string{
			".env",
			filepath.Join("..", ".env"),
			filepath.Join("..", "..", ".env"),
			filepath.Join("..", "..", "..", ".env"),
			filepath.Join("..", "..", "..", "..", ".env"),
		}
		for _, p := range candidates {
			if loadFile(p) {
				return
			}
		}
	})
}

func loadFile(path string) bool {
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
