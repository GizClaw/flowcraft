package mitm

import (
	"os"
	"strings"
	"testing"
)

func TestWriteBundleAppendsCA(t *testing.T) {
	path, cleanup, err := WriteBundle([]byte("-----BEGIN CERTIFICATE-----\nXXXXCAXXXX\n-----END CERTIFICATE-----\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "XXXXCAXXXX") {
		t.Fatalf("CA not appended: %d bytes", len(data))
	}
}
