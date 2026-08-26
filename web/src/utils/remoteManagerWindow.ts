export const REMOTE_MANAGER_WINDOW_NAME = 'wenzwork-remote-manager'

const remoteManagerWindowFeatures = () => {
  const availableWidth = Math.max(window.screen.availWidth || 1280, 960)
  const availableHeight = Math.max(window.screen.availHeight || 800, 640)
  const width = Math.min(1440, Math.max(960, availableWidth - 80))
  const height = Math.min(960, Math.max(640, availableHeight - 80))
  const positionedScreen = window.screen as Screen & { availLeft?: number; availTop?: number }
  const left = Math.max(
    positionedScreen.availLeft ?? 0,
    Math.round((positionedScreen.availLeft ?? 0) + (availableWidth - width) / 2),
  )
  const top = Math.max(
    positionedScreen.availTop ?? 0,
    Math.round((positionedScreen.availTop ?? 0) + (availableHeight - height) / 2),
  )
  return [
    'popup=yes',
    'toolbar=no',
    'menubar=no',
    'location=no',
    'status=no',
    `width=${width}`,
    `height=${height}`,
    `left=${left}`,
    `top=${top}`,
    'resizable=yes',
    'scrollbars=yes',
  ].join(',')
}

/**
 * A stable browsing-context name makes Chrome reuse the same app-like popup
 * instead of creating another device manager on every click.
 */
export const openRemoteManagerWindow = (url: string) => {
  if (typeof window === 'undefined') return null
  const remoteWindow = window.open(url, REMOTE_MANAGER_WINDOW_NAME, remoteManagerWindowFeatures())
  remoteWindow?.focus()
  return remoteWindow
}

export const claimRemoteManagerWindowName = () => {
  if (typeof window !== 'undefined') window.name = REMOTE_MANAGER_WINDOW_NAME
}
