package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft/internal/version"
)

// maxUpdateURLLen bounds the update URLs a manifest may carry.
const maxUpdateURLLen = 2048

// UpdateInfo is the update manifest an update.url endpoint returns: the
// version it describes, where that version can be downloaded, and the
// checksum the package must have. It is the shape opencraft serves, so a
// plugin repository stays portable between the two.
type UpdateInfo struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	Checksum    string `json:"checksum"`
	Changelog   string `json:"changelog,omitempty"`
}

// Installer is the remote half of an update: the seam an application
// implements because transport is the application's to decide — TLS
// settings, proxies, credentials for a private feed, and which hosts a
// plugin is allowed to reach at all. craft validates what comes back,
// checks the package against its checksum and installs it through the
// same pipeline as any other source, so the policy half (https only,
// private and loopback ranges refused, redirects capped, timeouts) can
// live where the network stack already is.
type Installer interface {
	// Check fetches and returns the update manifest published at source,
	// a plugin's update.url.
	Check(ctx context.Context, source string) (UpdateInfo, error)
	// Fetch downloads the package info describes and returns its path on
	// disk plus the cleanup that removes it, and returns a cleanup even
	// when it fails after writing something. craft verifies the checksum
	// before it reads the file, so Fetch needs no opinion about what it
	// downloaded.
	Fetch(ctx context.Context, info UpdateInfo) (string, func(), error)
}

// UpdateApproval describes what an update would add on top of the
// installed plugin: the permissions the incoming manifest requests that
// the installed one did not, and whether it starts declaring an MCP
// server where the installed one declared none. The update source is the
// same source that served the version check, so nothing here verifies
// that the package is the same plugin asking for the same things; these
// are the parts a user would have to agree to, so craft asks before the
// package is installed.
type UpdateApproval struct {
	// Permissions are the grants the update adds, in name order.
	Permissions []string
	// ServerAdded reports an update that starts shipping an MCP server.
	ServerAdded bool
}

// Empty reports whether the update asks for nothing the installed
// manifest did not already have.
func (a UpdateApproval) Empty() bool {
	return len(a.Permissions) == 0 && !a.ServerAdded
}

// String renders an approval for an error message.
func (a UpdateApproval) String() string {
	parts := make([]string, 0, 2)
	if len(a.Permissions) > 0 {
		parts = append(parts, "permissions "+strings.Join(a.Permissions, ", "))
	}
	if a.ServerAdded {
		parts = append(parts, "an MCP server")
	}
	return strings.Join(parts, " and ")
}

// UpdateApprover is optionally implemented by an Installer that decides
// grants for the application. An update that requests new permissions or
// starts an MCP server is approved through it and refused when there is
// no approver: craft never approves a grant expansion on its own. A nil
// return approves; any error is reported to the caller and installs
// nothing. An update that asks for nothing new is not routed here at all.
type UpdateApprover interface {
	ApproveUpdate(
		ctx context.Context,
		id string,
		approval UpdateApproval,
	) error
}

// UpdateFrom checks one installed plugin's declared update source,
// verifies what it offers and installs it. The version rule is the one
// every install obeys (strictly newer than what is installed) and the
// package must hold the plugin that was asked for, so an endpoint
// cannot turn an update into a different plugin. An update that would
// add permissions, or that starts declaring an MCP server, needs the
// installer's approval (see UpdateApprover) before anything is
// installed.
func (s *Store) UpdateFrom(
	ctx context.Context,
	id string,
	installer Installer,
) (Summary, error) {
	if installer == nil {
		return Summary{}, errdefs.Validationf("plugin update: Installer is required")
	}
	entry, ok := s.Entry(id)
	if !ok {
		return Summary{}, errdefs.NotFoundf("plugin update: plugin %q not found", id)
	}
	if entry.Manifest.Update == nil {
		return Summary{}, errdefs.NotFoundf(
			"plugin update: %s declares no update.url", id)
	}
	info, err := installer.Check(ctx, strings.TrimSpace(entry.Manifest.Update.URL))
	if err != nil {
		return Summary{}, errdefs.NotAvailablef("plugin update: check %s: %v", id, err)
	}
	if err := info.validate(); err != nil {
		return Summary{}, err
	}
	if version.Compare(info.Version, entry.Manifest.Version) <= 0 {
		return Summary{}, errdefs.Conflictf(
			"plugin update: %s version %s is not newer than %s",
			id, info.Version, entry.Manifest.Version)
	}
	zipPath, cleanup, err := installer.Fetch(ctx, info)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return Summary{}, errdefs.NotAvailablef("plugin update: fetch %s: %v", id, err)
	}
	if err := verifyChecksum(zipPath, info.Checksum); err != nil {
		return Summary{}, err
	}
	dir, cleanupZip, err := extractZip(zipPath)
	if err != nil {
		return Summary{}, err
	}
	defer cleanupZip()
	pkg, manifest, err := s.inspect(dir)
	if err != nil {
		return Summary{}, err
	}
	if pkg.ID != id {
		return Summary{}, errdefs.Validationf(
			"plugin update: package holds plugin %q, wanted %q", pkg.ID, id)
	}
	if pkg.Version != info.Version {
		return Summary{}, errdefs.Conflictf(
			"plugin update: package version %s does not match the announced %s",
			pkg.Version, info.Version)
	}
	if approval := approvalFor(entry.Manifest, manifest); !approval.Empty() {
		approver, ok := installer.(UpdateApprover)
		if !ok {
			return Summary{}, errdefs.Conflictf(
				"plugin update: %s %s adds %s; the installer does not "+
					"approve update grants — install the package explicitly "+
					"if the expansion is intended",
				id, info.Version, approval)
		}
		if err := approver.ApproveUpdate(ctx, id, approval); err != nil {
			return Summary{}, fmt.Errorf(
				"plugin update: %s %s: approve grants: %w", id, info.Version, err)
		}
	}
	return s.Install(ctx, dir)
}

// approvalFor diffs an incoming manifest against the installed one.
func approvalFor(installed, incoming Manifest) UpdateApproval {
	granted := make(map[string]struct{}, len(installed.Permissions))
	for _, permission := range installed.Permissions {
		granted[permission] = struct{}{}
	}
	var approval UpdateApproval
	for _, permission := range incoming.Permissions {
		if _, ok := granted[permission]; ok {
			continue
		}
		approval.Permissions = append(approval.Permissions, permission)
	}
	sort.Strings(approval.Permissions)
	approval.ServerAdded = len(incoming.Servers()) > 0 &&
		len(installed.Servers()) == 0
	return approval
}

// validate checks what craft relies on before anything is fetched or
// trusted: a version it can compare, an http(s) download URL, and a
// sha256 checksum.
func (info UpdateInfo) validate() error {
	if !version.Valid(info.Version) {
		return errdefs.Validationf(
			"plugin update: invalid version %q", info.Version)
	}
	if _, err := parseChecksum(info.Checksum); err != nil {
		return err
	}
	if err := checkRemoteURL(info.DownloadURL); err != nil {
		return errdefs.Validationf("plugin update: download_url %v", err)
	}
	return nil
}

// checkRemoteURL reports why a URL craft may hand to transport is not a
// shape it accepts: absolute http(s), no embedded credentials, no
// fragment. Absolute and credential-free are the parts craft can decide
// without a network; everything else about a request belongs to the
// transport that makes it.
func checkRemoteURL(raw string) error {
	text := strings.TrimSpace(raw)
	if text == "" {
		return errdefs.Validationf("is required")
	}
	if len(text) > maxUpdateURLLen {
		return errdefs.Validationf(
			"exceeds %d bytes", maxUpdateURLLen)
	}
	parsed, err := neturl.Parse(text)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errdefs.Validationf(
			"must be an absolute http(s) URL without credentials or fragment (got %q)",
			text)
	}
	return nil
}

// parseChecksum reads the "sha256:<64 hex>" spelling an update manifest
// uses and returns the lowercase digest.
func parseChecksum(text string) (string, error) {
	digest, found := strings.CutPrefix(strings.TrimSpace(text), "sha256:")
	if !found {
		return "", errdefs.Validationf(
			"plugin update: checksum must be sha256:<hex>")
	}
	if len(digest) != sha256.Size*2 {
		return "", errdefs.Validationf(
			"plugin update: checksum must be sha256:<64 hex chars>")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", errdefs.Validationf(
			"plugin update: checksum is not valid hex")
	}
	return strings.ToLower(digest), nil
}

// verifyChecksum hashes a fetched package and compares it with the
// announced digest. The transport is the application's, but nothing
// enters the install pipeline unverified, and nothing past the package
// cap is hashed at all.
func verifyChecksum(path, checksum string) error {
	want, err := parseChecksum(checksum)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return errdefs.Validationf("plugin update: open package: %v", err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, MaxZipBytes+1))
	if err != nil {
		return errdefs.Validationf("plugin update: read package: %v", err)
	}
	if written > MaxZipBytes {
		return errdefs.Validationf(
			"plugin update: package exceeds %d MiB", MaxZipBytes>>20)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != want {
		return errdefs.Conflictf(
			"plugin update: checksum mismatch (want sha256:%s, got sha256:%s)",
			want, got)
	}
	return nil
}
