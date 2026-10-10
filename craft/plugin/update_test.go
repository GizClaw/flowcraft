package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// updateManifest is a plugin that declares where its updates come from.
func updateManifest(id, version, url string) string {
	return `{
		"id": "` + id + `", "version": "` + version + `",
		"permissions": ["mcp:provide"],
		"mcp": {"command": "true"},
		"update": {"url": "` + url + `"}
	}`
}

// updateStore opens a store with one plugin installed at version.
func updateStore(t *testing.T, version string) *Store {
	t.Helper()
	root := t.TempDir()
	writePlugin(t, root, "hello",
		updateManifest("hello", version, "https://example.test/hello.json"))
	return newRootStore(t, root)
}

// fakeInstaller scripts the application half of an update: it records
// how each step was reached, so a test can tell a refusal that happened
// before the fetch from one that happened after it.
type fakeInstaller struct {
	info     UpdateInfo
	checkErr error
	fetchErr error
	path     string

	checked bool
	fetched bool
	cleaned bool
	source  string
}

func (f *fakeInstaller) Check(_ context.Context, source string) (UpdateInfo, error) {
	f.checked = true
	f.source = source
	if f.checkErr != nil {
		return UpdateInfo{}, f.checkErr
	}
	return f.info, nil
}

func (f *fakeInstaller) Fetch(_ context.Context, _ UpdateInfo) (string, func(), error) {
	f.fetched = true
	cleanup := func() { f.cleaned = true }
	if f.fetchErr != nil {
		// A fetcher that wrote something before failing still hands the
		// cleanup over; craft has to release it either way.
		return "", cleanup, f.fetchErr
	}
	return f.path, cleanup, nil
}

// updatePackage writes one plugin package and returns its path and the
// sha256 spelling of its digest.
func updatePackage(t *testing.T, dir, id, version string) (string, string) {
	t.Helper()
	path := writeZip(t, dir, zipEntry{
		name: "plugin.json", data: manifestFor(id, version)})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	sum := sha256.Sum256(data)
	return path, "sha256:" + hex.EncodeToString(sum[:])
}

// TestUpdateFromInstallsANewerPackage covers the whole remote path with
// the transport faked out: craft asks the application for the update
// manifest, checks what it offers, verifies the package it gets back,
// and installs it through the ordinary pipeline.
func TestUpdateFromInstallsANewerPackage(t *testing.T) {
	t.Parallel()
	store := updateStore(t, "0.1.0")
	path, checksum := updatePackage(t, t.TempDir(), "hello", "0.2.0")
	installer := &fakeInstaller{
		info: UpdateInfo{
			Version:     "0.2.0",
			DownloadURL: "https://example.test/hello-0.2.0.zip",
			Checksum:    checksum,
			Changelog:   "faster",
		},
		path: path,
	}
	summary, err := store.UpdateFrom(context.Background(), "hello", installer)
	if err != nil || summary.Version != "0.2.0" {
		t.Fatalf("UpdateFrom = %+v, %v", summary, err)
	}
	if installer.source != "https://example.test/hello.json" {
		t.Fatalf("checked source = %q, want the manifest's update.url",
			installer.source)
	}
	if !installer.cleaned {
		t.Fatal("the fetched package was not cleaned up")
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Manifest.Version != "0.2.0" {
		t.Fatalf("entry after the update = %+v, %v", entry, ok)
	}
}

// TestUpdateFromRefusals walks the checks in order: each one has to fire
// before the step behind it, and the installed plugin has to survive
// every refusal untouched.
func TestUpdateFromRefusals(t *testing.T) {
	t.Parallel()
	packageFor := func(t *testing.T, id, version string) (string, string) {
		t.Helper()
		return updatePackage(t, t.TempDir(), id, version)
	}
	cases := []struct {
		name      string
		installer func(t *testing.T) Installer
		want      string
		// wantFetched pins how far the pipeline got.
		wantChecked bool
		wantFetched bool
	}{
		{
			name:      "no installer",
			installer: func(*testing.T) Installer { return nil },
			want:      "Installer is required",
		},
		{
			name: "check fails",
			installer: func(*testing.T) Installer {
				return &fakeInstaller{checkErr: errors.New("dial tcp: refused")}
			},
			want:        "dial tcp: refused",
			wantChecked: true,
		},
		{
			name: "version cannot be compared",
			installer: func(*testing.T) Installer {
				return &fakeInstaller{info: UpdateInfo{
					Version:     "next",
					DownloadURL: "https://example.test/hello.zip",
					Checksum:    "sha256:" + strings.Repeat("0", 64),
				}}
			},
			want:        `invalid version "next"`,
			wantChecked: true,
		},
		{
			name: "checksum shape",
			installer: func(*testing.T) Installer {
				return &fakeInstaller{info: UpdateInfo{
					Version:     "0.2.0",
					DownloadURL: "https://example.test/hello.zip",
					Checksum:    "md5:abc",
				}}
			},
			want:        "checksum must be sha256",
			wantChecked: true,
		},
		{
			name: "download url shape",
			installer: func(*testing.T) Installer {
				return &fakeInstaller{info: UpdateInfo{
					Version:     "0.2.0",
					DownloadURL: "ftp://example.test/hello.zip",
					Checksum:    "sha256:" + strings.Repeat("0", 64),
				}}
			},
			want:        "download_url must be an absolute http(s) URL",
			wantChecked: true,
		},
		{
			name: "not newer",
			installer: func(*testing.T) Installer {
				return &fakeInstaller{info: UpdateInfo{
					Version:     "0.1.0",
					DownloadURL: "https://example.test/hello.zip",
					Checksum:    "sha256:" + strings.Repeat("0", 64),
				}}
			},
			want:        "version 0.1.0 is not newer than 0.1.0",
			wantChecked: true,
		},
		{
			name: "fetch fails",
			installer: func(*testing.T) Installer {
				return &fakeInstaller{
					fetchErr: errors.New("no route to host"),
					info: UpdateInfo{
						Version:     "0.2.0",
						DownloadURL: "https://example.test/hello.zip",
						Checksum:    "sha256:" + strings.Repeat("0", 64),
					}}
			},
			want:        "no route to host",
			wantChecked: true,
			wantFetched: true,
		},
		{
			name: "checksum mismatch",
			installer: func(t *testing.T) Installer {
				path, _ := packageFor(t, "hello", "0.2.0")
				return &fakeInstaller{
					path: path,
					info: UpdateInfo{
						Version:     "0.2.0",
						DownloadURL: "https://example.test/hello.zip",
						Checksum:    "sha256:" + strings.Repeat("a", 64),
					}}
			},
			want:        "checksum mismatch",
			wantChecked: true,
			wantFetched: true,
		},
		{
			name: "package holds another plugin",
			installer: func(t *testing.T) Installer {
				path, checksum := packageFor(t, "other", "0.2.0")
				return &fakeInstaller{
					path: path,
					info: UpdateInfo{
						Version:     "0.2.0",
						DownloadURL: "https://example.test/other.zip",
						Checksum:    checksum,
					}}
			},
			want:        `package holds plugin "other", wanted "hello"`,
			wantChecked: true,
			wantFetched: true,
		},
		{
			name: "package is not what was announced",
			installer: func(t *testing.T) Installer {
				path, checksum := packageFor(t, "hello", "0.3.0")
				return &fakeInstaller{
					path: path,
					info: UpdateInfo{
						Version:     "0.2.0",
						DownloadURL: "https://example.test/hello.zip",
						Checksum:    checksum,
					}}
			},
			want:        "package version 0.3.0 does not match the announced 0.2.0",
			wantChecked: true,
			wantFetched: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			store := updateStore(t, "0.1.0")
			installer := testCase.installer(t)
			_, err := store.UpdateFrom(context.Background(), "hello", installer)
			if err == nil {
				t.Fatal("the refusal did not happen")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to mention %q", err, testCase.want)
			}
			entry, ok := store.Entry("hello")
			if !ok || entry.Manifest.Version != "0.1.0" {
				t.Fatalf("entry after the refusal = %+v, %v", entry, ok)
			}
			fake, ok := installer.(*fakeInstaller)
			if !ok {
				return
			}
			if fake.checked != testCase.wantChecked || fake.fetched != testCase.wantFetched {
				t.Fatalf("checked = %v, fetched = %v; want %v/%v",
					fake.checked, fake.fetched,
					testCase.wantChecked, testCase.wantFetched)
			}
			if fake.fetched && !fake.cleaned {
				t.Fatal("a fetched package was not cleaned up")
			}
		})
	}
}

// updatePackageWith writes a package whose manifest carries the given
// grant list, so a test can hand an update that asks for more than the
// installed plugin holds.
func updatePackageWith(
	t *testing.T,
	dir, id, version string,
	permissions ...string,
) (string, string) {
	t.Helper()
	quoted := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		quoted = append(quoted, `"`+permission+`"`)
	}
	manifest := `{
		"id": "` + id + `", "version": "` + version + `",
		"permissions": [` + strings.Join(quoted, ", ") + `],
		"mcp": {"command": "true"}
	}`
	path := writeZip(t, dir, zipEntry{name: "plugin.json", data: manifest})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	sum := sha256.Sum256(data)
	return path, "sha256:" + hex.EncodeToString(sum[:])
}

// approvalInstaller is a fakeInstaller that also plays the approver the
// update path asks before it widens a plugin's grants.
type approvalInstaller struct {
	fakeInstaller
	approveErr error
	approved   bool
	approval   UpdateApproval
}

func (a *approvalInstaller) ApproveUpdate(
	_ context.Context,
	_ string,
	approval UpdateApproval,
) error {
	a.approved = true
	a.approval = approval
	return a.approveErr
}

// TestUpdateFromNeedsApprovalForExpandedGrants covers the expansion an
// update may not slip past: a package that asks for permissions the
// installed manifest did not hold. Without an approver the update is
// refused before it is installed, an approver that objects keeps the
// plugin as it was, and one that agrees sees what it is agreeing to.
func TestUpdateFromNeedsApprovalForExpandedGrants(t *testing.T) {
	t.Parallel()
	newInfo := func(checksum string) UpdateInfo {
		return UpdateInfo{
			Version:     "0.2.0",
			DownloadURL: "https://example.test/hello-0.2.0.zip",
			Checksum:    checksum,
		}
	}
	installedAt := func(t *testing.T, store *Store) string {
		t.Helper()
		entry, ok := store.Entry("hello")
		if !ok {
			t.Fatal("hello is not installed")
		}
		return entry.Manifest.Version
	}
	// The installed plugin grants mcp:provide only.
	store := updateStore(t, "0.1.0")
	path, checksum := updatePackageWith(t, t.TempDir(), "hello", "0.2.0",
		"mcp:provide", "events:emit")

	refuser := &fakeInstaller{info: newInfo(checksum), path: path}
	_, err := store.UpdateFrom(context.Background(), "hello", refuser)
	if !errdefs.IsConflict(err) ||
		!strings.Contains(err.Error(), "does not approve update grants") {
		t.Fatalf("UpdateFrom without an approver = %v, want a Conflict", err)
	}
	if version := installedAt(t, store); version != "0.1.0" {
		t.Fatalf("installed version after the refusal = %s, want 0.1.0", version)
	}

	objector := &approvalInstaller{approveErr: errors.New("user said no")}
	objector.info = newInfo(checksum)
	objector.path = path
	_, err = store.UpdateFrom(context.Background(), "hello", objector)
	if err == nil || !strings.Contains(err.Error(), "user said no") {
		t.Fatalf("UpdateFrom with an objecting approver = %v", err)
	}
	if version := installedAt(t, store); version != "0.1.0" {
		t.Fatalf("installed version after the objection = %s, want 0.1.0", version)
	}

	agreer := &approvalInstaller{}
	agreer.info = newInfo(checksum)
	agreer.path = path
	summary, err := store.UpdateFrom(context.Background(), "hello", agreer)
	if err != nil || summary.Version != "0.2.0" {
		t.Fatalf("UpdateFrom with an agreeing approver = %+v, %v", summary, err)
	}
	if !agreer.approved {
		t.Fatal("the approver was never asked")
	}
	if !slices.Equal(agreer.approval.Permissions, []string{"events:emit"}) {
		t.Fatalf("approval = %+v, want the added permission only", agreer.approval)
	}
	if agreer.approval.ServerAdded {
		t.Fatalf("approval = %+v, want no added server", agreer.approval)
	}
}

// TestApprovalFor pins the diff itself: what the incoming manifest adds
// on top of the installed one, not what it declares.
func TestApprovalFor(t *testing.T) {
	t.Parallel()
	installed := Manifest{
		Permissions:      []string{"mcp:provide"},
		LegacyMCPServers: []MCPServer{{Name: "primary", Command: "true"}},
	}
	if same := approvalFor(installed, installed); !same.Empty() {
		t.Fatalf("an unchanged manifest asks for approval: %+v", same)
	}
	approval := approvalFor(installed, Manifest{
		Permissions: []string{"skills:provide", "mcp:provide", "events:emit"},
		LegacyMCPServers: []MCPServer{
			{Name: "primary", Command: "true"},
		},
	})
	if !slices.Equal(approval.Permissions,
		[]string{"events:emit", "skills:provide"}) {
		t.Fatalf("approval.Permissions = %v, want the added grants in name order",
			approval.Permissions)
	}
	if approval.ServerAdded || approval.Empty() {
		t.Fatalf("approval = %+v, want added grants and no added server", approval)
	}
	added := approvalFor(
		Manifest{Permissions: []string{"mcp:provide"}},
		Manifest{
			Permissions: []string{"mcp:provide"},
			MCP:         &MCPServer{Name: "primary", Command: "true"},
		},
	)
	if !added.ServerAdded {
		t.Fatalf("approval = %+v, want an added MCP server", added)
	}
	if added.String() != "an MCP server" {
		t.Fatalf("approval.String() = %q, want it to name the server",
			added.String())
	}
}

// TestUpdateFromWithoutASource covers the plugin that cannot be updated
// because it never said where to look.
func TestUpdateFromWithoutASource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	store := newRootStore(t, root)
	installer := &fakeInstaller{info: UpdateInfo{Version: "0.2.0"}}
	_, err := store.UpdateFrom(context.Background(), "hello", installer)
	if !errdefs.IsNotFound(err) || !strings.Contains(err.Error(), "update.url") {
		t.Fatalf("UpdateFrom = %v, want NotFound naming update.url", err)
	}
	if installer.checked {
		t.Fatal("a plugin without an update source was checked anyway")
	}
	if _, err := store.UpdateFrom(context.Background(), "missing", installer); !errdefs.IsNotFound(err) {
		t.Fatalf("UpdateFrom of an unknown plugin = %v, want NotFound", err)
	}
}

// TestUpdateURLShape covers what a manifest may declare: an absolute
// http(s) URL without credentials or fragment. The scheme is validated
// rather than restricted — https-only is transport policy and belongs to
// the Installer, not to the field.
func TestUpdateURLShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url  string
		want string
	}{
		{url: "https://example.test/hello.json"},
		{url: "http://example.test/hello.json"},
		{url: "", want: "is required"},
		{url: "ftp://example.test/hello.json", want: "must be an absolute http(s) URL"},
		{url: "https://user:pass@example.test/x", want: "without credentials"},
		{url: "https://example.test/x#frag", want: "or fragment"},
		{url: "https://", want: "must be an absolute http(s) URL"},
		{url: "/hello.json", want: "must be an absolute http(s) URL"},
		{url: "https://example.test/" + strings.Repeat("a", 2048),
			want: "exceeds"},
	}
	for _, testCase := range cases {
		t.Run(testCase.url, func(t *testing.T) {
			t.Parallel()
			manifest, err := ParseManifest(context.Background(),
				[]byte(updateManifest("hello", "0.1.0", testCase.url)))
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			err = manifest.Validate("")
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("Validate(%q) = %v, want accepted", testCase.url, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate(%q) = %v, want %q", testCase.url, err, testCase.want)
			}
		})
	}
}
