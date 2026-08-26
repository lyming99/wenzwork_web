import { describe, expect, it } from 'vitest'

import authoritativeRpcV2Fixture from '../../../api/remote/v1/fixtures/rpc_v2_contract.json'
import rpcV2Fixture from './fixtures/rpc_v2_contract.json'
import {
  agentCapabilityCacheVersion,
  agentSupportsProjectMethod,
  agentSupportsTerminalDuplexStream,
  parseAgentCapabilities,
  peerScopeForMethod,
  remotePeerRequestTimeoutFor,
} from './peerClient'

describe('remote v2 request defaults', () => {
  it('keeps the Web copy equal to the authoritative terminal v3 contract', () => {
    expect(rpcV2Fixture).toEqual(authoritativeRpcV2Fixture)
    expect(rpcV2Fixture.terminalV3Contract).toMatchObject({
      minimumFeatureVersion: 3,
      longPollFeatureFlag: 'terminal.attachLongPoll',
      duplexMinimumFeatureVersion: 4,
      duplexFeatureFlag: 'terminal.duplexStream',
      duplexKeepAliveFeatureFlag: 'terminal.duplexKeepAlive',
      duplexKeepAliveSeconds: 120,
      duplexKeepAliveThroughSequence: 0,
      duplexKeepAliveCreditBytes: 0,
      duplexStreamKind: 'terminal',
      duplexFrameType: 'streamData',
      duplexInputWindowBytes: 32768,
      duplexOutputWindowBytes: 65536,
      duplexRawBytes: true,
      duplexCumulativeInputAck: true,
      duplexOutputByteCredit: true,
      disconnectGraceSeconds: 300,
      idleSeconds: 0,
      lifetimeSeconds: 0,
    })
    expect(rpcV2Fixture.sequencerLifecycleContract.maximumStreamsPerLink).toBe(131_072)
  })

  it('uses bounded request deadlines without a Peer-ticket lifetime', () => {
    expect(remotePeerRequestTimeoutFor('conversation.list', false)).toBe(30_000)
    expect(remotePeerRequestTimeoutFor('chat.history', false)).toBe(30_000)
    expect(remotePeerRequestTimeoutFor('project.list', false)).toBe(30_000)
    expect(remotePeerRequestTimeoutFor('conversation.send', true)).toBe(15 * 60_000)
  })
})

describe('agent capability negotiation', () => {
  it('builds a deterministic cache fingerprint and changes it with the usable surface', () => {
    const first = parseAgentCapabilities({
      protocol: { minimum: 1, maximum: 1 },
      featureVersions: { files: 2, projects: 2, ai: 2 },
      features: { 'file.v2': true, 'project.v2': true, 'ai.v2': true },
      platform: { os: 'linux', arch: 'amd64' },
      shells: ['zsh', 'sh'],
      taskRunners: ['codex', 'script'],
      resourceLimits: {},
    })
    const reordered = parseAgentCapabilities({
      protocol: { minimum: 1, maximum: 1 },
      featureVersions: { ai: 2, projects: 2, files: 2 },
      features: { 'ai.v2': true, 'project.v2': true, 'file.v2': true },
      platform: { os: 'linux', arch: 'amd64' },
      shells: ['sh', 'zsh'],
      taskRunners: ['script', 'codex'],
      resourceLimits: { rpcPayloadBytes: 1 },
    })
    const upgraded = parseAgentCapabilities({
      protocol: { minimum: 1, maximum: 1 },
      featureVersions: { files: 2, projects: 2, ai: 3 },
      features: { 'file.v2': true, 'project.v2': true, 'ai.v2': true },
      platform: { os: 'linux', arch: 'amd64' },
      shells: ['zsh', 'sh'],
      taskRunners: ['codex', 'script'],
      resourceLimits: {},
    })

    expect(agentCapabilityCacheVersion(first)).toBe(agentCapabilityCacheVersion(reordered))
    expect(agentCapabilityCacheVersion(upgraded)).not.toBe(agentCapabilityCacheVersion(first))
    expect(
      agentCapabilityCacheVersion({
        ...first,
        features: { ...first.features, 'ai.v2': false },
      }),
    ).not.toBe(agentCapabilityCacheVersion(first))
  })

  it('parses advertised limits and gates v2 project methods', () => {
    const capabilities = parseAgentCapabilities({
      protocol: { minimum: 1, maximum: 1 },
      featureVersions: { projects: 2, files: 2, terminal: 1, ai: 1 },
      features: {
        'project.v2': true,
        'file.v2': true,
        'project.remoteCreate': true,
        'project.remoteRoots': true,
        'terminal.interactive': false,
      },
      platform: { os: 'linux', arch: 'amd64' },
      shells: ['sh'],
      taskRunners: [],
      resourceLimits: { rpcPayloadBytes: 61440 },
      remoteV2Resources: { activeStreamCount: 3, sequencerTombstones: 9 },
    })

    expect(capabilities.protocolMinimum).toBe(1)
    expect(capabilities.featureVersions.projects).toBe(2)
    expect(capabilities.remoteV2Resources).toEqual({
      activeStreamCount: 3,
      sequencerTombstones: 9,
    })
    expect(agentSupportsProjectMethod(capabilities, 'file.list')).toBe(true)
    expect(agentSupportsProjectMethod(capabilities, 'terminal.execute')).toBe(true)
    expect(agentSupportsProjectMethod(capabilities, 'conversation.list')).toBe(true)
    expect(agentSupportsProjectMethod(capabilities, 'project.create')).toBe(true)
    expect(peerScopeForMethod('project.create')).toBe('remote.peer.query')
    expect(agentSupportsProjectMethod(capabilities, 'project.directory.list')).toBe(true)
    expect(peerScopeForMethod('project.directory.list')).toBe('remote.peer.query')
    expect(agentSupportsProjectMethod(capabilities, 'project.remove')).toBe(false)
    expect(
      agentSupportsProjectMethod(
        {
          ...capabilities,
          featureVersions: { ...capabilities.featureVersions, projects: 3 },
          features: { ...capabilities.features, 'project.remoteRemove': true },
        },
        'project.remove',
      ),
    ).toBe(true)
    expect(peerScopeForMethod('project.remove')).toBe('remote.peer.query')
    expect(peerScopeForMethod('terminal.execute')).toBe('remote.peer.terminal')
    expect(peerScopeForMethod('terminal.open')).toBe('remote.peer.terminal.interactive')
    expect(peerScopeForMethod('task.update')).toBe('remote.peer.task.control')
    expect(peerScopeForMethod('workflow.run.get')).toBe('remote.peer.task.control')
    expect(peerScopeForMethod('ai.config.models')).toBe('remote.peer.ai.config')
    expect(peerScopeForMethod('agent.environment.update')).toBe('remote.peer.ai.config')
    expect(peerScopeForMethod('conversation.approval.respond')).toBe('remote.peer.ai.tools')
    expect(peerScopeForMethod('conversation.question.answer')).toBe('remote.peer.ai.tools')
    expect(peerScopeForMethod('conversation.send', { enableWorkspaceTools: true })).toBe(
      'remote.peer.ai.tools',
    )
    expect(peerScopeForMethod('conversation.send', { enableWorkspaceTools: false })).toBe(
      'remote.peer.ai.chat',
    )
    expect(peerScopeForMethod('conversation.goal.create', { enableWorkspaceTools: true })).toBe(
      'remote.peer.ai.tools',
    )
    for (const method of ['file.create-text', 'file.move', 'file.delete.prepare']) {
      expect(peerScopeForMethod(method)).toBe('remote.peer.file.send')
    }
    for (const method of ['file.search', 'file.details']) {
      expect(peerScopeForMethod(method)).toBe('remote.peer.file.receive')
    }
    expect(agentSupportsProjectMethod(capabilities, 'terminal.open')).toBe(false)
    expect(
      agentSupportsProjectMethod(
        {
          ...capabilities,
          featureVersions: { ...capabilities.featureVersions, terminal: 3 },
          features: {
            ...capabilities.features,
            'terminal.interactive': true,
            'terminal.attachLongPoll': true,
          },
        },
        'terminal.open',
      ),
    ).toBe(true)
    expect(
      agentSupportsProjectMethod(
        {
          ...capabilities,
          featureVersions: { ...capabilities.featureVersions, terminal: 3 },
          features: { ...capabilities.features, 'terminal.interactive': true },
        },
        'terminal.open',
      ),
    ).toBe(false)
    expect(agentSupportsProjectMethod(capabilities, 'task.list')).toBe(false)
    expect(capabilities.features['terminal.interactive']).toBe(false)
    const terminalV4 = {
      ...capabilities,
      featureVersions: { ...capabilities.featureVersions, terminal: 4 },
      features: {
        ...capabilities.features,
        'terminal.interactive': true,
        'terminal.attachLongPoll': true,
        'terminal.duplexStream': true,
      },
    }
    expect(agentSupportsTerminalDuplexStream(terminalV4)).toBe(false)
    expect(
      agentSupportsTerminalDuplexStream({
        ...terminalV4,
        features: {
          ...terminalV4.features,
          'terminal.duplexKeepAlive': true,
        },
      }),
    ).toBe(true)

    const taskCapabilities = parseAgentCapabilities({
      protocol: { minimum: 1, maximum: 1 },
      featureVersions: { projects: 2, tasks: 2, workflows: 2 },
      features: { 'project.v2': true, 'tasks.v2': true, 'workflow.v2': true },
      platform: { os: 'linux', arch: 'amd64' },
      shells: ['sh'],
      taskRunners: ['codex'],
      resourceLimits: { taskRpcPayloadBytes: 2097152 },
    })
    expect(agentSupportsProjectMethod(taskCapabilities, 'task.list')).toBe(true)
    expect(agentSupportsProjectMethod(taskCapabilities, 'workflow.run.get')).toBe(true)

    const taskOnlyCapabilities = parseAgentCapabilities({
      protocol: { minimum: 1, maximum: 1 },
      featureVersions: { projects: 2, tasks: 2 },
      features: { 'project.v2': true, 'tasks.v2': true },
      platform: { os: 'linux', arch: 'amd64' },
      shells: [],
      taskRunners: [],
      resourceLimits: {},
    })
    expect(agentSupportsProjectMethod(taskOnlyCapabilities, 'workflow.run.get')).toBe(false)

    const goalCapabilities = parseAgentCapabilities({
      protocol: { minimum: 1, maximum: 1 },
      featureVersions: { projects: 2, ai: 8 },
      features: { 'project.v2': true, 'ai.v2': true, 'ai.goal': true },
      platform: { os: 'linux', arch: 'amd64' },
      shells: [],
      taskRunners: [],
      resourceLimits: {},
    })
    expect(agentSupportsProjectMethod(goalCapabilities, 'conversation.goal.create')).toBe(true)
    expect(
      agentSupportsProjectMethod(
        { ...goalCapabilities, featureVersions: { ...goalCapabilities.featureVersions, ai: 7 } },
        'conversation.goal.create',
      ),
    ).toBe(false)
  })

  it('fails closed for malformed or undeclared capability sets', () => {
    expect(() =>
      parseAgentCapabilities({
        protocol: { minimum: 2, maximum: 1 },
      }),
    ).toThrow('protocol range is invalid')

    const capabilities = parseAgentCapabilities({
      protocol: { minimum: 1, maximum: 1 },
      featureVersions: { projects: 1, files: 1 },
      features: { 'project.v2': false, 'file.v2': true },
      platform: { os: 'linux', arch: 'amd64' },
      shells: [],
      taskRunners: [],
      resourceLimits: {},
    })
    expect(agentSupportsProjectMethod(capabilities, 'file.list')).toBe(false)

    const oldDomains = parseAgentCapabilities({
      protocol: { minimum: 1, maximum: 1 },
      featureVersions: { projects: 2, files: 1, terminal: 0, ai: 0 },
      features: { 'project.v2': true, 'file.v2': true },
      platform: { os: 'linux', arch: 'amd64' },
      shells: [],
      taskRunners: [],
      resourceLimits: {},
    })
    expect(agentSupportsProjectMethod(oldDomains, 'file.list')).toBe(false)
    expect(agentSupportsProjectMethod(oldDomains, 'terminal.execute')).toBe(false)
    expect(agentSupportsProjectMethod(oldDomains, 'conversation.list')).toBe(false)

    const incompatibleProtocol = parseAgentCapabilities({
      protocol: { minimum: 2, maximum: 2 },
      featureVersions: { projects: 2, files: 2 },
      features: { 'project.v2': true, 'file.v2': true },
      platform: { os: 'linux', arch: 'amd64' },
      shells: [],
      taskRunners: [],
      resourceLimits: {},
    })
    expect(agentSupportsProjectMethod(incompatibleProtocol, 'file.list')).toBe(false)
  })
})

describe('cross-end capability domain contract', () => {
  it('covers project, file, terminal, task and AI protocol routing metadata', () => {
    expect(rpcV2Fixture.capabilityContract).toMatchObject({
      method: 'agent.capabilities.get',
      scope: 'remote.peer.query',
      projectRequired: false,
    })
    expect(rpcV2Fixture.capabilityContract.requiredFields).toEqual([
      'agentBuildId',
      'connectionEpoch',
      'capabilityVersion',
      'protocol.minimum',
      'protocol.maximum',
      'featureVersions',
      'features',
      'platform.os',
      'platform.arch',
      'shells',
      'taskRunners',
      'resourceLimits',
      'taskLogMetrics',
      'remoteV2Resources',
      'remoteOperationJournal',
    ])
    expect(rpcV2Fixture.remoteProjectCreate).toEqual({
      method: 'project.create',
      scope: 'remote.peer.query',
      channel: 'peer-rpc',
      projectRequired: false,
      featureVersionKey: 'projects',
      minimumFeatureVersion: 2,
      featureFlag: 'project.remoteCreate',
      allowedInputFields: ['name', 'displayName', 'gitUrl', 'directoryId', 'parentDirectoryId'],
      forbiddenInputFields: ['path', 'projectId', 'relativePath'],
    })
    expect(peerScopeForMethod(rpcV2Fixture.remoteProjectCreate.method)).toBe(
      rpcV2Fixture.remoteProjectCreate.scope,
    )
    expect(rpcV2Fixture.remoteProjectRemove).toEqual({
      method: 'project.remove',
      scope: 'remote.peer.query',
      channel: 'peer-rpc',
      projectRequired: false,
      featureVersionKey: 'projects',
      minimumFeatureVersion: 3,
      featureFlag: 'project.remoteRemove',
      allowedInputFields: ['projectId', 'expectedRevision'],
      expectedRevisionRequired: true,
      softDeleteOnly: true,
      blockingRelations: ['ai_conversations.generating', 'tasks.non_terminal'],
    })
    expect(peerScopeForMethod(rpcV2Fixture.remoteProjectRemove.method)).toBe(
      rpcV2Fixture.remoteProjectRemove.scope,
    )
    expect(rpcV2Fixture.fileV2Contract).toMatchObject({
      featureVersionKey: 'files',
      minimumFeatureVersion: 2,
      featureFlag: 'file.v2',
      projectRequired: true,
      readScope: 'remote.peer.file.receive',
      writeScope: 'remote.peer.file.send',
      textMaximumBytes: 524288,
      managedFileMaximumBytes: 0,
      chunkBytes: 32768,
    })
    expect(rpcV2Fixture.fileV2Contract.entryRequiredFields).toHaveLength(11)
    for (const method of rpcV2Fixture.fileV2Contract.readMethods) {
      expect(peerScopeForMethod(method)).toBe(rpcV2Fixture.fileV2Contract.readScope)
    }
    for (const method of rpcV2Fixture.fileV2Contract.writeMethods) {
      expect(peerScopeForMethod(method)).toBe(rpcV2Fixture.fileV2Contract.writeScope)
    }
    expect(rpcV2Fixture.fileV2Contract.recursiveDelete).toEqual({
      prepareMethod: 'file.delete.prepare',
      commitMethod: 'file.delete',
      featureFlag: 'recursiveDelete.confirmed',
      expectedRevisionRequired: true,
      oneTimeConfirmationTokenRequired: true,
    })
    expect(rpcV2Fixture.terminalV3Contract).toMatchObject({
      minimumFeatureVersion: 3,
      featureFlag: 'terminal.interactive',
      longPollFeatureFlag: 'terminal.attachLongPoll',
      scope: 'remote.peer.terminal.interactive',
      legacyMethod: 'terminal.execute',
      legacyScope: 'remote.peer.terminal',
      events: ['terminal.output', 'terminal.exit'],
      resetRequiredOnEviction: true,
      attachWaitSeconds: 25,
      attachMaximumWaitSeconds: 30,
      attachMaximumPerMinute: 6,
      attachBurst: 2,
      singleAttachPerSession: true,
      attachCompletionReasons: ['events', 'timeout', 'exit', 'reset'],
      attachResponseDiagnosticFields: ['completionReason', 'heldMilliseconds', 'eventCount'],
    })
    for (const method of rpcV2Fixture.terminalV3Contract.methods) {
      expect(peerScopeForMethod(method)).toBe(rpcV2Fixture.terminalV3Contract.scope)
    }
    expect(rpcV2Fixture.taskV2Contract).toMatchObject({
      featureVersionKey: 'tasks',
      minimumFeatureVersion: 2,
      featureFlag: 'tasks.v2',
      projectRequired: true,
      scope: 'remote.peer.task.control',
      compareAndSwapRequired: true,
      resetRequiredOnEviction: true,
      sensitiveBodyChannel: 'peer-rpc',
    })
    for (const method of rpcV2Fixture.taskV2Contract.methods) {
      expect(peerScopeForMethod(method)).toBe(rpcV2Fixture.taskV2Contract.scope)
    }
    for (const method of rpcV2Fixture.taskV2Contract.workflowMethods) {
      expect(peerScopeForMethod(method)).toBe(rpcV2Fixture.taskV2Contract.scope)
    }
    expect(rpcV2Fixture.taskLogV1Contract).toMatchObject({
      featureVersionKey: 'taskLogs',
      minimumFeatureVersion: 1,
      seekFeatureFlag: 'taskLogs.fileSeek',
      bulkDownloadFeatureFlag: 'taskLogs.bulkDownload',
      projectRequired: true,
      legacyReadScope: 'remote.task.read',
      peerScope: 'remote.peer.task.control',
      runProjectionFields: [
        'logAvailable',
        'logState',
        'logGeneration',
        'logFormatVersion',
        'logSizeBytes',
      ],
      absolutePathForbidden: true,
      sqliteBodyForbidden: true,
      wholeRunRetention: true,
    })
    expect(rpcV2Fixture.taskLogV1Contract.seek).toMatchObject({
      method: 'task.logs',
      mutuallyExclusiveCursorFields: ['offset', 'tailBytes', 'beforeOffset'],
      maximumBytes: 32 * 1024,
      responseMaximumBytes: 48 * 1024,
      cursorUnit: 'utf8-file-bytes',
      boundedRead: true,
      completePhysicalRecords: true,
    })
    expect(rpcV2Fixture.taskLogV1Contract.download).toMatchObject({
      prepareMethod: 'task.logs.download.prepare',
      sourceKind: 'taskLog',
      streamKind: 'file',
      chunkBytes: 32 * 1024,
      maximumBytes: 64 * 1024 * 1024,
      hash: 'sha256',
      immutablePreparedPrefix: true,
      acknowledgedResume: true,
      peerSessionBound: true,
    })
    expect(rpcV2Fixture.taskLogV1Contract.event).toEqual({
      type: 'task.logs.available',
      cursorKind: 'task_log_bytes',
      dataFields: ['runId', 'generation', 'highWatermark'],
      contentFree: true,
      invalidateOnGenerationChange: true,
    })
    for (const method of [
      rpcV2Fixture.taskLogV1Contract.seek.method,
      rpcV2Fixture.taskLogV1Contract.download.prepareMethod,
    ]) {
      expect(peerScopeForMethod(method)).toBe(rpcV2Fixture.taskLogV1Contract.peerScope)
    }
    expect(rpcV2Fixture.aiV2Contract).toMatchObject({
      featureVersionKey: 'ai',
      minimumFeatureVersion: 2,
      featureFlag: 'ai.v2',
      configProjectRequired: false,
      conversationProjectRequired: true,
      configScope: 'remote.peer.ai.config',
      chatScope: 'remote.peer.ai.chat',
      toolsScope: 'remote.peer.ai.tools',
      monotonicEventSequence: true,
      persistedEventReplay: true,
      compareAndSwapRequired: true,
      absolutePathForbidden: true,
      sensitiveBodyChannel: 'peer-rpc',
      persistentTerminalFeatureFlag: 'ai.persistentTerminal',
      agentLoopMinimumFeatureVersion: 6,
      agentLoopFeatureFlag: 'ai.agentLoop',
      durableInboxFeatureFlag: 'ai.durableInbox',
      collaborationMinimumFeatureVersion: 7,
      goalMinimumFeatureVersion: 8,
      goalFeatureFlag: 'ai.goal',
      planModeFeatureFlag: 'ai.planMode',
      todoFeatureFlag: 'ai.todo',
      subagentsFeatureFlag: 'ai.subagents',
      inboxDestinations: ['nextTurn', 'nextStep'],
      cancelSupportsKeepInbox: true,
    })
    expect(rpcV2Fixture.aiV2Contract.providers).toEqual([
      'openai',
      'anthropic',
      'google',
      'deepseek',
      'ollama',
      'openai-compatible',
    ])
    expect(rpcV2Fixture.aiV2Contract.tools).toEqual([
      'list_files',
      'search_files',
      'read_file',
      'read_tool_result',
      'read_image',
      'web_search',
      'web_fetch',
      'terminal_open',
      'terminal_send',
      'terminal_read',
      'terminal_signal',
      'terminal_close',
      'terminal_list',
      'write_file',
      'replace_in_file',
      'rollback_file_change',
      'run_command',
      'get_goal',
      'create_goal',
      'update_goal',
      'todo_write',
      'exit_plan_mode',
      'ask_user_question',
      'skill',
      'spawn_agent',
      'subagent_fork',
      'list_agents',
      'send_message',
      'interrupt_agent',
      'job_list',
      'job_output',
      'job_kill',
    ])
    expect(rpcV2Fixture.aiV2Contract.todoStatuses).toEqual(['pending', 'in_progress', 'completed'])
    expect(rpcV2Fixture.aiV2Contract.subagentStatuses).toEqual([
      'running',
      'ready',
      'completed',
      'failed',
      'interrupted',
    ])
    expect(rpcV2Fixture.aiV2Contract.goalPhases).toEqual([
      'active',
      'paused',
      'blocked',
      'complete',
    ])
    for (const method of rpcV2Fixture.aiV2Contract.configMethods) {
      expect(peerScopeForMethod(method)).toBe(rpcV2Fixture.aiV2Contract.configScope)
    }
    for (const method of rpcV2Fixture.aiV2Contract.chatMethods) {
      expect(peerScopeForMethod(method)).toBe(rpcV2Fixture.aiV2Contract.chatScope)
    }
    expect(rpcV2Fixture.aiV2Contract.workspaceToolIntentField).toBe('enableWorkspaceTools')
    expect(rpcV2Fixture.aiV2Contract.workspaceToolIntentScope).toBe(
      rpcV2Fixture.aiV2Contract.toolsScope,
    )
    expect(rpcV2Fixture.aiV2Contract.workspaceToolIntentDurable).toBe(true)
    for (const method of rpcV2Fixture.aiV2Contract.workspaceToolIntentMethods) {
      expect(peerScopeForMethod(method, { enableWorkspaceTools: true })).toBe(
        rpcV2Fixture.aiV2Contract.toolsScope,
      )
      expect(peerScopeForMethod(method, { enableWorkspaceTools: false })).toBe(
        rpcV2Fixture.aiV2Contract.chatScope,
      )
    }
    for (const method of rpcV2Fixture.aiV2Contract.toolOnlyMethods) {
      expect(peerScopeForMethod(method)).toBe(rpcV2Fixture.aiV2Contract.toolsScope)
    }
    expect(rpcV2Fixture.aiV3GenerationRecoveryContract).toMatchObject({
      featureVersionKey: 'ai',
      minimumFeatureVersion: 3,
      featureFlag: 'ai.generationRecovery',
      snapshot: {
        method: 'conversation.get',
        projectRequired: true,
        requiredFields: [
          'conversation',
          'messages',
          'snapshotEventHighWatermark',
          'earliestAvailableEventSequence',
        ],
        consistentReadTransaction: true,
      },
      attach: {
        method: 'conversation.generation.attach',
        scope: 'remote.peer.ai.chat',
        projectRequired: true,
        requestFields: ['conversationId', 'generationId', 'afterSequence'],
        live: true,
        cacheReplayEvents: false,
        durableAfterRoute: false,
        detach: 'peer-cancel-query-only',
        resetRequiredOnCursorExpiredOrQueueOverflow: true,
      },
      regenerate: {
        method: 'conversation.regenerate',
        scope: 'remote.peer.ai.chat',
        projectRequired: true,
        requestFields: [
          'conversationId',
          'messageId',
          'regenerationRequestId',
          'workspaceMode',
          'enableWorkspaceTools',
        ],
        stableRequestIdRequired: true,
        generationIdEqualsRequestId: true,
      },
      limits: {
        subscriptionCount: 8,
        conversationSubscriptionCount: 4,
        queueCount: 256,
        queueBytes: 524288,
      },
    })
    expect(peerScopeForMethod(rpcV2Fixture.aiV3GenerationRecoveryContract.attach.method)).toBe(
      rpcV2Fixture.aiV3GenerationRecoveryContract.attach.scope,
    )
    expect(
      peerScopeForMethod(rpcV2Fixture.aiV3GenerationRecoveryContract.regenerate.method),
    ).toBe(rpcV2Fixture.aiV3GenerationRecoveryContract.regenerate.scope)
    expect(rpcV2Fixture.domainContracts.map(({ domain }) => domain)).toEqual([
      'project',
      'file',
      'terminal',
      'task',
      'ai',
    ])
    for (const contract of rpcV2Fixture.domainContracts.filter(
      ({ channel }) => channel === 'peer-rpc',
    )) {
      expect(peerScopeForMethod(contract.representativeMethod)).toBe(contract.scope)
      // Project discovery is a device-level query. The remaining domains act
      // on a concrete project and therefore bind that project in the ticket.
      expect(contract.projectRequired).toBe(contract.domain !== 'project')
    }

    expect(
      rpcV2Fixture.compatibilityContract.matrix.map(
        ({ clientGeneration, agentGeneration, expectedMode }) =>
          `${clientGeneration}/${agentGeneration}:${expectedMode}`,
      ),
    ).toEqual([
      'v1/v1:legacy-only',
      'v1/v2:legacy-on-v2-agent',
      'v2/v1:safe-degrade',
      'v2/v2:capability-gated-v2',
    ])
    for (const combination of rpcV2Fixture.compatibilityContract.matrix) {
      expect(combination.mustNotCrash).toBe(true)
      if (combination.clientGeneration === 'v1') {
        expect(combination.capabilityQueryRequired).toBe(false)
        expect(combination.newMethodsAllowed).toBe(false)
      }
      if (combination.agentGeneration === 'v1') {
        expect(combination.newMethodsAllowed).toBe(false)
      }
    }
    expect(rpcV2Fixture.compatibilityContract.metrics).toEqual({
      capabilityVersions: [
        'files.v1',
        'files.v2',
        'terminal.v1',
        'terminal.v2',
        'tasks.v1',
        'tasks.v2',
        'ai.v1',
        'ai.v2',
      ],
      successErrorCode: 'ok',
      requiredFields: ['capabilityVersion', 'errorCode', 'callCount', 'totalDurationMilliseconds'],
      forbiddenDimensions: [
        'userId',
        'deviceId',
        'projectId',
        'projectName',
        'path',
        'command',
        'prompt',
        'response',
        'fileDigest',
      ],
    })
  })
})

describe('Agent event contract fixture', () => {
  it('keeps the event scope, protocol kinds, limits and forbidden payload boundary aligned', () => {
    expect(rpcV2Fixture.eventV1Contract).toMatchObject({
      featureVersionKey: 'events',
      minimumFeatureVersion: 1,
      featureFlag: 'events.v1',
      scope: 'remote.peer.events',
      method: 'event.subscribe',
      projectRequired: true,
      readOnlyMethods: ['event.subscribe', 'agent.capabilities.get'],
      rpcEventKinds: { subscriptionControl: 13, agentStateChanged: 14 },
      limits: {
        subscriptionCount: 8,
        projectSubscriptionCount: 4,
        queueCount: 256,
        queueBytes: 524288,
        replayCount: 4096,
        payloadBytes: 4096,
        heartbeatMinimumSeconds: 15,
        heartbeatMaximumSeconds: 60,
      },
      replayCache: 'disabled',
    })
    expect(peerScopeForMethod(rpcV2Fixture.eventV1Contract.method)).toBe(
      rpcV2Fixture.eventV1Contract.scope,
    )
    expect(rpcV2Fixture.eventV1Contract.forbiddenPayloadFields).toEqual(
      expect.arrayContaining([
        'prompt',
        'content',
        'reasoning',
        'toolArguments',
        'toolOutput',
        'taskDefinition',
        'command',
        'logContent',
        'path',
        'accessToken',
        'ticket',
        'secret',
      ]),
    )
    expect(rpcV2Fixture.eventV1Contract.completionReasons).toEqual([
      'clientCancel',
      'sessionRenewal',
      'deadline',
      'reset',
      'agentShutdown',
      'policyRevoked',
    ])
  })
})
