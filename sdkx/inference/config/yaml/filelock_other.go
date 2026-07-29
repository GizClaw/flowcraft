//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package yaml

import "context"

type processFileLock struct{}

func lockFile(context.Context, string) (fileLocker, error) {
	return processFileLock{}, nil
}

func (processFileLock) Unlock() error { return nil }
