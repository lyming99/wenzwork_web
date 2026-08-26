import type { InjectionKey, Ref } from 'vue'

import type { RemoteAgentCapabilities, RemotePeerRpcEvent } from './peerClient'

export interface RemoteRPCPage<T> {
  items: T[]
  nextCursor: string | null
  highWatermark: number
  resetRequired?: boolean
}

export interface RemoteAIConfiguration {
  id: string
  revision: number
  name: string
  provider: 'openai' | 'anthropic' | 'google' | 'deepseek' | 'ollama' | 'openai-compatible'
  baseUrl: string
  nonSecretHeaders: Record<string, string>
  model: string
  systemPrompt: string
  temperature: number
  reasoningEffort: string
  maxTurnOutputTokens: number
  maxActiveContextTokens: number
  maxAgentRounds: number
  maxAgentToolCalls: number
  maxAgentNoProgressRounds: number
  requestTimeoutSeconds: number
  maxRetries: number
  retryBaseDelayMilliseconds: number
  showUsage: boolean
  secretConfigured: boolean
  enabled: boolean
}

export interface RemoteAIModelBinding {
  configId: string
  configRevision: number
  provider: RemoteAIConfiguration['provider']
  model: string
}

export type RemoteAIWorkspaceMode = 'readOnly' | 'workspaceWrite' | 'fullAccess'

export type RemoteAITodoStatus = 'pending' | 'in_progress' | 'completed'

/** Durable, assistant-owned checklist state for the current conversation. */
export interface RemoteAITodo {
  content: string
  status: RemoteAITodoStatus
}

export type RemoteAISubagentStatus = 'running' | 'ready' | 'completed' | 'failed' | 'interrupted'

/** Metadata for a child conversation created by the device Agent. */
export interface RemoteAISubagent {
  parentConversationId: string
  label: string
  depth: number
  status: RemoteAISubagentStatus
  background: boolean
  createdAt: string
  updatedAt: string
  summary?: string
  error?: string
}

export type RemoteAIGoalPhase = 'active' | 'paused' | 'blocked' | 'complete'

export interface RemoteAIGoalBlockReason {
  code: string
  message: string
}

/** A revisioned long-running objective. Goal activation is deliberately kept
 * separate because it is process-local on the device. */
export interface RemoteAIGoal {
  id: string
  revision: number
  objective: string
  phase: RemoteAIGoalPhase
  blockedReason?: RemoteAIGoalBlockReason
  roundsStarted: number
  maxGoalRounds: number
  createdAt: string
  updatedAt: string
}

export interface RemoteAIApprovalPreview {
  title: string
  description: string
  command?: string
  workingDirectory?: string
  relativePaths?: string[]
  sandboxStatus?: string
}

export interface RemoteAIApproval {
  id: string
  conversationId: string
  generationId: string
  messageId: string
  toolCallId: string
  toolName: string
  preview: RemoteAIApprovalPreview
  expiresAt: string
  allowForSession: boolean
  reason?: string
}

export interface RemoteConversation {
  id: string
  projectId: string
  revision: number
  title: string
  configId: string
  modelBinding: RemoteAIModelBinding
  workspaceMode: RemoteAIWorkspaceMode
  lastMessageSequence: number
  createdAt: string
  updatedAt: string
  messageCount: number
  state: 'idle' | 'generating' | 'failed'
  generationId?: string
  planModeActive?: boolean
  todos?: RemoteAITodo[]
  subagent?: RemoteAISubagent
  goal?: RemoteAIGoal
  goalArmed?: boolean
}

export interface RemoteAIAttachment {
  id: string
  relativePath: string
  name: string
  mimeType: string
  size: number
  sha256: string
  revision: number
}

export interface RemoteAIToolRun {
  id: string
  name: string
  description: string
  status: 'running' | 'succeeded' | 'failed' | 'cancelled'
  arguments: unknown
  result: unknown
  output: string
  errorCode: string
  contentOffset?: number
  startedAt: string
  finishedAt?: string
}

export interface RemoteAIUsage {
  inputTokens: number
  outputTokens: number
  reasoningTokens: number
  cachedInputTokens: number
  totalTokens: number
}

export interface RemoteAIProviderRun {
  provider: string
  model: string
  providerRequestId: string
  finishReason: string
  attemptCount: number
}

export interface RemoteAIMessageContentReference {
  field: 'content' | 'reasoning'
  totalBytes: number
}

export interface RemoteChatMessage {
  id: string
  revision: number
  sequence: number
  role: 'user' | 'assistant' | 'system' | 'tool'
  content: string
  status: 'complete' | 'streaming' | 'failed' | 'stopped'
  errorCode: string
  attachments: RemoteAIAttachment[]
  reasoning: string
  toolRuns: RemoteAIToolRun[]
  usage: RemoteAIUsage
  providerRun: RemoteAIProviderRun
  createdAt: string
  generationId?: string
  contentRef?: RemoteAIMessageContentReference
  reasoningRef?: RemoteAIMessageContentReference
}

export type RemoteAIEventKind =
  | 'chat.text.delta'
  | 'chat.reasoning.delta'
  | 'chat.tool.status'
  | 'chat.approval.requested'
  | 'chat.usage'
  | 'chat.goal.changed'
  | 'chat.plan_mode.changed'
  | 'chat.todo.updated'
  | 'chat.subagent.started'
  | 'chat.subagent.status'
  | 'chat.subagent.message'
  | 'chat.completed'
  | 'chat.failed'
  | 'chat.cancelled'

export interface RemoteAIEvent {
  eventId: string
  conversationId: string
  generationId: string
  messageId: string
  kind: RemoteAIEventKind
  sequence: number
  payload: {
    delta?: string
    toolRun?: RemoteAIToolRun
    usage?: RemoteAIUsage
    message?: RemoteChatMessage
    messageRef?: {
      id: string
      revision: number
      sequence: number
      status: 'complete' | 'failed' | 'stopped'
      generationId: string
    }
    errorCode?: string
    approval?: RemoteAIApproval
    approvalRef?: {
      id: string
      conversationId: string
      generationId: string
      messageId: string
      toolCallId: string
      toolName: string
      source: 'generationState'
    }
    active?: boolean
    todos?: RemoteAITodo[]
    goal?: RemoteAIGoal | null
    agentId?: string
    status?: RemoteAISubagentStatus
    error?: string
  }
  occurredAt: string
}

/** `conversation.get` is a single consistent messages + event-cursor
 * snapshot. New viewers attach only after this cursor. */
export interface RemoteConversationDetail {
  conversation: RemoteConversation
  messages: RemoteChatMessage[]
  nextCursor: string | null
  highWatermark: number
  resetRequired: boolean
  snapshotEventHighWatermark?: number
  /** Compatibility field emitted by v2 Agents. */
  eventHighWatermark?: number
  earliestAvailableEventSequence?: number
}

export interface RemoteGenerationAttachResult {
  accepted: boolean
  conversationId: string
  generationId: string
  revision: number
  replayed: boolean
  resetRequired?: boolean
  highWatermark?: number
  reason?: string
}

export interface RemoteConversationEventPage {
  items: RemoteAIEvent[]
  highWatermark: number
  resetRequired: boolean
  hasMore: boolean
  nextSequence?: number
  earliestAvailableSequence?: number
}

export interface RemoteFileEntry {
  id: string
  revision: number
  name: string
  relativePath: string
  kind: 'file' | 'directory'
  category: 'directory' | 'text' | 'image' | 'video' | 'audio' | 'archive' | 'other'
  extension: string
  size: number
  modifiedAt: string
  readable: boolean
  writable: boolean
}

export interface RemoteRPCContext {
  projectId?: string
}

/** A request-bound stream whose detach operation only sends PEER_CANCEL for
 * its own query. It never maps navigation away from a chat to
 * conversation.cancel. */
export interface RemoteRPCStreamHandle<T> {
  readonly result: Promise<T>
  detach(): Promise<void>
}

export interface RemoteRPCClient {
  readonly connected: Ref<boolean>
  readonly reconnecting: Ref<boolean>
  readonly error: Ref<string>
  connect(): Promise<void>
  close(): Promise<void>
  getCapabilities(refresh?: boolean): Promise<RemoteAgentCapabilities>
  call<T>(method: string, input?: Record<string, unknown>, context?: RemoteRPCContext): Promise<T>
  stream<T>(
    method: string,
    input: Record<string, unknown>,
    onDelta: (delta: T) => void,
    context?: RemoteRPCContext,
  ): Promise<void>
  startStream<TDelta, TResult = unknown>(
    method: string,
    input: Record<string, unknown>,
    onDelta: (delta: TDelta) => void,
    context?: RemoteRPCContext,
  ): RemoteRPCStreamHandle<TResult>
  subscribeAgentEvents(
    input: { afterSequence?: number; heartbeatSeconds?: number },
    onEvent: (event: RemotePeerRpcEvent) => void,
    context: RemoteRPCContext,
  ): Promise<void>
  cancelAgentEventSubscriptions(context: RemoteRPCContext): Promise<void>
  downloadFile(
    relativePath: string,
    onProgress?: (received: number, total: number) => void,
    context?: RemoteRPCContext,
    expectedRevision?: number,
  ): Promise<Blob>
  downloadTaskLog(
    taskId: string,
    runId: string,
    generation: number,
    onProgress?: (received: number, total: number) => void,
    context?: RemoteRPCContext,
  ): Promise<{ blob: Blob; fileName: string }>
  pauseDownloads?(): void
  resumeDownloads?(): void
  uploadFile(
    relativePath: string,
    file: File,
    onProgress?: (sent: number, total: number) => void,
    context?: RemoteRPCContext,
    expectedRevision?: number,
  ): Promise<{ revision: number; entry: RemoteFileEntry }>
}

export const remoteRPCKey: InjectionKey<RemoteRPCClient> = Symbol('remote-rpc')
