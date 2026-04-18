package darwin

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed user-data
var userDataContent []byte

//go:embed meta-data
var metaDataContent []byte

// WriteCloudInitFiles writes the embedded user-data and meta-data files to dir.
// The file paths are passed to vfkit via --cloud-init.
func WriteCloudInitFiles(dir string) (userDataPath, metaDataPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create cloud-init dir: %w", err)
	}

	udPath := filepath.Join(dir, "user-data")
	if err := os.WriteFile(udPath, userDataContent, 0o644); err != nil {
		return "", "", fmt.Errorf("write user-data: %w", err)
	}

	mdPath := filepath.Join(dir, "meta-data")
	if err := os.WriteFile(mdPath, metaDataContent, 0o644); err != nil {
		return "", "", fmt.Errorf("write meta-data: %w", err)
	}

	return udPath, mdPath, nil
}
