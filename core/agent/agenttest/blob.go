package agenttest

import (
	"crypto/sha256"
	"encoding/hex"
)

// blobRef is a content-addressed reference, which is what makes a repeated PutBlob of
// the same bytes free.
func blobRef(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
