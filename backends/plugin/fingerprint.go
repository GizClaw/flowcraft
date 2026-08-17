package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ManifestFingerprint returns a stable content hash of the manifest.
func ManifestFingerprint(m Manifest) string {
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
