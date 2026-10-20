package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// The workspace contract: the rules `core/workspace` enforces *around* a backend
// -- traversal, the scope's two directions, the prefixed view, walking and
// globbing, the read cap, atomic publish -- and the two sentences a bounded read
// can fail with.
//
// The medium in every row is a map of path to bytes, written twice: once here and
// once in the Rust crate (`flowcraft_workspace::mem`). Core has no in-memory
// backend of its own, so the map is the harness's, and that is deliberate: it has
// no temporary directory, no platform sorting and no permission bits, so a row can
// only fail because of the code under it. A row never asserts the medium's own
// answers where core has an opinion -- `raw-ops` is the exception, and it is there
// to prove the two harnesses agree before the other rows lean on them.
//
// What each row is evidence of is in its `why`. The interesting ones:
//
//   - `sub-*` goes through core's own `cleanPath`, because `Sub` validates its
//     prefix with it: a prefix of `../x` and one of `..a` are both refused, and the
//     sentence is the same one a path gets.
//   - `scope-*` goes through core's `matchesAny`, which owns the four pattern
//     forms; each pattern is a row so a form that stops working has a name.
//   - `capped-*` distinguishes the two refusal sentences, which is what proves the
//     port asked the *type* question ("can this backend bound a read?") rather
//     than calling a method and hoping.
//   - `atomic-*` pins the temp name (`.a.txt.tmp` beside the target) because a
//     failed rename names it, and pins that the temp file is gone afterwards.
//
// The `local` section is the other half: the same questions over a real directory
// tree, because that is where the backend's rules actually live. Its medium is a
// temporary directory (fixtures may be symlinks, and a symlink may point outside),
// so a row there is built from two areas: `root`, the workspace, and `outside`, a
// sibling the links reach for. What the two filesystems must agree on is the
// *outcome* -- and, per row, either core's sentence or, when the host wrote it, the
// classification. `kind` names that classification; `internal` is the marker for
// "the host's words", which is why those rows are the ones a reader must not
// compare word for word.

// wsContract is the committed document.
type wsContract struct {
	// Note says what the table is for.
	Note []string `json:"note"`
	// Masked names what a reader must not expect to compare. Nothing is masked:
	// the medium is a map, so no path in this file depends on the run.
	Masked []string `json:"masked"`
	// Cases are the fixtures and their answers.
	Cases []wsCase `json:"cases"`
	// Local is the same table over a real directory tree.
	Local wsLocal `json:"local"`
}

// wsFile is one entry the harness writes into the medium.
type wsFile struct {
	// Path is where it is written, relative to the view.
	Path string `json:"path"`
	// Data is the file's bytes.
	Data string `json:"data,omitempty"`
	// Symlink, when set, makes the entry a symlink to that target instead of a
	// file. A target of the form "abs:<path>" is made absolute against the
	// workspace root, so no row has to name the run's temporary directory.
	Symlink string `json:"symlink,omitempty"`
	// Area is "outside" for a fixture that lives beside the workspace rather than
	// in it, which is how a link out of the tree gets a target. Empty means the
	// workspace.
	Area string `json:"area,omitempty"`
}

// wsCase is one medium, one view over it, and the questions put to the view.
type wsCase struct {
	// Name identifies the case in a mismatch.
	Name string `json:"name"`
	// Why says what the case is evidence of.
	Why string `json:"why"`
	// Files is the medium's contents before the operations.
	Files []wsFile `json:"files"`
	// Kind is "raw" (the medium itself), "scope" (a ScopedWorkspace over it) or
	// "sub" (one Sub per entry of Prefixes, nested).
	Kind string `json:"kind,omitempty"`
	// Prefixes are the Sub prefixes, outermost last.
	Prefixes []string `json:"prefixes,omitempty"`
	// DenyRead, AllowWrite and MandatoryDeny are the scope's patterns.
	DenyRead      []string `json:"deny_read,omitempty"`
	AllowWrite    []string `json:"allow_write,omitempty"`
	MandatoryDeny []string `json:"mandatory_deny,omitempty"`
	// Limited makes the medium answer bounded reads.
	Limited bool `json:"limited,omitempty"`
	// RefuseRename makes the medium fail every rename, so the atomic publish's
	// failure path can be observed.
	RefuseRename bool `json:"refuse_rename,omitempty"`
	// Ops are the questions, in order.
	Ops []wsOp `json:"ops"`
	// Final is the medium's contents after the operations.
	Final []wsFile `json:"final"`
}

// wsOp is one operation and its answer.
type wsOp struct {
	// Op is the operation: read, write, append, rename, delete, remove_all, list,
	// exists, stat, walk, walk_skip, glob, capped, atomic or copy.
	Op string `json:"op"`
	// Path is the operation's subject, or a glob's pattern, or a walk's root.
	Path string `json:"path,omitempty"`
	// Arg is a second path (rename's destination, copy's destination), the bytes
	// to write, or the path a walk skips.
	Arg string `json:"arg,omitempty"`
	// Max is the read cap.
	Max int64 `json:"max,omitempty"`
	// OK reports whether the operation returned no error.
	OK bool `json:"ok"`
	// Data is a read's contents, a stat's summary, or a glob's pattern.
	Data string `json:"data,omitempty"`
	// Bool is exists' answer.
	Bool bool `json:"bool,omitempty"`
	// List is a list's, walk's or glob's answer.
	List []wsEntry `json:"list,omitempty"`
	// Error is the refusal's text, empty on success.
	Error string `json:"error,omitempty"`
	// Violations are the records the scope logged for this operation.
	Violations []wsViolation `json:"violations,omitempty"`
	// Kind is the error's classification, for the rows where the sentence alone
	// would over-assert: "internal" means the host wrote the words, so only the
	// outcome and this field are comparable. Empty on success, and empty in the
	// map-medium rows, whose sentences are core's own.
	Kind string `json:"kind,omitempty"`
}

// wsEntry is one name or path in an answer.
type wsEntry struct {
	// Path is the entry's name (for List) or its path (for Walk and Glob).
	Path string `json:"path"`
	// Dir reports whether it is a directory.
	Dir bool `json:"dir,omitempty"`
}

// wsViolation is one violation record, without its clock.
type wsViolation struct {
	// Operation is the refused direction.
	Operation string `json:"operation"`
	// Path is the path as the caller wrote it.
	Path string `json:"path"`
	// Reason is the rule that refused.
	Reason string `json:"reason"`
}

// wsLocal is the real-filesystem half of the table.
type wsLocal struct {
	// Note says what the section is for.
	Note []string `json:"note"`
	// Masked names what a reader must not compare word for word.
	Masked []string `json:"masked"`
	// Cases are the trees and their answers.
	Cases []wsLocalCase `json:"cases"`
}

// wsLocalCase is one temporary tree and the questions put to it.
type wsLocalCase struct {
	// Name identifies the case in a mismatch.
	Name string `json:"name"`
	// Why says what the case is evidence of.
	Why string `json:"why"`
	// Kind is "raw" (the workspace itself) or "sub" (the backend's own Sub, which
	// creates the directory and shares the handle).
	Kind string `json:"kind,omitempty"`
	// Prefixes are the Sub prefixes, outermost last.
	Prefixes []string `json:"prefixes,omitempty"`
	// Files are the fixtures, in the workspace or beside it.
	Files []wsFile `json:"files,omitempty"`
	// Build is the refusal a Sub answered with instead of a view: the prefix a
	// symlink made escape the root is refused before any operation runs.
	Build string `json:"build,omitempty"`
	// BuildKind is that refusal's classification.
	BuildKind string `json:"build_kind,omitempty"`
	// Ops are the questions, in order.
	Ops []wsOp `json:"ops,omitempty"`
	// Final is the workspace tree after the operations, directories included.
	Final []wsEntry `json:"final"`
	// FinalOutside is the tree beside the workspace, which is how a row shows
	// that removing a link never removed its target.
	FinalOutside []wsFile `json:"final_outside,omitempty"`
}

// writeWorkspaceContract dumps the table to out.
func writeWorkspaceContract(out io.Writer) error {
	document := wsContract{
		Note: []string{
			"The rules core/workspace enforces around any backend: traversal, the scope's two directions, the prefixed view, walking, globbing, the read cap and atomic publish.",
			"The medium is a map of path to bytes, implemented twice (core's oracle and the port's crate) because core ships no in-memory backend; a row asserts what the code *around* the medium answers.",
			"Nothing is masked: the medium has no temporary directory and no clock in it, so every string here is a function of this file.",
			"Not covered here: anything that needs a real filesystem (symlink escape, a directory handle, permission bits). That is the local backend's slice.",
		},
		Masked: []string{"nothing: the medium is a map, so no path in this file depends on the run"},
	}
	for _, testCase := range workspaceCases() {
		document.Cases = append(document.Cases, runCase(testCase))
	}
	document.Local = wsLocal{
		Note: []string{
			"The same questions over a real directory tree: the local backend is where a path can leave the tree, and where the host, not core, writes some of the words.",
			"Each case builds two areas: `root`, the workspace, and `outside`, a sibling the fixtures' symlinks reach for. A symlink target of `abs:<path>` is made absolute against the workspace root, so no row names the run's temporary directory.",
			"`kind` classifies every error the way core's callers read it. When it is `internal`, the host wrote the sentence (`openat a: no such file or directory` on one side, `a path led outside of the filesystem` on the other), so the row asserts the outcome and the kind and not the words. Every other kind carries core's own sentence, and is compared exactly.",
			"Not covered here: permission bits (the harness cannot make them portable), and Windows (its own slice, `docs/plans/2026-10-15-rust-core-port.md` section 12).",
		},
		Masked: []string{
			"the sentence of every row whose kind is internal: the host's words, per platform and per implementation of the directory handle",
		},
	}
	for _, testCase := range localCases() {
		run, err := localRoot(testCase)
		if err != nil {
			return err
		}
		document.Local.Cases = append(document.Local.Cases, run)
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

// memWorkspace is the harness's medium: a map of path to bytes, with directories
// implied by the paths. It is not core code and does not pretend to be; every
// answer it gives is either the medium's own (raw-ops) or core's (everything else).
type memWorkspace struct {
	files        map[string][]byte
	refuseRename bool
}

func newMemWorkspace(files []wsFile) *memWorkspace {
	medium := &memWorkspace{files: map[string][]byte{}}
	for _, file := range files {
		key, err := memKey(file.Path)
		if err != nil {
			panic(fmt.Sprintf("harness: fixture path %q is invalid: %v", file.Path, err))
		}
		medium.files[key] = []byte(file.Data)
	}
	return medium
}

// memKey validates path and folds the root spellings together.
//
// Core's own `cleanPath` is unexported, so the same rule is spelled again here:
// `path.Clean`, then refuse an absolute path or one that begins with `..`. The
// three root spellings are the medium's rule, not core's.
func memKey(path string) (string, error) {
	if path == "" || path == "." {
		return "", nil
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." {
		return "", nil
	}
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("%w: %s", workspace.ErrPathTraversal, path)
	}
	return cleaned, nil
}

// isDir reports whether some file lives under path.
func (m *memWorkspace) isDir(path string) bool {
	prefix := path + "/"
	for key := range m.files {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func (m *memWorkspace) Read(_ context.Context, path string) ([]byte, error) {
	key, err := memKey(path)
	if err != nil {
		return nil, err
	}
	data, ok := m.files[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", workspace.ErrNotFound, path)
	}
	return append([]byte(nil), data...), nil
}

func (m *memWorkspace) Write(_ context.Context, path string, data []byte) error {
	key, err := memKey(path)
	if err != nil {
		return err
	}
	if key == "" || m.isDir(key) {
		return fmt.Errorf("mem: %s is a directory", path)
	}
	m.files[key] = append([]byte(nil), data...)
	return nil
}

func (m *memWorkspace) Append(_ context.Context, path string, data []byte) error {
	key, err := memKey(path)
	if err != nil {
		return err
	}
	if key == "" || m.isDir(key) {
		return fmt.Errorf("mem: %s is a directory", path)
	}
	m.files[key] = append(m.files[key], data...)
	return nil
}

func (m *memWorkspace) Rename(_ context.Context, src, dst string) error {
	srcKey, err := memKey(src)
	if err != nil {
		return err
	}
	dstKey, err := memKey(dst)
	if err != nil {
		return err
	}
	if m.refuseRename {
		return fmt.Errorf("fixture: rename refused")
	}
	data, ok := m.files[srcKey]
	if !ok {
		return fmt.Errorf("%w: %s", workspace.ErrNotFound, src)
	}
	delete(m.files, srcKey)
	m.files[dstKey] = data
	return nil
}

func (m *memWorkspace) Delete(_ context.Context, path string) error {
	key, err := memKey(path)
	if err != nil {
		return err
	}
	if _, ok := m.files[key]; ok {
		delete(m.files, key)
		return nil
	}
	if key == "" || m.isDir(key) {
		return fmt.Errorf("mem: %s is a directory", path)
	}
	return nil
}

func (m *memWorkspace) RemoveAll(_ context.Context, path string) error {
	key, err := memKey(path)
	if err != nil {
		return err
	}
	if key == "" {
		// The medium's own refusal, spelled differently from core's `Sub` so a
		// row says which layer answered.
		return fmt.Errorf("mem: refusing to remove root")
	}
	prefix := key + "/"
	for candidate := range m.files {
		if candidate == key || strings.HasPrefix(candidate, prefix) {
			delete(m.files, candidate)
		}
	}
	return nil
}

func (m *memWorkspace) List(_ context.Context, dir string) ([]fs.DirEntry, error) {
	key, err := memKey(dir)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if key != "" {
		prefix = key + "/"
	}
	entries := map[string]bool{}
	for candidate := range m.files {
		rest, ok := strings.CutPrefix(candidate, prefix)
		if !ok || rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			entries[rest[:i]] = true
			continue
		}
		if _, seen := entries[rest]; !seen {
			entries[rest] = false
		}
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]fs.DirEntry, 0, len(names))
	for _, name := range names {
		out = append(out, memEntry{name: name, dir: entries[name]})
	}
	return out, nil
}

func (m *memWorkspace) Exists(_ context.Context, path string) (bool, error) {
	key, err := memKey(path)
	if err != nil {
		return false, err
	}
	if key == "" {
		return true, nil
	}
	if _, ok := m.files[key]; ok {
		return true, nil
	}
	return m.isDir(key), nil
}

func (m *memWorkspace) Stat(_ context.Context, path string) (fs.FileInfo, error) {
	key, err := memKey(path)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return memEntry{name: "/", dir: true}, nil
	}
	if data, ok := m.files[key]; ok {
		return memInfo{name: filepath.Base(key), size: int64(len(data))}, nil
	}
	if m.isDir(key) {
		return memInfo{name: filepath.Base(key), dir: true}, nil
	}
	return nil, fmt.Errorf("%w: %s", workspace.ErrNotFound, path)
}

// limitedWorkspace is the same medium, answering bounded reads: the type exists
// because core's `Capped` asks a *type* question, and a row has to be able to
// answer yes or no.
type limitedWorkspace struct{ *memWorkspace }

func (l limitedWorkspace) ReadLimited(_ context.Context, path string, maxBytes int64) ([]byte, error) {
	key, err := memKey(path)
	if err != nil {
		return nil, err
	}
	data, ok := l.files[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", workspace.ErrNotFound, path)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("workspace: %s exceeds %d bytes", path, maxBytes)
	}
	return append([]byte(nil), data...), nil
}

// memEntry is a directory entry.
type memEntry struct {
	name string
	dir  bool
}

func (e memEntry) Name() string               { return e.name }
func (e memEntry) IsDir() bool                { return e.dir }
func (e memEntry) Type() fs.FileMode          { return e.Mode().Type() }
func (e memEntry) Info() (fs.FileInfo, error) { return e, nil }
func (e memEntry) Mode() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e memEntry) Size() int64        { return 0 }
func (e memEntry) ModTime() time.Time { return time.Time{} }
func (e memEntry) Sys() any           { return nil }

// memInfo is a file's metadata.
type memInfo struct {
	name string
	size int64
	dir  bool
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return memEntry{i.name, i.dir}.Mode() }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }

// recordingLogger keeps the records a scope emits, in order.
type recordingLogger struct {
	records []workspace.ViolationRecord
}

func (r *recordingLogger) LogViolation(_ context.Context, record workspace.ViolationRecord) {
	r.records = append(r.records, record)
}

// runOp performs one operation and records its answer.
//
// The error comes back too: the map-medium rows only need its sentence, while the
// real-tree rows also record how it classifies, because there the sentence is not
// always core's (see the file's opening comment).
func runOp(ws workspace.Workspace, logger *recordingLogger, in wsOp) (wsOp, error) {
	ctx := context.Background()
	out := wsOp{Op: in.Op, Path: in.Path, Arg: in.Arg, Max: in.Max}
	var err error

	switch in.Op {
	case "read":
		var data []byte
		data, err = ws.Read(ctx, in.Path)
		out.Data = string(data)
	case "write":
		err = ws.Write(ctx, in.Path, []byte(in.Arg))
	case "append":
		err = ws.Append(ctx, in.Path, []byte(in.Arg))
	case "rename":
		err = ws.Rename(ctx, in.Path, in.Arg)
	case "delete":
		err = ws.Delete(ctx, in.Path)
	case "remove_all":
		err = ws.RemoveAll(ctx, in.Path)
	case "list":
		var entries []fs.DirEntry
		entries, err = ws.List(ctx, in.Path)
		for _, entry := range entries {
			out.List = append(out.List, wsEntry{Path: entry.Name(), Dir: entry.IsDir()})
		}
	case "exists":
		out.Bool, err = ws.Exists(ctx, in.Path)
	case "stat":
		var info fs.FileInfo
		info, err = ws.Stat(ctx, in.Path)
		if info != nil {
			// A directory's size is the host's -- an inode's blocks on one
			// filesystem, nothing on another -- so the row states zero for it, as
			// core's in-memory medium does. The port reports the same.
			size := info.Size()
			if info.IsDir() {
				size = 0
			}
			out.Data = fmt.Sprintf("%s:%d:%t", info.Name(), size, info.IsDir())
		}
	case "walk":
		err = workspace.Walk(ctx, ws, in.Path, func(path string, entry fs.DirEntry) error {
			out.List = append(out.List, wsEntry{Path: path, Dir: entry.IsDir()})
			return nil
		})
	case "walk_skip":
		err = workspace.Walk(ctx, ws, in.Path, func(path string, entry fs.DirEntry) error {
			if path == in.Arg && entry.IsDir() {
				return filepath.SkipDir
			}
			out.List = append(out.List, wsEntry{Path: path, Dir: entry.IsDir()})
			return nil
		})
	case "glob":
		out.Data = in.Path
		var matches []string
		matches, err = workspace.Glob(ctx, ws, in.Path)
		for _, match := range matches {
			out.List = append(out.List, wsEntry{Path: match})
		}
	case "capped":
		var data []byte
		data, err = workspace.Capped(ctx, ws, in.Path, in.Max)
		out.Data = string(data)
	case "atomic":
		err = workspace.AtomicWrite(ctx, ws, in.Path, []byte(in.Arg))
	case "copy":
		err = workspace.Copy(ctx, ws, in.Path, in.Arg)
	case "capabilities":
		// The optional interface, asked as core's callers ask it: a backend that
		// does not report is the conservative all-false answer.
		caps := workspace.CapabilitiesOf(ws)
		out.Data = fmt.Sprintf("%t,%t,%t,%t",
			caps.AtomicRename, caps.ReadAfterWrite, caps.DurableOnWrite, caps.Distributed)
	default:
		err = fmt.Errorf("harness: unknown op %q", in.Op)
	}

	out.Error = errText(err)
	out.OK = err == nil
	if logger != nil {
		for _, record := range logger.records {
			out.Violations = append(out.Violations, wsViolation{
				Operation: record.Operation,
				Path:      record.Path,
				Reason:    record.Reason,
			})
		}
		logger.records = nil
	}
	return out, err
}

// errText is the error's sentence, or the empty string.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// runCase performs a case's operations and fills in their answers.
func runCase(testCase wsCase) wsCase {
	medium := newMemWorkspace(testCase.Files)
	medium.refuseRename = testCase.RefuseRename
	ws, logger := viewOver(medium, testCase)
	for i, in := range testCase.Ops {
		testCase.Ops[i], _ = runOp(ws, logger, in)
	}
	testCase.Final = filesOf(medium)
	return testCase
}

// viewOver is buildView with an already-built medium.
func viewOver(medium *memWorkspace, testCase wsCase) (workspace.Workspace, *recordingLogger) {
	var ws workspace.Workspace = medium
	if testCase.Limited {
		ws = limitedWorkspace{medium}
	}
	logger := &recordingLogger{}
	switch testCase.Kind {
	case "sub":
		for _, prefix := range testCase.Prefixes {
			ws = workspace.Sub(ws, prefix)
		}
	case "scope":
		ws = workspace.NewScopedWorkspace(ws,
			workspace.WithDenyRead(testCase.DenyRead...),
			workspace.WithAllowWrite(testCase.AllowWrite...),
			workspace.WithMandatoryDeny(testCase.MandatoryDeny...),
			workspace.WithViolationLogger(logger),
		)
	}
	return ws, logger
}

// filesOf is snapshot for a medium already in hand.
func filesOf(medium *memWorkspace) []wsFile {
	paths := make([]string, 0, len(medium.files))
	for path := range medium.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]wsFile, 0, len(paths))
	for _, path := range paths {
		out = append(out, wsFile{Path: path, Data: string(medium.files[path])})
	}
	return out
}

// kindOf classifies an error the way core's callers read it.
//
// `internal` is the answer for everything core did not wrap in one of its own
// markers, which for the local backend means the host wrote the sentence: those
// rows are the ones whose words cannot be compared between two filesystems.
func kindOf(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, workspace.ErrNotFound):
		return "not_found"
	case errors.Is(err, workspace.ErrPathTraversal):
		return "forbidden"
	case errdefs.IsValidation(err):
		return "validation"
	case errdefs.IsForbidden(err):
		return "forbidden"
	case errdefs.IsNotAvailable(err):
		return "not_available"
	case errdefs.IsConflict(err):
		return "conflict"
	case errdefs.IsTimeout(err):
		return "timeout"
	default:
		return "internal"
	}
}

// localRoot builds one case's temporary tree and answers its questions.
//
// The two areas are the point of the section: `root` is the workspace and
// `outside` is a sibling, so a symlink can be made to leave the tree and a row can
// show that removing it removed the link, not what it pointed at.
func localRoot(testCase wsLocalCase) (wsLocalCase, error) {
	base, err := os.MkdirTemp("", "flowcraft-workspace-")
	if err != nil {
		return testCase, fmt.Errorf("harness: create the temporary tree: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(base)
	}()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, area := range []string{root, outside} {
		if err := os.MkdirAll(area, 0o700); err != nil {
			return testCase, fmt.Errorf("harness: create %s: %w", area, err)
		}
	}
	for _, fixture := range testCase.Files {
		if err := applyLocalFixture(root, outside, fixture); err != nil {
			return testCase, err
		}
	}

	workspaceRoot, err := workspace.NewLocalWorkspace(root)
	if err != nil {
		return testCase, fmt.Errorf("harness: open the workspace: %w", err)
	}
	defer func() {
		_ = workspaceRoot.Close()
	}()

	var view workspace.Workspace = workspaceRoot
	for _, prefix := range testCase.Prefixes {
		sub, subErr := workspaceRoot.Sub(prefix)
		if subErr != nil {
			// The view a link made escape is refused before any operation runs, so
			// the row records the refusal instead of a tree's answers.
			testCase.Build = subErr.Error()
			testCase.BuildKind = kindOf(subErr)
			testCase.Final = treeOf(root)
			testCase.FinalOutside = filesOfTree(outside)
			return testCase, nil
		}
		view = sub
	}

	for i, in := range testCase.Ops {
		out, opErr := runOp(view, nil, in)
		out.Kind = kindOf(opErr)
		testCase.Ops[i] = out
	}
	testCase.Final = treeOf(root)
	testCase.FinalOutside = filesOfTree(outside)
	return testCase, nil
}

// applyLocalFixture writes one fixture into its area.
func applyLocalFixture(root, outside string, fixture wsFile) error {
	area := root
	if fixture.Area == "outside" {
		area = outside
	}
	target := filepath.Join(area, fixture.Path)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("harness: create the parent of %q: %w", fixture.Path, err)
	}
	if fixture.Symlink == "" {
		if err := os.WriteFile(target, []byte(fixture.Data), 0o600); err != nil {
			return fmt.Errorf("harness: write %q: %w", fixture.Path, err)
		}
		return nil
	}
	link := fixture.Symlink
	if rest, ok := strings.CutPrefix(link, "abs:"); ok {
		// Absolute against the workspace's root, spelled by the harness so the row
		// does not carry the run's temporary directory.
		link = filepath.Join(root, rest)
	}
	if err := os.Symlink(link, target); err != nil {
		return fmt.Errorf("harness: symlink %q: %w", fixture.Path, err)
	}
	return nil
}

// treeOf lists a directory tree, directories included, in walk order.
func treeOf(root string) []wsEntry {
	entries := []wsEntry{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		entries = append(entries, wsEntry{Path: filepath.ToSlash(rel), Dir: entry.IsDir()})
		return nil
	})
	if err != nil {
		panic(fmt.Sprintf("harness: walk %s: %v", root, err))
	}
	return entries
}

// filesOfTree lists a directory tree's regular files, with their bytes.
func filesOfTree(root string) []wsFile {
	files := []wsFile{}
	for _, entry := range treeOf(root) {
		if entry.Dir {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			panic(fmt.Sprintf("harness: read %s: %v", entry.Path, err))
		}
		files = append(files, wsFile{Path: entry.Path, Data: string(data)})
	}
	return files
}

// workspaceCases is the table.
func workspaceCases() []wsCase {
	// file and read keep the table readable: most rows are "these files, then
	// these questions".
	file := func(path, data string) wsFile { return wsFile{Path: path, Data: data} }
	op := func(name, path string) wsOp { return wsOp{Op: name, Path: path} }
	read := func(path string) wsOp { return op("read", path) }

	// The tree the walk and glob rows share.
	tree := []wsFile{
		file("a.txt", "a"),
		file("dir/b.txt", "b"),
		file("dir/sub/c.go", "c"),
		file("dir/sub/deep/d.go", "d"),
		file("other/e.go", "e"),
	}

	// The ten paths the four pattern forms are asked about, in one scope.
	formFiles := []wsFile{file("secrets/key.txt", "s"), file("out/a.txt", "a")}

	return []wsCase{
		{
			Name:  "raw-ops",
			Why:   "the medium's own answers, so the two harnesses agree before any other row leans on them: parents are implied, delete and remove_all are idempotent, rename publishes",
			Files: []wsFile{file("a.txt", "a"), file("b/c.txt", "c")},
			Ops: []wsOp{
				read("a.txt"),
				op("list", ""),
				op("list", "b"),
				op("exists", "a.txt"),
				op("exists", "zz"),
				op("stat", "a.txt"),
				op("stat", "b"),
				{Op: "write", Path: "d/e.txt", Arg: "e"},
				{Op: "append", Path: "a.txt", Arg: "!"},
				read("a.txt"),
				{Op: "rename", Path: "a.txt", Arg: "f.txt"},
				op("delete", "f.txt"),
				op("delete", "f.txt"),
				op("remove_all", "b"),
				op("list", ""),
			},
		},
		{
			Name:  "raw-not-found",
			Why:   "a missing path is a refusal for read, stat and rename's source, and not for list, delete or remove_all",
			Files: []wsFile{file("a.txt", "a")},
			Ops: []wsOp{
				read("nope.txt"),
				op("stat", "nope.txt"),
				{Op: "rename", Path: "nope.txt", Arg: "x.txt"},
				op("list", "nope"),
				op("delete", "nope.txt"),
				op("remove_all", "nope"),
			},
		},
		{
			Name:  "raw-traversal",
			Why:   "an absolute path or one that climbs out is refused by the medium, and a path that merely visits a .. on its way somewhere is not",
			Files: []wsFile{file("a.txt", "a")},
			Ops: []wsOp{
				read("../x"),
				{Op: "write", Path: "/abs", Arg: "x"},
				op("list", "..a"),
				{Op: "delete", Path: "a/../../b"},
				read("./a.txt"),
				read("b/../a.txt"),
			},
		},
		{
			Name:  "raw-root",
			Why:   "the root is a directory for writes and reads, present for exists and stat, and cannot be removed by remove_all",
			Files: []wsFile{file("a.txt", "a")},
			Ops: []wsOp{
				op("remove_all", ""),
				op("remove_all", "."),
				{Op: "write", Path: "", Arg: "x"},
				op("exists", ""),
				op("stat", ""),
				op("list", ""),
			},
		},
		{
			Name:    "capped-bounded",
			Why:     "a medium that can bound a read refuses an oversized file at the read layer, with the sentence that says so",
			Files:   []wsFile{file("a.txt", "0123456789")},
			Limited: true,
			Ops: []wsOp{
				{Op: "capped", Path: "a.txt", Max: 10},
				{Op: "capped", Path: "a.txt", Max: 9},
				{Op: "capped", Path: "a.txt", Max: 20},
				{Op: "capped", Path: "nope.txt", Max: 5},
				{Op: "capped", Path: "a.txt", Max: 0},
			},
		},
		{
			Name:  "capped-fallback",
			Why:   "a medium that cannot bound a read is read in full and then measured, and says a different sentence -- so a reader can tell which path it took",
			Files: []wsFile{file("a.txt", "0123456789")},
			Ops: []wsOp{
				{Op: "capped", Path: "a.txt", Max: 10},
				{Op: "capped", Path: "a.txt", Max: 9},
				{Op: "capped", Path: "nope.txt", Max: 5},
			},
		},
		{
			Name:  "atomic-ok",
			Why:   "the temp file is .<base>.tmp beside the target, it is renamed over the live path, and nothing is left behind",
			Files: []wsFile{file("a.txt", "old"), file("dir/b.txt", "b")},
			Ops: []wsOp{
				{Op: "atomic", Path: "dir/a.txt", Arg: "new"},
				read("dir/a.txt"),
				op("list", "dir"),
				{Op: "atomic", Path: "a.txt", Arg: "new2"},
				read("a.txt"),
				op("list", ""),
			},
		},
		{
			Name:         "atomic-rename-refused",
			Why:          "a rename that fails keeps the old payload, deletes the temp file, and names both paths in the refusal",
			Files:        []wsFile{file("a.txt", "old")},
			RefuseRename: true,
			Ops: []wsOp{
				{Op: "atomic", Path: "a.txt", Arg: "new"},
				read("a.txt"),
			},
		},
		{
			Name:  "copy",
			Why:   "copy wraps the read and the write separately, so the sentence says which half failed",
			Files: []wsFile{file("a.txt", "x")},
			Ops: []wsOp{
				{Op: "copy", Path: "a.txt", Arg: "b/c.txt"},
				read("b/c.txt"),
				{Op: "copy", Path: "nope.txt", Arg: "d.txt"},
			},
		},
		{
			Name:     "scope-read-deny",
			Why:      "read is deny-only: an unlisted path is readable, a matching one is refused, and the violation names the rule",
			Files:    formFiles,
			Kind:     "scope",
			DenyRead: []string{"secrets/**"},
			Ops: []wsOp{
				read("secrets/key.txt"),
				read("out/a.txt"),
				read("secrets/../secrets/key.txt"),
				op("list", "secrets"),
				op("exists", "secrets/key.txt"),
				op("stat", "secrets/key.txt"),
			},
		},
		{
			Name:       "scope-write-allow",
			Why:        "write is allow-only: an unlisted path is refused, a matching one is not, and rename checks both ends",
			Files:      formFiles,
			Kind:       "scope",
			AllowWrite: []string{"out/**"},
			Ops: []wsOp{
				{Op: "write", Path: "out/x.txt", Arg: "x"},
				{Op: "write", Path: "other/y.txt", Arg: "y"},
				{Op: "append", Path: "out/x.txt", Arg: "!"},
				op("delete", "secrets/key.txt"),
				{Op: "rename", Path: "out/x.txt", Arg: "out/z.txt"},
				{Op: "rename", Path: "out/z.txt", Arg: "other/z.txt"},
				op("remove_all", "out"),
			},
		},
		{
			Name:          "scope-mandatory-first",
			Why:           "mandatory_deny is checked before the allow list in both directions, and says so in the reason",
			Files:         []wsFile{file("secrets/a.txt", "s"), file("ok.txt", "o")},
			Kind:          "scope",
			AllowWrite:    []string{"**"},
			MandatoryDeny: []string{"secrets/**"},
			Ops: []wsOp{
				{Op: "write", Path: "secrets/a.txt", Arg: "s2"},
				{Op: "write", Path: "ok.txt", Arg: "o2"},
				read("secrets/a.txt"),
			},
		},
		{
			Name:     "scope-pattern-forms",
			Why:      "the four forms as decisions: dir/** covers the directory itself, **/dir/** is any depth, **/name is a suffix rule that also globs the base name, and a separator-free pattern only reaches the root",
			Files:    []wsFile{file("placeholder.txt", "p")},
			Kind:     "scope",
			DenyRead: []string{"dir/**", "**/cache/**", "**/*.key", "*.tmp"},
			Ops: []wsOp{
				read("dir"),
				read("dir/x"),
				read("xdir/x"),
				read("cache"),
				read("a/cache/b"),
				read("a/b/c.key"),
				read("top.key"),
				read("a/b/c.key.bak"),
				read("x.tmp"),
				read("d/x.tmp"),
			},
		},
		{
			Name:    "scope-limited",
			Why:     "a scoped view answers the bounded-read question with not-available rather than materializing the file, and the positive-cap check still comes first",
			Files:   []wsFile{file("a.txt", "0123456789")},
			Kind:    "scope",
			Limited: false,
			Ops: []wsOp{
				{Op: "capped", Path: "a.txt", Max: 10},
				{Op: "capped", Path: "a.txt", Max: 0},
				read("a.txt"),
			},
		},
		{
			Name:     "sub-prefix",
			Why:      "a prefixed view joins its prefix onto every path, and the write lands under the prefix for the parent to see",
			Files:    []wsFile{file("a/x.txt", "x"), file("a/b/y.txt", "y"), file("root.txt", "r")},
			Kind:     "sub",
			Prefixes: []string{"a"},
			Ops: []wsOp{
				read("x.txt"),
				read("b/y.txt"),
				op("list", ""),
				op("list", "b"),
				{Op: "write", Path: "new/z.txt", Arg: "z"},
				read("new/z.txt"),
			},
		},
		{
			Name:     "sub-empty-prefix",
			Why:      "an empty prefix returns the workspace itself: the root's files are reachable without a prefix",
			Files:    []wsFile{file("a/x.txt", "x"), file("root.txt", "r")},
			Kind:     "sub",
			Prefixes: []string{""},
			Ops: []wsOp{
				read("root.txt"),
				op("list", ""),
			},
		},
		{
			Name:     "sub-nested",
			Why:      "two prefixes compose, and the second is not able to climb out of the first",
			Files:    []wsFile{file("a/b/c.txt", "c"), file("a/d.txt", "d")},
			Kind:     "sub",
			Prefixes: []string{"a", "b"},
			Ops: []wsOp{
				read("c.txt"),
				op("list", ""),
				{Op: "write", Path: "d.txt", Arg: "d"},
				{Op: "write", Path: "../d.txt", Arg: "escape"},
			},
		},
		{
			Name:     "sub-traversal",
			Why:      "the prefixed view validates the path before joining it, so a climb is refused as a climb rather than as a missing file under the prefix",
			Files:    []wsFile{file("a/x.txt", "x"), file("root.txt", "r")},
			Kind:     "sub",
			Prefixes: []string{"a"},
			Ops: []wsOp{
				read("../root.txt"),
				{Op: "write", Path: "/abs", Arg: "x"},
				op("list", "..a"),
			},
		},
		{
			Name:     "sub-root",
			Why:      "removing the view's root is refused by the view, and removing a child still works",
			Files:    []wsFile{file("a/x.txt", "x"), file("a/b/y.txt", "y")},
			Kind:     "sub",
			Prefixes: []string{"a"},
			Ops: []wsOp{
				op("remove_all", ""),
				op("remove_all", "."),
				op("remove_all", "b"),
				op("exists", "x.txt"),
				op("exists", "b/y.txt"),
			},
		},
		{
			Name:     "sub-bad-prefix",
			Why:      "a prefix that climbs out makes a view that refuses everything with the construction's sentence, and that sentence survives being wrapped again",
			Files:    []wsFile{file("root.txt", "r")},
			Kind:     "sub",
			Prefixes: []string{"../escape"},
			Ops: []wsOp{
				read("x.txt"),
				op("list", ""),
				op("exists", "x.txt"),
				read("../x.txt"),
			},
		},
		{
			Name:     "sub-bad-prefix-dot-name",
			Why:      "the prefix rule is lexical, not component-wise: a name that begins with two dots is refused too",
			Files:    []wsFile{file("root.txt", "r")},
			Kind:     "sub",
			Prefixes: []string{"..a"},
			Ops:      []wsOp{op("list", "")},
		},
		{
			Name:     "sub-limited",
			Why:      "the prefixed view bounds a read only when the medium can, and the refusal carries the joined path, which is what the medium was asked for",
			Files:    []wsFile{file("a/x.txt", "0123456789")},
			Kind:     "sub",
			Limited:  true,
			Prefixes: []string{"a"},
			Ops: []wsOp{
				{Op: "capped", Path: "x.txt", Max: 10},
				{Op: "capped", Path: "x.txt", Max: 9},
				{Op: "capped", Path: "x.txt", Max: 0},
			},
		},
		{
			Name:  "walk-order",
			Why:   "directories are visited before their contents, in list order, and a skipped directory takes its subtree with it",
			Files: tree,
			Ops: []wsOp{
				op("walk", "."),
				{Op: "walk_skip", Path: ".", Arg: "dir/sub"},
				{Op: "walk", Path: "missing"},
				op("list", ""),
			},
		},
		{
			Name:  "glob-forms",
			Why:   "a star never crosses a separator, a ** segment spans zero or more of them, and only a pattern without ** can fail to compile",
			Files: tree,
			Ops: []wsOp{
				op("glob", "*.txt"),
				op("glob", "dir/*.txt"),
				op("glob", "**/*.go"),
				op("glob", "dir/**/*.go"),
				op("glob", "**/deep/*.go"),
				op("glob", "*.go"),
				op("glob", "dir/*"),
				op("glob", "**"),
				op("glob", "a["),
				op("glob", "**/a["),
			},
		},
		{
			Name:  "glob-no-doublestar-brackets",
			Why:   "a character class is one character and does not cross a separator, and the same pattern under a ** segment is matched per segment",
			Files: []wsFile{file("a1.go", "1"), file("ab.go", "2"), file("x/a2.go", "3")},
			Ops: []wsOp{
				op("glob", "[a-z][0-9].go"),
				op("glob", "*[0-9].go"),
				op("glob", "**/[a-z][0-9].go"),
			},
		},
	}
}

// localCases is the real-tree table.
func localCases() []wsLocalCase {
	file := func(path, data string) wsFile { return wsFile{Path: path, Data: data} }
	outsideFile := func(path, data string) wsFile {
		return wsFile{Path: path, Data: data, Area: "outside"}
	}
	link := func(path, target string) wsFile { return wsFile{Path: path, Symlink: target} }
	op := func(name, path string) wsOp { return wsOp{Op: name, Path: path} }
	read := func(path string) wsOp { return op("read", path) }

	return []wsLocalCase{
		{
			Name: "local-ops",
			Why:  "the backend's own answers over a real tree, and the same rules the map medium's rows state: implied parents, idempotent delete and remove_all, list of a missing directory, a directory's size left unstated, the capability report only a real backend can give",
			Files: []wsFile{
				file("a.txt", "a"),
				file("d/c.txt", "c"),
			},
			Ops: []wsOp{
				read("a.txt"),
				op("list", ""),
				op("list", "d"),
				op("list", "missing"),
				op("exists", "a.txt"),
				op("exists", "zz"),
				op("stat", "a.txt"),
				op("stat", "d"),
				op("stat", "missing"),
				{Op: "write", Path: "e/f.txt", Arg: "f"},
				{Op: "append", Path: "a.txt", Arg: "!"},
				read("a.txt"),
				{Op: "rename", Path: "a.txt", Arg: "e/g.txt"},
				read("e/g.txt"),
				op("delete", "e/g.txt"),
				op("delete", "e/g.txt"),
				op("delete", "d"),
				op("remove_all", "d"),
				op("remove_all", "d"),
				op("list", ""),
				op("walk", ""),
				op("glob", "**/*.txt"),
				op("capabilities", ""),
			},
		},
		{
			Name:  "local-refusals",
			Why:   "every operation refuses a path that climbs out, an absolute path and one that only looks like a climb, before the host is asked; removing a view's root is refused in core's words",
			Files: []wsFile{file("a.txt", "a")},
			Ops: []wsOp{
				read("../x"),
				read("/etc/passwd"),
				read("..a"),
				{Op: "write", Path: "../x", Arg: "x"},
				op("list", "../x"),
				op("exists", "../x"),
				{Op: "rename", Path: "../x", Arg: "y"},
				op("remove_all", "../x"),
				op("remove_all", ""),
				op("remove_all", "."),
			},
		},
		{
			Name: "local-symlinks",
			Why:  "what a pinned root decides: a relative link inside the tree is followed, an absolute one is refused even when it points back inside, a link that leaves is refused, a dangling one is not found, remove_all removes the link and never the target, and delete -- which stats first -- cannot even see a link that escapes",
			Files: []wsFile{
				file("a.txt", "aaa"),
				file("d/kept.txt", "k"),
				outsideFile("secret.txt", "s"),
				link("link-in", "a.txt"),
				link("link-abs-in", "abs:a.txt"),
				link("link-out", "../outside/secret.txt"),
				link("link-dir", "d"),
				link("link-dangling", "nope.txt"),
			},
			Ops: []wsOp{
				read("link-in"),
				op("stat", "link-in"),
				read("link-abs-in"),
				read("link-out"),
				read("link-dangling"),
				op("exists", "link-dangling"),
				op("exists", "link-out"),
				{Op: "write", Path: "link-dir/new.txt", Arg: "n"},
				read("d/new.txt"),
				op("list", ""),
				op("delete", "link-out"),
				op("remove_all", "link-out"),
				op("delete", "link-in"),
				op("remove_all", "link-dir"),
				read("d/new.txt"),
				read("d/kept.txt"),
				op("list", ""),
			},
		},
		{
			Name:     "local-sub",
			Why:      "the backend's own Sub creates the prefix through the pinned handle and prefixes every operation with it; the view's own root cannot be removed, and a path that climbs is still refused lexically",
			Kind:     "sub",
			Prefixes: []string{"s/t"},
			Files:    []wsFile{file("a.txt", "a")},
			Ops: []wsOp{
				{Op: "write", Path: "f.txt", Arg: "f"},
				read("f.txt"),
				op("list", ""),
				op("walk", ""),
				read("../a.txt"),
				read("../../a.txt"),
				op("remove_all", ""),
				op("remove_all", "."),
				{Op: "write", Path: "../../../x", Arg: "x"},
			},
		},
		{
			Name:     "local-sub-escape",
			Why:      "a prefix a symlink made leave the tree is refused as a traversal -- the advisory classification, in core's words, before the handle is even asked to create it",
			Kind:     "sub",
			Prefixes: []string{"link-out/deeper"},
			Files: []wsFile{
				outsideFile("secret.txt", "s"),
				link("link-out", "../outside"),
			},
		},
		{
			Name:  "local-capped",
			Why:   "the real backend answers the bounded read itself, so the oversized file is refused at the read layer with the sentence that says so, and a zero cap is refused before the host is asked",
			Files: []wsFile{file("big.txt", "0123456789")},
			Ops: []wsOp{
				{Op: "capped", Path: "big.txt", Max: 9},
				{Op: "capped", Path: "big.txt", Max: 10},
				{Op: "capped", Path: "big.txt", Max: 20},
				{Op: "capped", Path: "missing.txt", Max: 5},
				{Op: "capped", Path: "big.txt", Max: 0},
			},
		},
		{
			Name:  "local-atomic",
			Why:   "atomic publish and copy are the helpers over this backend too: the temp file appears beside the target, the rename publishes it, and nothing is left behind",
			Files: []wsFile{file("a.txt", "old")},
			Ops: []wsOp{
				{Op: "atomic", Path: "dir/a.txt", Arg: "new"},
				read("dir/a.txt"),
				op("list", "dir"),
				{Op: "copy", Path: "a.txt", Arg: "b/c.txt"},
				read("b/c.txt"),
				op("list", "b"),
				op("list", ""),
			},
		},
	}
}
