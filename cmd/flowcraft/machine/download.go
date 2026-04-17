package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GizClaw/flowcraft/internal/paths"
)

const (
	defaultRepo   = "GizClaw/flowcraft"
	releaseURLFmt = "https://github.com/%s/releases/download/%s/%s"
	latestURLFmt  = "https://api.github.com/repos/%s/releases/latest"
)

// ImageKind selects which runtime image to download.
type ImageKind int

const (
	ImageQCOW2  ImageKind = iota // macOS VM image
	ImageRootFS                  // Windows WSL rootfs tarball
	ImageOCI                     // Docker/K8s OCI image (for reference)
)

func (k ImageKind) filename(version, arch string) string {
	switch k {
	case ImageQCOW2:
		return fmt.Sprintf("flowcraft-vm-%s-%s.qcow2", version, arch)
	case ImageRootFS:
		return fmt.Sprintf("flowcraft-wsl-%s-amd64.tar.gz", version)
	default:
		return fmt.Sprintf("flowcraft-runtime-%s-%s.tar.gz", version, arch)
	}
}

// EnsureImage downloads the runtime image for the given version if not already cached.
// Returns the path to the cached image file.
func EnsureImage(ctx context.Context, version string, kind ImageKind) (string, error) {
	if err := paths.EnsureLayout(); err != nil {
		return "", err
	}

	arch := runtime.GOARCH
	name := kind.filename(version, arch)
	cached := filepath.Join(paths.MachineDir(), name)

	if _, err := os.Stat(cached); err == nil {
		return cached, nil
	}

	repo := defaultRepo
	url := fmt.Sprintf(releaseURLFmt, repo, version, name)

	fmt.Printf("Downloading runtime image %s ...\n", name)
	if err := downloadFile(ctx, url, cached); err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	fmt.Printf("Cached at %s\n", cached)
	return cached, nil
}

// CheckVersionMismatch compares the CLI version against the cached runtime
// image version. Returns the latest release version if an update is available.
func CheckVersionMismatch(ctx context.Context, cliVersion string) (latestVersion string, needsUpdate bool, err error) {
	latest, err := fetchLatestRelease(ctx)
	if err != nil {
		return "", false, err
	}
	norm := func(v string) string { return strings.TrimPrefix(v, "v") }
	if norm(latest) != norm(cliVersion) {
		return latest, true, nil
	}
	return latest, false, nil
}

func fetchLatestRelease(ctx context.Context) (string, error) {
	url := fmt.Sprintf(latestURLFmt, defaultRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}
	return release.TagName, nil
}

func downloadFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// FileFingerprint returns the SHA256 fingerprint of a file (first 16 hex chars).
func FileFingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
