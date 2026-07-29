//go:build windows

package yaml

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

type windowsFileLock struct {
	file *os.File
}

func lockFile(ctx context.Context, path string) (fileLocker, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open YAML inference config lock: %w", err)
	}
	handle := windows.Handle(file.Fd())
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		// A zero-valued Overlapped locks byte range [0,1) of the lock file;
		// the file is opened for synchronous I/O so no event is required.
		err = windows.LockFileEx(
			handle,
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			1,
			0,
			&windows.Overlapped{},
		)
		if err == nil {
			return &windowsFileLock{file: file}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, fmt.Errorf(
				"lock YAML inference config: %w",
				err,
			)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *windowsFileLock) Unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := windows.UnlockFileEx(
		windows.Handle(l.file.Fd()),
		0,
		1,
		0,
		&windows.Overlapped{},
	)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf(
			"unlock YAML inference config: %w",
			unlockErr,
		)
	}
	if closeErr != nil {
		return fmt.Errorf(
			"close YAML inference config lock: %w",
			closeErr,
		)
	}
	return nil
}
