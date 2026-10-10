package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCappedPrefersLimitedReader(t *testing.T) {
	ws, ctx := newLocalWS(t)
	if err := ws.Write(ctx, "data.txt", []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	// Over the cap: the bounded read rejects before the payload is
	// materialized, so an error comes back instead of data.
	if data, err := Capped(ctx, ws, "data.txt", 4); err == nil {
		t.Fatalf("over-cap read returned %q, want an error", data)
	}
	got, err := Capped(ctx, ws, "data.txt", 10)
	if err != nil {
		t.Fatalf("read at exact cap: %v", err)
	}
	if string(got) != "0123456789" {
		t.Fatalf("got %q", got)
	}
}

// plainWorkspace embeds the interface value, so its method set is
// exactly Workspace's: the bounded-read interface is invisible and the
// fallback path (full read, then check) is what Capped sees.
type plainWorkspace struct{ Workspace }

func TestCappedFallbackReadsThenRejects(t *testing.T) {
	ws, ctx := newLocalWS(t)
	if err := ws.Write(ctx, "data.txt", []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	plain := plainWorkspace{ws}
	if data, err := Capped(ctx, plain, "data.txt", 4); err == nil {
		t.Fatalf("over-cap fallback read returned %q, want an error", data)
	} else if !strings.Contains(err.Error(), "read cap") {
		t.Fatalf("error = %v, want the read-cap message", err)
	}
	got, err := Capped(ctx, plain, "data.txt", 10)
	if err != nil {
		t.Fatalf("fallback read under cap: %v", err)
	}
	if string(got) != "0123456789" {
		t.Fatalf("got %q", got)
	}
}

// overReturning claims the bounded-read interface and then ignores the
// cap; Capped must still refuse to hand the oversized payload on.
type overReturning struct {
	Workspace
	payload []byte
}

func (o overReturning) ReadLimited(context.Context, string, int64) ([]byte, error) {
	return o.payload, nil
}

func TestCappedRejectsBackendThatIgnoresTheCap(t *testing.T) {
	ws, ctx := newLocalWS(t)
	ignoring := overReturning{Workspace: ws, payload: []byte("0123456789")}
	if data, err := Capped(ctx, ignoring, "data.txt", 4); err == nil {
		t.Fatalf("cap-ignoring backend returned %q, want a rejection", data)
	}
}

func TestCappedRejectsNonPositiveCap(t *testing.T) {
	ws, ctx := newLocalWS(t)
	if err := ws.Write(ctx, "data.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, max := range []int64{0, -1} {
		if _, err := Capped(ctx, ws, "data.txt", max); err == nil {
			t.Fatalf("cap %d: want a validation error", max)
		}
	}
}

func TestCappedMissingFileKeepsBackendError(t *testing.T) {
	ws, ctx := newLocalWS(t)
	if _, err := Capped(ctx, ws, "nope.txt", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file error = %v, want ErrNotFound", err)
	}
	if _, err := Capped(ctx, plainWorkspace{ws}, "nope.txt", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file fallback error = %v, want ErrNotFound", err)
	}
}
