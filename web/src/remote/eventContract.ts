export const REMOTE_COLLABORATION_EVENT_CONTRACT_VERSION = 1

export const REMOTE_COLLABORATION_EVENT_KINDS = [
  'chat.goal.changed',
  'chat.plan_mode.changed',
  'chat.todo.updated',
  'chat.subagent.started',
  'chat.subagent.status',
  'chat.subagent.message',
] as const

const remoteEventContractMethods = new Set([
  'event.subscribe',
  'conversation.events',
  'conversation.generation.attach',
  'conversation.send',
  'conversation.chat.send',
  'chat.send',
])

/** Adds the event kinds understood by this browser to RPCs that can stream
 * conversation events. remote/v2 was introduced after this v1 contract, so
 * every v2 Agent accepts these negotiation fields. */
export const withRemoteEventContract = (
  method: string,
  input: Record<string, unknown>,
): Record<string, unknown> => {
  if (!remoteEventContractMethods.has(method)) return input
  return {
    ...input,
    eventContractVersion: REMOTE_COLLABORATION_EVENT_CONTRACT_VERSION,
    acceptedEventKinds: [...REMOTE_COLLABORATION_EVENT_KINDS],
  }
}
