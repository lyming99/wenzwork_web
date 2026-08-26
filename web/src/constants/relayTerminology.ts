export const RELAY_TERMINOLOGY = {
  region: {
    internal: 'Region',
    label: '服务区域',
    description: '中继服务所在的地域。',
  },
  pool: {
    internal: 'Pool',
    label: '资源池',
    description: '相同规格、网络或用途的资源集合。',
  },
  cell: {
    internal: 'Cell',
    label: '中继组',
    description: '共享公网接入地址、容量和调度策略的一组中继节点。',
  },
  relay: {
    internal: 'Relay',
    label: '中继服务',
    description: '为客户端提供安全长连接和消息转发的服务能力。',
  },
  node: {
    internal: 'Relay Node',
    label: '中继节点',
    description: '实际接收客户端连接的服务器进程。',
  },
  endpoint: {
    internal: 'Endpoint',
    label: '公网接入地址',
    description: '客户端连接中继组使用的 WSS 地址。',
  },
  installation: {
    internal: 'Installation',
    label: '节点安装记录',
    description: '一台服务器上稳定存在的中继程序安装，进程重启后保持不变。',
  },
  instance: {
    internal: 'Instance',
    label: '运行实例',
    description: '中继程序的一次启动，进程重启后重新生成。',
  },
  assignment: {
    internal: 'Assignment',
    label: '用户中继归属',
    description: '用户当前归属的中继组。',
  },
  allocation: {
    internal: 'Allocation',
    label: '接入分配结果',
    description: '客户端获取的接入地址、连接凭证和重试策略。',
  },
  ticket: {
    internal: 'Ticket',
    label: '短期连接凭证',
    description: '客户端连接中继节点使用的临时凭证。',
  },
  enrollment: {
    internal: 'Enrollment',
    label: '节点注册',
    description: '中继节点首次与管理端建立身份绑定。',
  },
  enrollmentToken: {
    internal: 'Enrollment Token',
    label: '一次性注册令牌',
    description: '短期、单次使用的节点注册凭据。',
  },
  accessKey: {
    internal: 'Relay Access Key',
    label: '中继访问密钥',
    description: '管理端生成、默认不过期且可随时吊销的节点连接凭据。',
  },
  release: {
    internal: 'Release',
    label: '中继程序版本',
    description: '可以安装或升级的中继服务端版本。',
  },
  heartbeat: {
    internal: 'Heartbeat',
    label: '心跳',
    description: '节点定期向管理端报告的运行状态。',
  },
  routingReady: {
    internal: 'Routing Ready',
    label: '可接入',
    description: '节点依赖正常，可以接收客户端连接。',
  },
  drain: {
    internal: 'Drain',
    label: '排空',
    description: '停止接收新连接并等待已有连接退出。',
  },
  revoke: {
    internal: 'Revoke',
    label: '吊销',
    description: '废除节点身份或连接权限。',
  },
  client: {
    internal: 'Client',
    label: '客户端',
    description: '需要接入中继服务的设备程序。',
  },
  hostMaintenance: {
    internal: 'Host maintenance',
    label: '管理端维护循环',
    description: '由 Host 内部执行排空、迁移和租约维护，不是独立服务。',
  },
  scheduler: {
    internal: 'Scheduler',
    label: '接入调度服务',
    description: '选择中继组并生成接入分配结果。',
  },
  directory: {
    internal: 'Directory',
    label: '节点目录服务',
    description: '旧版 mTLS 接入的兼容目录；默认由 Relay 直接向 Host 注册和心跳。',
  },
} as const

export const RELAY_STATUS_LABELS = {
  draft: '待配置',
  pending_enrollment: '等待节点连接',
  enrolled: '已注册',
  pending_activation: '等待启用',
  active: '运行中',
  ready: '可接入',
  starting: '启动中',
  draining: '排空中',
  disabled: '已停用',
  offline: '离线',
  failed: '运行异常',
  revoked: '已吊销',
  expired: '已过期',
  validating: '正在验证',
  validated: '验证通过',
  current: '当前生效',
  historical: '历史记录',
  pending: '等待处理',
  queued: '已排队',
  running: '正在执行',
  succeeded: '操作成功',
  cancelled: '已取消',
  timed_out: '操作超时',
  deleted: '已删除',
  stopped: '已停止',
  forced_offline: '强制离线',
  retired: '已退役',
  skipped: '已跳过',
  effective: '已生效',
  published: '已发布',
  waiting: '等待安装',
  heartbeat_received: '已收到心跳',
  consumed: '已使用',
  locked: '已锁定',
  superseded: '已取代',
  quarantined: '已隔离',
  degraded: '服务降级',
  pending_approval: '等待审批',
  cancel_requested: '正在取消',
  dispatched: '已派发',
  accepted: '已接收',
  rejected: '已拒绝',
  available: '可以使用',
  removed: '已移除',
  unavailable: '暂不可用',
  scheduled: '已安排',
} as const

export type RelayKnownStatus = keyof typeof RELAY_STATUS_LABELS

export const RELAY_INSTALLATION_STATUSES = [
  'draft',
  'pending_enrollment',
  'enrolled',
  'pending_activation',
  'active',
  'draining',
  'disabled',
  'revoked',
  'expired',
  'deleted',
] as const satisfies readonly RelayKnownStatus[]

export const RELAY_INSTANCE_STATUSES = [
  'starting',
  'ready',
  'draining',
  'stopped',
  'failed',
  'offline',
  'forced_offline',
] as const satisfies readonly RelayKnownStatus[]

export const RELAY_CELL_STATUSES = [
  'draft',
  'active',
  'draining',
  'disabled',
] as const satisfies readonly RelayKnownStatus[]

export const RELAY_ENDPOINT_STATUSES = [
  'draft',
  'validating',
  'validated',
  'active',
  'draining',
  'retired',
  'failed',
] as const satisfies readonly RelayKnownStatus[]

export const RELAY_ASSIGNMENT_STATUSES = [
  'pending',
  'effective',
  'historical',
  'expired',
] as const satisfies readonly RelayKnownStatus[]

export const RELAY_OPERATION_STATUSES = [
  'pending',
  'queued',
  'running',
  'succeeded',
  'failed',
  'cancelled',
  'timed_out',
] as const satisfies readonly RelayKnownStatus[]

export const RELAY_OPERATION_ITEM_STATUSES = [
  'pending',
  'running',
  'succeeded',
  'failed',
  'skipped',
] as const satisfies readonly RelayKnownStatus[]

export const RELAY_OPERATION_LABELS = {
  node_drain: '排空中继节点',
  cell_drain: '排空中继组',
  cell_update: '更新中继组',
  migrate_user: '迁移用户中继归属',
  user_unpin: '取消固定用户中继归属',
  bulk_migrate: '批量迁移用户中继归属',
  endpoint_validate: '验证公网接入地址',
  endpoint_activate: '启用公网接入地址',
  installation_revoke: '吊销节点安装记录',
  certificate_rotate: '轮换节点证书',
  rebuild_projection: '重建中继状态投影',
} as const

export const RELAY_ASSIGNMENT_MODE_LABELS = {
  auto: '自动调度',
  pinned: '固定中继组',
} as const

export const RELAY_ERROR_MESSAGES = {
  loadTopology: '暂时无法读取中继服务拓扑。',
  loadCellStatus: '暂时无法读取中继组运行状态。',
  loadInstallation: '暂时无法读取节点安装记录详情。',
  loadOperation: '暂时无法读取操作进度。',
  activateCell: '无法启用中继组。',
  createEndpoint: '无法创建或验证公网接入地址。',
  validateEndpoint: '无法开始验证公网接入地址。',
  activateEndpoint: '无法启用公网接入地址。',
  drainNode: '无法开始排空中继节点。',
  drainCell: '无法开始排空中继组。',
  queryAssignment: '无法查询用户中继归属。',
  createInstallation: '无法创建节点安装记录。',
  issueEnrollmentToken: '无法重新签发一次性注册令牌。',
  issueAccessKey: '无法重新生成中继访问密钥。',
} as const

export const RELAY_HELP = {
  installationAndInstance:
    '节点安装记录代表一台服务器上的稳定安装；运行实例代表中继程序的一次启动，进程重启后会重新生成。',
  endpointRevision: '新地址验证并启用后，旧地址会进入排空历史。',
  drainNodeImpact: '排空后该节点停止接收新连接，并等待已有连接退出。',
  drainCellImpact: '排空后该中继组停止接收新连接，并通知已有连接迁移。',
  revokeImpact: '吊销后 Access Key、节点身份和活动证书立即失效，节点无法继续连接或心跳。',
  enrollmentToken: '一次性注册令牌只显示一次，不得写入 URL、命令、脚本正文或日志。',
  accessKey: 'Access Key 明文只显示一次，应只保存在权限受限的 Relay .env 文件中。',
} as const

export const relayStatusLabel = (status: string | null | undefined) =>
  status && status in RELAY_STATUS_LABELS
    ? RELAY_STATUS_LABELS[status as RelayKnownStatus]
    : '未知状态'

export const relayOperationLabel = (operation: string | null | undefined) =>
  operation && operation in RELAY_OPERATION_LABELS
    ? RELAY_OPERATION_LABELS[operation as keyof typeof RELAY_OPERATION_LABELS]
    : '中继操作'

export const relayAssignmentModeLabel = (mode: string | null | undefined) =>
  mode && mode in RELAY_ASSIGNMENT_MODE_LABELS
    ? RELAY_ASSIGNMENT_MODE_LABELS[mode as keyof typeof RELAY_ASSIGNMENT_MODE_LABELS]
    : '未知模式'

export type RelayStatusTone = 'ready' | 'waiting' | 'danger' | 'neutral'

export const relayStatusTone = (status: string | null | undefined): RelayStatusTone => {
  if (
    status &&
    ['active', 'ready', 'validated', 'current', 'effective', 'succeeded', 'published'].includes(
      status,
    )
  ) {
    return 'ready'
  }
  if (
    status &&
    [
      'draft',
      'pending_enrollment',
      'enrolled',
      'pending_activation',
      'starting',
      'validating',
      'pending',
      'queued',
      'running',
      'waiting',
    ].includes(status)
  ) {
    return 'waiting'
  }
  if (
    status &&
    [
      'failed',
      'revoked',
      'expired',
      'deleted',
      'offline',
      'forced_offline',
      'cancelled',
      'timed_out',
    ].includes(status)
  ) {
    return 'danger'
  }
  return 'neutral'
}
