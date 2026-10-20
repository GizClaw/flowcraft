package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
// Deliberately not rows: anything that needs a real filesystem (symlink escape,
// a directory handle, permission bits) -- that is the local backend's slice, and
// the two implementations' containment is only comparable once the port has one.

// wsContract is the committed document.
type wsContract struct {
	// Note says what the table is for.
	Note []string `json:"note"`
	// Masked names what a reader must not expect to compare. Nothing is masked:
	// the medium is a map, so no path in this file depends on the run.
	Masked []string `json:"masked"`
	// Cases are the fixtures and their answers.
	Cases []wsCase `json:"cases"`
}

// wsFile is one entry the harness writes into the medium.
type wsFile struct {
	// Path is where it is written, relative to the view.
	Path string `json:"path"`
	// Data is the file's bytes.
	Data string `json:"data"`
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
func runOp(ws workspace.Workspace, logger *recordingLogger, in wsOp) wsOp {
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
			out.Data = fmt.Sprintf("%s:%d:%t", info.Name(), info.Size(), info.IsDir())
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
	return out
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
		testCase.Ops[i] = runOp(ws, logger, in)
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
