<script setup lang="ts">
import { FitAddon } from '@xterm/addon-fit'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'
import { inject, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'

import { remoteRPCKey, type RemoteRPCStreamHandle } from '@/remote/rpcTypes'

type SessionState = 'opening' | 'running' | 'reconnecting' | 'exited' | 'failed' | 'closed'

interface TerminalOpenResult {
  sessionId: string
  shell: string
  cwd: string
  rows: number
  columns: number
  firstSequence: number
  highWatermark: number
  nextInputSequence: number
  nextResizeSequence: number
  running: boolean
  exitCode: number
  exitReason: string
}

interface TerminalAttachResult {
  sessionId: string
  firstSequence: number
  highWatermark: number
  resetRequired: boolean
  running: boolean
}

interface TerminalEvent {
  type: 'output' | 'exit'
  sessionId: string
  sequence: number
  encoding?: 'base64url'
  data?: string
  exitCode?: number
  reason?: string
}

interface TerminalWriteResult {
  sessionId: string
  inputSequence: number
  acceptedBytes: number
  nextInputSequence: number
}

interface TerminalResizeResult {
  sessionId: string
  resizeSequence: number
  rows: number
  columns: number
  nextResizeSequence: number
}

const props = defineProps<{ projectId: string; active: boolean }>()
const emit = defineEmits<{
  state: [value: { state: SessionState; message: string }]
}>()

const rpc = inject(remoteRPCKey)
if (!rpc) throw new Error('Remote RPC provider is required')

const host = ref<HTMLElement | null>(null)
const state = ref<SessionState>('opening')
const stateMessage = ref('正在打开远程 PTY…')

let terminal: Terminal | undefined
let fitAddon: FitAddon | undefined
let resizeObserver: ResizeObserver | undefined
let attachHandle: RemoteRPCStreamHandle<TerminalAttachResult> | undefined
let sessionId = ''
let lastSequence = 0
let inputSequence = 1
let resizeSequence = 1
let disposed = false
let sessionRunning = false
let attachGeneration = 0
let inputBuffer = ''
let inputFlushTimer: ReturnType<typeof setTimeout> | undefined
let resizeFlushTimer: ReturnType<typeof setTimeout> | undefined
let inputQueue = Promise.resolve()
let resizeQueue = Promise.resolve()

const setState = (next: SessionState, message: string) => {
  state.value = next
  stateMessage.value = message
  emit('state', { state: next, message })
}

const isSafeSequence = (value: unknown, minimum = 0): value is number =>
  Number.isSafeInteger(value) && (value as number) >= minimum

const uuid = () => {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (value) => {
    const random = (Math.random() * 16) | 0
    return (value === 'x' ? random : (random & 0x3) | 0x8).toString(16)
  })
}

const encodeBase64Url = (bytes: Uint8Array) => {
  let binary = ''
  for (let offset = 0; offset < bytes.length; offset += 0x4000) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x4000))
  }
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '')
}

const decodeBase64Url = (value: string) => {
  if (!/^[A-Za-z0-9_-]*$/.test(value)) throw new Error('设备返回了无效的终端输出。')
  const padding = (4 - (value.length % 4)) % 4
  const binary = atob(value.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat(padding))
  return Uint8Array.from(binary, (character) => character.charCodeAt(0))
}

const dimensions = () => ({
  rows: Math.min(500, Math.max(2, terminal?.rows || 24)),
  columns: Math.min(1000, Math.max(10, terminal?.cols || 80)),
})

const fit = () => {
  if (!host.value || !fitAddon || !terminal || host.value.clientWidth === 0) return
  try {
    fitAddon.fit()
  } catch {
    // A hidden retained terminal can briefly report a zero-sized viewport.
  }
}

const writeExit = (code: number, reason: string) => {
  terminal?.write(`\r\n\x1b[90m[远程进程已退出：${code}${reason ? ` · ${reason}` : ''}]\x1b[0m\r\n`)
}

const fail = (error: unknown, fallback = '远程终端连接失败。') => {
  sessionRunning = false
  attachGeneration += 1
  const current = attachHandle
  attachHandle = undefined
  void current?.detach().catch(() => undefined)
  const failedSession = sessionId
  sessionId = ''
  if (failedSession) {
    void rpc
      .call('terminal.close', { sessionId: failedSession }, { projectId: props.projectId })
      .catch(() => undefined)
  }
  const message = error instanceof Error && error.message ? error.message : fallback
  setState('failed', message)
}

const handleEvent = (event: TerminalEvent) => {
  if (
    !sessionId ||
    event.sessionId !== sessionId ||
    !isSafeSequence(event.sequence, 1) ||
    event.sequence !== lastSequence + 1
  ) {
    throw new Error('终端输出序列不连续，请重新连接。')
  }
  lastSequence = event.sequence
  if (event.type === 'output') {
    if (event.encoding !== 'base64url' || typeof event.data !== 'string') {
      throw new Error('设备返回了无效的终端输出。')
    }
    terminal?.write(decodeBase64Url(event.data))
    return
  }
  if (event.type !== 'exit' || !Number.isInteger(event.exitCode)) {
    throw new Error('设备返回了无效的终端事件。')
  }
  sessionRunning = false
  const reason = typeof event.reason === 'string' ? event.reason : ''
  writeExit(event.exitCode!, reason)
  setState('exited', `远程进程已退出（${event.exitCode}）。`)
}

const attachLoop = async (generation: number) => {
  let failures = 0
  while (!disposed && sessionRunning && generation === attachGeneration) {
    try {
      const current = rpc.startStream<TerminalEvent, TerminalAttachResult>(
        'terminal.attach',
        { sessionId, lastSequence, waitSeconds: 25 },
        handleEvent,
        { projectId: props.projectId },
      )
      attachHandle = current
      const result = await current.result
      if (attachHandle === current) attachHandle = undefined
      if (disposed || generation !== attachGeneration || !sessionRunning) return
      if (
        result.sessionId !== sessionId ||
        !isSafeSequence(result.firstSequence, 1) ||
        !isSafeSequence(result.highWatermark) ||
        result.highWatermark < lastSequence
      ) {
        throw new Error('设备返回了无效的终端游标。')
      }
      if (result.resetRequired || result.firstSequence > lastSequence + 1) {
        throw new Error('终端回放窗口已过期，请重新打开此终端。')
      }
      if (result.highWatermark !== lastSequence) {
        throw new Error('设备未完整发送终端输出，请重新连接。')
      }
      failures = 0
      if (!result.running) {
        sessionRunning = false
        setState('exited', '远程进程已结束。')
        return
      }
      if (state.value === 'reconnecting') setState('running', '远程 PTY 已恢复。')
    } catch (error) {
      if (disposed || generation !== attachGeneration || !sessionRunning) return
      failures += 1
      if (failures >= 5) {
        fail(error, '终端输出通道连续恢复失败。')
        return
      }
      setState('reconnecting', '终端输出通道中断，正在恢复…')
      await new Promise((resolve) => setTimeout(resolve, Math.min(5000, failures * 750)))
    }
  }
}

const flushInput = () => {
  if (inputFlushTimer) clearTimeout(inputFlushTimer)
  inputFlushTimer = undefined
  const data = inputBuffer
  inputBuffer = ''
  if (!data || !sessionRunning || !sessionId) return
  const encoded = new TextEncoder().encode(data)
  for (let offset = 0; offset < encoded.length; offset += 32 * 1024) {
    const chunk = encoded.slice(offset, offset + 32 * 1024)
    inputQueue = inputQueue
      .then(async () => {
        if (!sessionRunning || disposed) return
        const sequence = inputSequence
        const result = await rpc.call<TerminalWriteResult>(
          'terminal.write',
          {
            sessionId,
            inputSequence: sequence,
            encoding: 'base64url',
            data: encodeBase64Url(chunk),
          },
          { projectId: props.projectId },
        )
        if (
          result.sessionId !== sessionId ||
          result.inputSequence !== sequence ||
          result.acceptedBytes !== chunk.length ||
          result.nextInputSequence !== sequence + 1
        ) {
          throw new Error('设备未确认终端输入。')
        }
        inputSequence = result.nextInputSequence
      })
      .catch((error) => fail(error, '无法向远程终端发送输入。'))
  }
}

const queueInput = (data: string) => {
  if (!sessionRunning || disposed) return
  inputBuffer += data
  if (new TextEncoder().encode(inputBuffer).length >= 4096) {
    flushInput()
    return
  }
  if (!inputFlushTimer) inputFlushTimer = setTimeout(flushInput, 6)
}

const flushResize = () => {
  if (resizeFlushTimer) clearTimeout(resizeFlushTimer)
  resizeFlushTimer = undefined
  if (!sessionRunning || !sessionId) return
  const { rows, columns } = dimensions()
  resizeQueue = resizeQueue
    .then(async () => {
      if (!sessionRunning || disposed) return
      const sequence = resizeSequence
      const result = await rpc.call<TerminalResizeResult>(
        'terminal.resize',
        { sessionId, resizeSequence: sequence, rows, columns },
        { projectId: props.projectId },
      )
      if (
        result.sessionId !== sessionId ||
        result.resizeSequence !== sequence ||
        result.rows !== rows ||
        result.columns !== columns ||
        result.nextResizeSequence !== sequence + 1
      ) {
        throw new Error('设备未确认终端尺寸。')
      }
      resizeSequence = result.nextResizeSequence
    })
    .catch((error) => fail(error, '无法调整远程终端尺寸。'))
}

const scheduleResize = () => {
  fit()
  if (resizeFlushTimer) clearTimeout(resizeFlushTimer)
  resizeFlushTimer = setTimeout(flushResize, 75)
}

const start = async () => {
  if (disposed) return
  attachGeneration += 1
  const generation = attachGeneration
  sessionRunning = false
  sessionId = ''
  lastSequence = 0
  inputSequence = 1
  resizeSequence = 1
  terminal?.reset()
  setState('opening', '正在打开远程 PTY…')
  try {
    await rpc.connect()
    await nextTick()
    fit()
    const { rows, columns } = dimensions()
    const opened = await rpc.call<TerminalOpenResult>(
      'terminal.open',
      { clientRequestId: uuid(), cwd: '', rows, columns },
      { projectId: props.projectId },
    )
    if (disposed || generation !== attachGeneration) return
    if (
      !opened.sessionId ||
      !isSafeSequence(opened.highWatermark) ||
      !isSafeSequence(opened.nextInputSequence, 1) ||
      !isSafeSequence(opened.nextResizeSequence, 1)
    ) {
      throw new Error('设备返回了无效的终端会话。')
    }
    sessionId = opened.sessionId
    lastSequence = opened.highWatermark
    inputSequence = opened.nextInputSequence
    resizeSequence = opened.nextResizeSequence
    if (!opened.running) {
      writeExit(opened.exitCode, opened.exitReason)
      setState('exited', `远程进程已退出（${opened.exitCode}）。`)
      return
    }
    sessionRunning = true
    setState('running', `${opened.shell || 'shell'} · ${opened.cwd || '项目根目录'}`)
    void attachLoop(generation)
    if (props.active) terminal?.focus()
  } catch (error) {
    if (!disposed && generation === attachGeneration) fail(error)
  }
}

const closeRemote = async () => {
  sessionRunning = false
  attachGeneration += 1
  const current = attachHandle
  attachHandle = undefined
  await current?.detach().catch(() => undefined)
  const closingSession = sessionId
  sessionId = ''
  if (closingSession) {
    await rpc
      .call('terminal.close', { sessionId: closingSession }, { projectId: props.projectId })
      .catch(() => undefined)
  }
}

const restart = async () => {
  await closeRemote()
  await start()
}

defineExpose({ restart, focus: () => terminal?.focus() })

watch(
  () => props.active,
  (active) => {
    if (!active) return
    void nextTick(() => {
      fit()
      terminal?.focus()
    })
  },
)

onMounted(() => {
  terminal = new Terminal({
    allowProposedApi: false,
    convertEol: false,
    cursorBlink: true,
    cursorStyle: 'bar',
    fontFamily: '"Cascadia Mono", "SFMono-Regular", Consolas, monospace',
    fontSize: 13,
    lineHeight: 1.25,
    scrollback: 10_000,
    theme: {
      background: '#101828',
      foreground: '#f2f4f7',
      cursor: '#7ee2bd',
      selectionBackground: '#344054',
      black: '#101828',
      red: '#f97066',
      green: '#6ce9a6',
      yellow: '#fec84b',
      blue: '#84adff',
      magenta: '#c7a7ff',
      cyan: '#67e3f9',
      white: '#f2f4f7',
    },
  })
  fitAddon = new FitAddon()
  terminal.loadAddon(fitAddon)
  terminal.open(host.value!)
  terminal.onData(queueInput)
  resizeObserver = new ResizeObserver(scheduleResize)
  resizeObserver.observe(host.value!)
  void start()
})

onBeforeUnmount(() => {
  disposed = true
  if (inputFlushTimer) clearTimeout(inputFlushTimer)
  if (resizeFlushTimer) clearTimeout(resizeFlushTimer)
  resizeObserver?.disconnect()
  void closeRemote()
  terminal?.dispose()
  setState('closed', '终端已关闭。')
})
</script>

<template>
  <div class="terminal-session" :class="state">
    <div v-if="state !== 'running'" class="terminal-session-notice" role="status">
      <span>{{ stateMessage }}</span>
      <button v-if="state === 'failed' || state === 'exited'" type="button" @click="restart">
        重新连接
      </button>
    </div>
    <div ref="host" class="terminal-host" :aria-hidden="!active"></div>
  </div>
</template>

<style scoped>
.terminal-session {
  position: relative;
  min-height: 0;
  height: 100%;
  border: 1px solid #344054;
  border-radius: 12px;
  overflow: hidden;
  background: #101828;
}
.terminal-host {
  box-sizing: border-box;
  width: 100%;
  height: 100%;
  padding: 12px;
}
.terminal-host :deep(.xterm) {
  height: 100%;
}
.terminal-session-notice {
  position: absolute;
  z-index: 2;
  top: 10px;
  right: 10px;
  left: 10px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  border: 1px solid #475467;
  border-radius: 9px;
  padding: 8px 10px;
  color: #d0d5dd;
  background: rgb(16 24 40 / 94%);
  font-size: 0.76rem;
}
.terminal-session-notice button {
  border-color: #667085;
  color: #f2f4f7;
  background: #1d2939;
}
.terminal-session.failed .terminal-session-notice {
  border-color: #f97066;
  color: #fecdca;
}
</style>
