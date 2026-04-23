package bootstrap

import (
	"fmt"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/projection/chat"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// wireChatProjectors registers the chat-domain projectors:
//   - ChatProjector materializes the chat read model and uses RestoreSnapshot.
//   - ChatAutoAckProjector emits auto-ack events; it depends on ChatProjector
//     so the read model is consistent before auto-ack starts firing.
func wireChatProjectors(c *ProjectorComponents, mgr *projection.Manager, log eventlog.Log, snapshots projection.SnapshotStore) error {
	c.Chat = chat.NewChatProjector(log)
	if err := mgr.RegisterProjector(c.Chat, nil, projection.WithSnapshotStore(snapshots)); err != nil {
		return fmt.Errorf("register chat projector: %w", err)
	}
	c.ChatAutoAck = chat.NewChatAutoAckProjector(log, c.Chat)
	if err := mgr.RegisterProjector(c.ChatAutoAck, []string{c.Chat.Name()}); err != nil {
		return fmt.Errorf("register chat_auto_ack projector: %w", err)
	}
	return nil
}
