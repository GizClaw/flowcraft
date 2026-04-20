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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	otellog "go.opentelemetry.io/otel/log"
)

const (
	defaultRepo   = "GizClaw/flowcraft"
	releaseURLFmt = "https://github.com/%s/releases/download/%s/%s"
	latestURLFmt  = "https://api.github.com/repos/%s/releases/latest"

	debianCloudBase    = "https://cloud.debian.org/images/cloud/bookworm/latest"
	debianImageVersion = "12"
)

// ImageKind selects which runtime image to download.
type ImageKind int

const (
	ImageDisk     ImageKind = iota // Debian genericcloud raw disk (downloaded as tar.xz, extracted)
	ImageLinuxBin                  // Linux server binary
)

func debianArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// EnsureImage downloads the runtime image if not already cached.
// For ImageDisk, it downloads the official Debian nocloud tar.xz and extracts
// the raw image. For others, it downloads from GitHub releases.
func EnsureImage(ctx context.Context, version string, kind ImageKind) (string, error) {
	if err := config.EnsureLayout(); err != nil {
		return "", err
	}

	switch kind {
	case ImageDisk:
		return ensureDebianDisk(ctx)
	case ImageLinuxBin:
		asset := fmt.Sprintf("flowcraft-linux-%s", runtime.GOARCH)
		dst := filepath.Join(config.BinDir(), "flowcraft")
		if _, err := os.Stat(dst); err == nil {
			return dst, nil
		}
		src, err := ensureGitHubAsset(ctx, version, asset, config.BinDir())
		if err != nil {
			return "", err
		}
		if src != dst {
			if err := os.Rename(src, dst); err != nil {
				return "", fmt.Errorf("rename linux binary: %w", err)
			}
		}
		if err := os.Chmod(dst, 0o755); err != nil {
			return "", err
		}
		return dst, nil
	default:
		return "", fmt.Errorf("unknown image kind: %d", kind)
	}
}

func ensureDebianDisk(ctx context.Context) (string, error) {
	rawPath := filepath.Join(config.MachineDir(), "disk.raw")

	if _, err := os.Stat(rawPath); err == nil {
		return rawPath, nil
	}

	arch := debianArch()
	tarName := fmt.Sprintf("debian-%s-genericcloud-%s.tar.xz", debianImageVersion, arch)
	tarPath := filepath.Join(config.MachineDir(), tarName)
	url := fmt.Sprintf("%s/%s", debianCloudBase, tarName)

	telemetry.Info(ctx, "download: fetching Debian cloud image", otellog.String("file", tarName))
	if err := downloadFile(ctx, url, tarPath); err != nil {
		return "", fmt.Errorf("download %s: %w", tarName, err)
	}

	telemetry.Info(ctx, "download: extracting disk image")
	if err := extractTarXZ(tarPath, config.MachineDir()); err != nil {
		return "", fmt.Errorf("extract %s: %w", tarName, err)
	}

	_ = os.Remove(tarPath)

	extractedName := fmt.Sprintf("debian-%s-genericcloud-%s.raw", debianImageVersion, arch)
	extractedPath := filepath.Join(config.MachineDir(), extractedName)
	if _, err := os.Stat(extractedPath); err == nil {
		if err := os.Rename(extractedPath, rawPath); err != nil {
			return "", fmt.Errorf("rename %s → disk.raw: %w", extractedName, err)
		}
	}

	if _, err := os.Stat(rawPath); err != nil {
		return "", fmt.Errorf("expected disk.raw after extraction but not found")
	}

	telemetry.Info(ctx, "download: disk image cached", otellog.String("path", rawPath))
	return rawPath, nil
}

// extractTarXZ extracts a .tar.xz archive into destDir using the system tar.
func extractTarXZ(archive, destDir string) error {
	cmd := exec.Command("tar", "xJf", archive, "-C", destDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func ensureGitHubAsset(ctx context.Context, version, name, dir string) (string, error) {
	cached := filepath.Join(dir, name)
	if _, err := os.Stat(cached); err == nil {
		return cached, nil
	}

	url := fmt.Sprintf(releaseURLFmt, defaultRepo, version, name)
	telemetry.Info(ctx, "download: fetching asset", otellog.String("name", name))
	if err := downloadFile(ctx, url, cached); err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	telemetry.Info(ctx, "download: asset cached", otellog.String("path", cached))
	return cached, nil
}

// CheckVersionMismatch compares the CLI version against the latest GitHub
// release. Returns the latest version if an update is available.
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
