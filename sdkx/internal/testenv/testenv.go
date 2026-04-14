// Package testenv provides shared environment loading for integration tests.
//
// Usage in any _integration_test.go:
//
//	func init() { testenv.Load() }
package testenv

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var once sync.Once

// Load reads deploy/.env (relative to the repository root) into the process
// environment. Existing env vars are never overwritten. The function is
// idempotent — concurrent or repeated calls are safe.
func Load() {
	once.Do(func() {
		candidates := []string{
			filepath.Join("deploy", ".env"),
			filepath.Join("..", "deploy", ".env"),
			filepath.Join("..", "..", "deploy", ".env"),
			filepath.Join("..", "..", "..", "deploy", ".env"),
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
