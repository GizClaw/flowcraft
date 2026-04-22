package eventlog

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/sdk/kanban"
)

// BootKanbanWithBridge is the single supported entry point for wiring a
// kanban Board into the eventlog. It enforces §2.4 steps 7 → 9 (board
// constructed, restored, then bridges attached, BEFORE scheduler.Start
// is called by the caller).
//
// Constraints:
//
//   - board must be supplied by the caller (already constructed via
//     kanban.NewBoard / kanban.New).
//   - boardCtx is the bridge subscription lifetime; the bridges exit when
//     it is cancelled.
//   - The caller MUST call scheduler.Start() AFTER this returns (step 10),
//     never before. Doing it before risks cron ticks firing into a
//     bus that no one is bridging yet.
//
// On error, any bridge that was already started is closed before
// returning so we don't leak goroutines.
func BootKanbanWithBridge(boardCtx context.Context, log *SQLiteLog, board *kanban.Board) (*KanbanBridge, *CronBridge, error) {
	if log == nil {
		return nil, nil, errors.New("boot_kanban: nil log")
	}
	if board == nil {
		return nil, nil, errors.New("boot_kanban: nil board")
	}

	kb := NewKanbanBridge(log)
	if err := kb.Attach(boardCtx, board); err != nil {
		return nil, nil, err
	}

	cb := NewCronBridge(log)
	if err := cb.Attach(boardCtx, board); err != nil {
		_ = kb.Close()
		return nil, nil, err
	}

	return kb, cb, nil
}
