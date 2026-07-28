//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package yaml

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

type unixFileLock struct {
	file *os.File
}

func lockFile(ctx context.Context, path string) (fileLocker, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open YAML inference config lock: %w", err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = syscall.Flock(
			int(file.Fd()),
			syscall.LOCK_EX|syscall.LOCK_NB,
		)
		if err == nil {
			return &unixFileLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) &&
			!errors.Is(err, syscall.EAGAIN) {
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

func (l *unixFileLock) Unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
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
