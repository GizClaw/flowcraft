package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Workspace layout roots. A fresh namespace deliberately avoids the legacy
// per-store layouts; old workspace data is not migrated.
const (
	logRoot = "storage/v1/log"
	kvRoot  = "storage/v1/kv"
)

func nilWorkspace(ws workspace.Workspace) bool {
	if ws == nil {
		return true
	}
	value := reflect.ValueOf(ws)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func marshalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func decodeJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("storage: unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func isNotFound(err error) bool {
	return err != nil && errdefs.IsNotFound(err)
}

// writeImmutableJSON publishes data only when path does not exist. Equal
// existing content is accepted (idempotent retry); different content returns
// ErrConflict. The check-and-write is atomic only within one process; the
// workspace adapter is single-writer.
func writeImmutableJSON(ctx context.Context, ws workspace.Workspace, path string, value any) error {
	data, err := marshalJSON(value)
	if err != nil {
		return err
	}
	existing, err := ws.Read(ctx, path)
	if err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return ErrConflict
	}
	if !isNotFound(err) {
		return err
	}
	return workspace.AtomicWrite(ctx, ws, path, data)
}

func readJSON(ctx context.Context, ws workspace.Workspace, path string, destination any) (bool, error) {
	data, err := ws.Read(ctx, path)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if err := decodeJSON(data, destination); err != nil {
		return false, err
	}
	return true, nil
}

func deleteIfExists(ctx context.Context, ws workspace.Workspace, path string) error {
	if err := ws.Delete(ctx, path); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}
