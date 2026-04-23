package eventlog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/sdk/kanban"
)

func TestBootKanbanWithBridge_RejectsNilArgs(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-boot")
	t.Cleanup(board.Close)

	if _, _, _, err := eventlog.BootKanbanWithBridge(context.Background(), nil, board); err == nil {
		t.Fatal("nil log accepted")
	}
	if _, _, _, err := eventlog.BootKanbanWithBridge(context.Background(), log, nil); err == nil {
		t.Fatal("nil board accepted")
	}
}

func TestBootKanbanWithBridge_AttachesAll(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-boot-2")
	t.Cleanup(board.Close)

	kb, cb, ab, err := eventlog.BootKanbanWithBridge(context.Background(), log, board)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() {
		_ = ab.Close()
		_ = cb.Close()
		_ = kb.Close()
	})

	// Re-attaching any bridge should fail (Attach is non-idempotent on
	// purpose so callers can't accidentally double-publish).
	if err := kb.Attach(context.Background(), board); err == nil {
		t.Fatal("KanbanBridge.Attach: second call should fail")
	}
	if err := cb.Attach(context.Background(), board); err == nil {
		t.Fatal("CronBridge.Attach: second call should fail")
	}
	if err := ab.Attach(context.Background(), board); err == nil {
		t.Fatal("AgentStreamBridge.Attach: second call should fail")
	}
}

func TestBootKanbanWithBridge_CloseBeforeAttachOK(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-boot-3")
	t.Cleanup(board.Close)

	bare := eventlog.NewKanbanBridge(log)
	if err := bare.Close(); err != nil {
		t.Fatalf("close before attach: %v", err)
	}
	bareCron := eventlog.NewCronBridge(log)
	if err := bareCron.Close(); err != nil {
		t.Fatalf("close before attach: %v", err)
	}
}

func TestBootKanbanWithBridge_CloseStopsGoroutines(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-boot-4")

	kb, cb, ab, err := eventlog.BootKanbanWithBridge(context.Background(), log, board)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := ab.Close(); err != nil {
		t.Fatalf("ab close: %v", err)
	}
	if err := kb.Close(); err != nil {
		t.Fatalf("kb close: %v", err)
	}
	if err := cb.Close(); err != nil {
		t.Fatalf("cb close: %v", err)
	}
	board.Close()

	// Calling Close twice is safe.
	if err := kb.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("second kb close: %v", err)
	}
}
