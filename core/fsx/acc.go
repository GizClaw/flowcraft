package fsx

import (
	"io/fs"
	"os"
	"path/filepath"
)

// acc is where one operation lands: a handle and the name to use with it, or —
// when that side carries no roots — the absolute path the os package takes
// directly.
type acc struct {
	// root is the handle to go through, or nil for an unmediated side.
	root *os.Root
	// base is root's canonical path; empty when root is nil.
	base string
	// name is relative to base, or absolute when root is nil.
	name string
}

// This file is the only place that knows there are two ways to reach the
// filesystem: through a handle (a restricted side) and directly through the os
// package (an unrestricted side). Keeping the branch here means every operation
// above it is written once.

func (a acc) readFile() ([]byte, error) {
	if a.root != nil {
		return a.root.ReadFile(a.name)
	}
	return os.ReadFile(a.name)
}

func (a acc) open() (*os.File, error) {
	if a.root != nil {
		return a.root.Open(a.name)
	}
	return os.Open(a.name)
}

func (a acc) stat() (fs.FileInfo, error) {
	if a.root != nil {
		return a.root.Stat(a.name)
	}
	return os.Stat(a.name)
}

func (a acc) readDir() ([]fs.DirEntry, error) {
	if a.root != nil {
		return fs.ReadDir(a.root.FS(), a.fsName())
	}
	return os.ReadDir(a.name)
}

func (a acc) create() (*os.File, error) {
	const flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if a.root != nil {
		return a.root.OpenFile(a.name, flag, 0o644)
	}
	return os.OpenFile(a.name, flag, 0o644)
}

func (a acc) mkdirAll() error {
	if a.root != nil {
		return a.root.MkdirAll(a.name, 0o755)
	}
	return os.MkdirAll(a.name, 0o755)
}

func (a acc) removeAll() error {
	if a.root != nil {
		return a.root.RemoveAll(a.name)
	}
	return os.RemoveAll(a.name)
}

// parent returns the directory that must exist before the target can be
// created, and whether there is one to create at all. A target directly in its
// root needs nothing: the handle is already open.
func (a acc) parent() (acc, bool) {
	dir := filepath.Dir(a.name)
	if dir == "." {
		return acc{}, false
	}
	return acc{root: a.root, base: a.base, name: dir}, true
}

// abs is the target as an absolute path, for messages and for the unmediated
// side.
func (a acc) abs() string {
	if a.root == nil || a.name == "." {
		if a.root == nil {
			return a.name
		}
		return a.base
	}
	return filepath.Join(a.base, a.name)
}

// fsName is the target in the slash-separated form an io/fs filesystem expects.
func (a acc) fsName() string { return filepath.ToSlash(a.name) }
