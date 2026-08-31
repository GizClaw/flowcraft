//go:build windows

package workspace

import (
	"errors"
	"time"

	xwin "golang.org/x/sys/windows"
)

// removeWithRetry retries remove-style operations that fail with a
// sharing or lock violation. Windows refuses to delete a file another
// process has open (Explorer, an editor, an antivirus scan), while
// POSIX unlink succeeds regardless. The violation is transient, so a
// short bounded retry turns a race into a success; a file that stays
// locked keeps failing honestly instead of hanging.
func removeWithRetry(op func() error) error {
	const maxAttempts = 3
	delay := 200 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		if !isSharingViolation(err) || attempt >= maxAttempts {
			return err
		}
		time.Sleep(delay)
		delay *= 2
	}
}

// isSharingViolation reports whether err is a Windows sharing or lock
// violation (ERROR_SHARING_VIOLATION / ERROR_LOCK_VIOLATION), the
// errors Remove and RemoveAll surface while another process holds the
// target open.
func isSharingViolation(err error) bool {
	return errors.Is(err, xwin.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, xwin.ERROR_LOCK_VIOLATION)
}
