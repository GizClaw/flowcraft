package inference

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"sort"

	"github.com/GizClaw/flowcraft/core/internal/ptr"
	"github.com/GizClaw/flowcraft/core/message"
)

// RequestHash is the canonical identity of a generation request.
//
// It is the key a replay looks a recorded call up by: two requests with the same
// model, context, input, output, tools and extension values hash the same,
// regardless of map iteration order or the order tools were declared in.
//
// Field values are length-prefixed, so concatenating two requests cannot produce
// the same byte stream as one request with different boundaries.
func RequestHash(request GenerateRequest) string {
	h := sha256.New()
	writeValue(h, "model", request.Model)
	writeValue(h, "output", request.Output)
	writeValue(h, "hint", request.ModelHint)
	writeValue(h, "input", request.Input)
	for _, msg := range request.Context {
		writeValue(h, "message", msg)
	}
	definitions := append([]message.ToolDefinition(nil), request.Tools.Definitions()...)
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	for _, definition := range definitions {
		writeValue(h, "tool", definition)
	}
	if choice := request.Tools.Choice(); choice != nil {
		writeValue(h, "choice", choice)
	}
	for _, extension := range request.Extensions {
		if ptr.IsNil(extension) {
			continue
		}
		writeText(h, "extension", fmt.Sprintf("%T/%s/%s",
			extension, extension.ProviderID(), extension.ExtensionID()))
		writeValue(h, "extension-value", extension)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func writeValue(h hash.Hash, label string, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		body = []byte("!")
	}
	writeText(h, label, string(body))
}

func writeText(h hash.Hash, label, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	h.Write([]byte(label))
	h.Write([]byte{0})
	h.Write(size[:])
	h.Write([]byte(value))
}
