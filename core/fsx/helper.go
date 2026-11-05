package fsx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// helperArgv marks a re-executed process as the filesystem helper. The host's
// main calls MaybeRunHelper, which recognises it and never returns.
const helperArgv = "--flowcraft-fs-helper"

// helperRequest is one filesystem operation. Reads are absent on purpose: they
// never leave this process.
type helperRequest struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	Data []byte `json:"data,omitempty"`
}

// helperResponse is the helper's whole reply. Denied distinguishes "the kernel
// said no" from "the operation failed", because only the former is a policy
// refusal the caller should surface as ErrDenied.
type helperResponse struct {
	Error  string `json:"error,omitempty"`
	Denied bool   `json:"denied,omitempty"`
}

func (r helperResponse) err() error {
	if r.Error == "" {
		return nil
	}
	if r.Denied {
		return fmt.Errorf("%w: %s", ErrDenied, r.Error)
	}
	return errors.New(r.Error)
}

func encodeRequest(req helperRequest) ([]byte, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("fsx: encode helper request: %w", err)
	}
	return payload, nil
}

func decodeResponse(payload []byte) (helperResponse, error) {
	var resp helperResponse
	if err := json.Unmarshal(bytes.TrimSpace(payload), &resp); err != nil {
		return resp, fmt.Errorf("fsx: decode helper response (%d bytes, %q): %w",
			len(payload), snippet(payload), err)
	}
	return resp, nil
}

// snippet keeps protocol errors debuggable without dumping a whole payload.
func snippet(payload []byte) string {
	const max = 200
	if len(payload) > max {
		return string(payload[:max]) + "..."
	}
	return string(payload)
}

// MaybeRunHelper turns the current process into the filesystem helper when it
// was started that way, and returns immediately otherwise.
//
// The host must call it at the top of main. Functionality is deliberately not
// installed through init: taking over the process is the host's decision to
// make explicitly, and a host that forgets gets a clear error on its first
// write rather than a silently unconfined one.
func MaybeRunHelper() {
	if len(os.Args) != 2 || os.Args[1] != helperArgv {
		return
	}
	os.Exit(helperMain(os.Stdin, os.Stdout))
}

// helperMain performs one request and reports the exit status.
func helperMain(stdin io.Reader, stdout io.Writer) int {
	payload, err := io.ReadAll(stdin)
	if err != nil {
		writeResponse(stdout, helperResponse{Error: err.Error()})
		return 1
	}
	var req helperRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		writeResponse(stdout, helperResponse{Error: err.Error()})
		return 1
	}
	resp := runHelperRequest(req)
	writeResponse(stdout, resp)
	if resp.Error != "" {
		return 1
	}
	return 0
}

func writeResponse(w io.Writer, resp helperResponse) {
	payload, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = w.Write(payload)
}

// runHelperRequest performs one operation with no policy check of its own: the
// sandbox it runs in is the boundary.
func runHelperRequest(req helperRequest) helperResponse {
	var err error
	switch req.Op {
	case "write":
		err = writeFile(req.Path, req.Data)
	case "mkdir":
		err = os.MkdirAll(req.Path, 0o755)
	case "remove":
		err = os.RemoveAll(req.Path)
	default:
		err = fmt.Errorf("fsx: unknown helper op %q", req.Op)
	}
	if err == nil {
		return helperResponse{}
	}
	return helperResponse{Error: err.Error(), Denied: isDenied(err)}
}

// writeFile creates missing parents and writes the whole payload.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// isDenied reports whether the kernel refused the operation rather than the
// operation failing. EROFS covers a read-only bind mount, EPERM/EACCES cover a
// seatbelt rule and an ACL.
func isDenied(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EROFS, syscall.EPERM, syscall.EACCES:
			return true
		}
	}
	return false
}

func executable() (string, error) { return os.Executable() }

func isAbs(path string) bool { return filepath.IsAbs(path) }
