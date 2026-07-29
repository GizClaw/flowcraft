//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris || windows

package yaml

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdkx/inference/config"
)

func watchDocument(t *testing.T) config.Document {
	t.Helper()
	document, err := config.DecodeJSON(strings.NewReader(`{
		"version": "v1",
		"providers": [{"id": "openai", "driver": "openai"}]
	}`))
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	return document
}

func awaitSignal(t *testing.T, signals <-chan struct{}, what string) {
	t.Helper()
	select {
	case _, ok := <-signals:
		if !ok {
			t.Fatalf("signal channel closed while waiting for %s", what)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no signal for %s within 5s", what)
	}
}

func TestStoreNotifySignalsSavesAndExternalWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inference.yaml")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := store.Save(t.Context(), "", watchDocument(t)); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	signals, err := store.Notify(t.Context())
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	t.Run("save through store", func(t *testing.T) {
		if _, err := store.Save(
			t.Context(), config.AnyRevision, watchDocument(t),
		); err != nil {
			t.Fatalf("Save: %v", err)
		}
		awaitSignal(t, signals, "store save")
	})

	t.Run("external replace", func(t *testing.T) {
		// Simulate another process editing the file out of band.
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		awaitSignal(t, signals, "external write")
	})

	t.Run("ignores unrelated files in the same directory", func(t *testing.T) {
		unrelated := filepath.Join(filepath.Dir(path), "other.txt")
		if err := os.WriteFile(unrelated, []byte("noise"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		select {
		case <-signals:
			t.Fatal("unrelated file produced a signal")
		case <-time.After(250 * time.Millisecond):
		}
	})
}

func TestStoreNotifySurvivesSaveBursts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inference.yaml")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snapshot, err := store.Save(t.Context(), "", watchDocument(t))
	if err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	signals, err := store.Notify(t.Context())
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	// A burst of saves must coalesce without wedging the signal channel: at
	// least one signal arrives and the final revision is loadable.
	for range 3 {
		snapshot, err = store.Save(
			t.Context(), snapshot.Revision, watchDocument(t),
		)
		if err != nil {
			t.Fatalf("burst Save: %v", err)
		}
	}
	awaitSignal(t, signals, "save burst")
	loaded, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Revision != snapshot.Revision {
		t.Fatalf("revision = %q, want %q", loaded.Revision, snapshot.Revision)
	}
}
