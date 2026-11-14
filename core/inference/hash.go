package inference

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"

	"github.com/GizClaw/flowcraft/core/internal/ptr"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
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
	hashes := make([]string, len(request.Context))
	for i, msg := range request.Context {
		hashes[i] = MessageHash(msg)
	}
	return RequestHashWith(request, hashes)
}

// RequestHashWith is RequestHash for a caller that already digests the context:
// the model node computes the per-message identities for the trace and must not
// serialize the same transcript twice.
func RequestHashWith(request GenerateRequest, messageHashes []string) string {
	h := sha256.New()
	writeValue(h, "model", request.Model)
	writeValue(h, "output", request.Output)
	writeValue(h, "hint", request.ModelHint)
	writeValue(h, "input", request.Input)
	for i, msg := range request.Context {
		hash := ""
		if i < len(messageHashes) {
			hash = messageHashes[i]
		}
		if hash == "" {
			hash = MessageHash(msg)
		}
		writeText(h, "message", hash)
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
