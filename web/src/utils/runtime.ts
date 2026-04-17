export const OWNER_RUNTIME_ID = 'owner';

export function getRuntimeConversationId(agentId: string): string {
  return `${OWNER_RUNTIME_ID}--${agentId}`;
}
