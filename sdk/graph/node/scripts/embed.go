// Package scripts embeds all built-in JS node scripts via embed.FS.
package scripts

import (
	"embed"
	"fmt"
	"strings"
	"sync"
)

//go:embed *.js
var scriptsFS embed.FS

var (
	builtinOnce  sync.Once
	builtinTypes []string
)

func loadBuiltinTypes() {
	builtinOnce.Do(func() {
		entries, err := scriptsFS.ReadDir(".")
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".js") {
				builtinTypes = append(builtinTypes, strings.TrimSuffix(e.Name(), ".js"))
			}
		}
	})
}

// Get returns the JS source for a built-in node type.
func Get(nodeType string) (string, error) {
	filename := nodeType + ".js"
	data, err := scriptsFS.ReadFile(filename)
	if err != nil {
		return "", fmt.Errorf("scripts: unknown node type %q", nodeType)
	}
	return string(data), nil
}

// MustGet is like Get but panics on error.
func MustGet(nodeType string) string {
	s, err := Get(nodeType)
	if err != nil {
		panic(err)
	}
	return s
}

// BuiltinTypes returns the list of all built-in JS node type names,
// derived automatically from the embedded .js files.
func BuiltinTypes() []string {
	loadBuiltinTypes()
	cp := make([]string, len(builtinTypes))
	copy(cp, builtinTypes)
	return cp
}

// IsBuiltin reports whether the given node type has a built-in JS script.
func IsBuiltin(nodeType string) bool {
	loadBuiltinTypes()
	for _, t := range builtinTypes {
		if t == nodeType {
			return true
		}
	}
	return false
}
