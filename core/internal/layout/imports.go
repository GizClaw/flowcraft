package layout

import (
	"go/parser"
	"go/token"
	"strconv"
)

// importsOf returns the import paths of one file. It parses imports only, so the guard
// costs a token pass per file instead of a full parse.
func importsOf(path string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		unquoted, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		paths = append(paths, unquoted)
	}
	return paths, nil
}
