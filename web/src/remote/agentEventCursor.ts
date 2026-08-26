const cursorPrefix = 'wenzwork:agent-event-cursor:v1:'

const storage = () => {
  try {
    return globalThis.localStorage
  } catch {
    return undefined
  }
}

export const agentEventCursorKey = (deviceId: string, projectId: string) =>
  `${cursorPrefix}${deviceId}:${projectId}`

export const readAgentEventCursor = (deviceId: string, projectId: string) => {
  try {
    const raw = storage()?.getItem(agentEventCursorKey(deviceId, projectId))
    if (!raw) return undefined
    const value = JSON.parse(raw) as { sequence?: unknown }
    return typeof value.sequence === 'number' &&
      Number.isSafeInteger(value.sequence) &&
      value.sequence >= 0
      ? value.sequence
      : undefined
  } catch {
    return undefined
  }
}

export const storeAgentEventCursor = (deviceId: string, projectId: string, sequence: number) => {
  try {
    storage()?.setItem(
      agentEventCursorKey(deviceId, projectId),
      JSON.stringify({ sequence, storedAt: Date.now() }),
    )
  } catch {
    // Persistence is an optimization. The next connection safely resets when
    // browser storage is unavailable or the user explicitly clears it.
  }
}

export const removeAgentEventCursor = (deviceId: string, projectId: string) => {
  try {
    storage()?.removeItem(agentEventCursorKey(deviceId, projectId))
  } catch {
    // Best effort only; browser privacy modes may reject storage writes.
  }
}

// Cursor values are not sensitive content, but they must not survive account
// logout, controller revocation, or a device-access revocation. Clearing the
// small prefix is safe even if a browser previously used another account.
export const clearStoredAgentEventCursors = () => {
  try {
    const target = storage()
    if (!target) return
    const keys: string[] = []
    for (let index = 0; index < target.length; index += 1) {
      const key = target.key(index)
      if (key?.startsWith(cursorPrefix)) keys.push(key)
    }
    for (const key of keys) target.removeItem(key)
  } catch {
    // Browser storage can be disabled independently of the remote session.
  }
}
