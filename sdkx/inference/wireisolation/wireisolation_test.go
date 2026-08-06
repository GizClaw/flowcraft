// Package wireisolation is a test-only static guard for the provider request
// wire isolation invariant: request wire DTOs (the types bound into
// BindGenerate/BindEmbed/BindTranscription/BindRealtime) must not embed
// canonical SDK types (message.*, media.*, inference.* request/input shapes).
// The sdk/inference runtime enforces the same invariant at Bind time via
// reflection and remains authoritative; this check gives in-repo provider
// authors feedback at test time without running a provider test that reaches
// Bind. Response-side decode types (e.g. ttsRaw) are intentionally not
// covered here — the runtime permits canonical media types there.
package wireisolation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canonicalTypeNames mirrors the canonical-type set enforced by
// inference.provider.go (invalidProviderWire and the realtime variants).
var canonicalTypeNames = map[string]bool{
	// inference requests, inputs, intents, and shapes
	"GenerateRequest": true, "GenerateInput": true, "InputContent": true,
	"Intent": true, "TextIntent": true, "ImageIntent": true,
	"AudioIntent": true, "VideoIntent": true, "ResponseFormat": true,
	"ToolChoice": true, "EmbedRequest": true, "EmbedItem": true,
	"TranscriptionRequest": true, "TranscriptionSessionConfig": true,
	"RealtimeConfig": true, "RealtimeInput": true, "RealtimeTextInput": true,
	"RealtimeAudioInput": true, "RealtimeVideoInput": true,
	"RealtimeToolResultInput": true,
	// realtime events
	"RealtimeEvent": true, "RealtimeTextDeltaEvent": true,
	"RealtimeAudioDeltaEvent": true, "RealtimeTranscriptDeltaEvent": true,
	"RealtimeToolCallEvent": true, "RealtimeResponseDoneEvent": true,
	// message DTOs
	"Message": true, "Content": true, "Part": true,
	"TextPart": true, "ImagePart": true, "AudioPart": true,
	"VideoPart": true, "FilePart": true, "DataPart": true,
	"ToolCallPart": true, "ToolResultPart": true,
	"Definition": true, "Call": true, "Result": true,
	// media value types
	"ImageSize": true, "AudioFormat": true, "VoiceSpec": true,
	"AudioChunk": true, "VideoFrame": true,
}

func TestRequestWireTypesDoNotEmbedCanonicalTypes(t *testing.T) {
	providerDirs, err := filepath.Glob(filepath.Join("..", "*"))
	if err != nil {
		t.Fatalf("glob provider dirs: %v", err)
	}
	tested := 0
	for _, dir := range providerDirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		tested++
		scanProviderDir(t, dir)
	}
	if tested == 0 {
		t.Fatal("no provider directories scanned")
	}
}

func scanProviderDir(t *testing.T, dir string) {
	t.Helper()
	fileSet := token.NewFileSet()
	structs := map[string]*ast.StructType{}

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if structType, ok := typeSpec.Type.(*ast.StructType); ok {
					structs[typeSpec.Name.Name] = structType
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	// Resolve provider-local struct references recursively, so a wire type
	// cannot hide a canonical type behind a local wrapper.
	var containsCanonical func(typeName string, visiting map[string]bool) bool
	containsCanonical = func(typeName string, visiting map[string]bool) bool {
		structType, ok := structs[typeName]
		if !ok || visiting[typeName] {
			return false
		}
		visiting[typeName] = true
		defer delete(visiting, typeName)
		for _, field := range structType.Fields.List {
			for _, name := range referencedTypeNames(field.Type) {
				if canonicalTypeNames[name] || containsCanonical(name, visiting) {
					return true
				}
			}
		}
		return false
	}

	for typeName, structType := range structs {
		// Request wires follow the repo convention of a Wire suffix; the
		// runtime check remains the authority for types this heuristic misses.
		if !strings.HasSuffix(typeName, "Wire") {
			continue
		}
		if !containsCanonical(typeName, map[string]bool{}) {
			continue
		}
		for _, field := range structType.Fields.List {
			for _, name := range referencedTypeNames(field.Type) {
				if canonicalTypeNames[name] {
					t.Errorf("%s: request wire type %s embeds canonical type %s", dir, typeName, name)
				}
			}
		}
	}
}

// referencedTypeNames returns the last segment of every type reference in an
// expression, unwrapping pointer, slice, array, and map wrappers.
func referencedTypeNames(expr ast.Expr) []string {
	switch n := expr.(type) {
	case *ast.Ident:
		return []string{n.Name}
	case *ast.SelectorExpr:
		return []string{n.Sel.Name}
	case *ast.StarExpr:
		return referencedTypeNames(n.X)
	case *ast.ArrayType:
		return referencedTypeNames(n.Elt)
	case *ast.MapType:
		var names []string
		names = append(names, referencedTypeNames(n.Key)...)
		names = append(names, referencedTypeNames(n.Value)...)
		return names
	case *ast.IndexExpr:
		return referencedTypeNames(n.X)
	default:
		return nil
	}
}
