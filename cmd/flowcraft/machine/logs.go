package machine

import (
	"errors"
	"io"
	"os"
)

// WriteLogFile streams the file at path to w. When tail > 0, only the
// last `tail` lines are written. The whole file is loaded in memory —
// fine for the rotated server.log (lumberjack default 100MB cap), and
// keeps the implementation portable across host and virtio-fs guest
// mounts where streaming + seek semantics are not always reliable.
//
// notFoundMsg is used as the user-facing error message when the file is
// missing, so callers can phrase it per-source ("no server log file
// yet" vs. "no crash log on this platform").
func WriteLogFile(w io.Writer, path string, tail int, notFoundMsg string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New(notFoundMsg)
		}
		return err
	}
	if tail > 0 {
		data = lastLines(data, tail)
	}
	_, err = w.Write(data)
	return err
}

// lastLines returns the suffix of data containing the last n newline-
// terminated lines. A trailing partial line (no final newline) counts
// as one. When data has fewer than n lines, the entire input is
// returned.
func lastLines(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return data
	}
	end := len(data)
	if data[end-1] == '\n' {
		end--
	}
	count := 0
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == n {
				return data[i+1:]
			}
		}
	}
	return data
}
