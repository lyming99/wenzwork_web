import { describe, expect, it } from 'vitest'

import {
  RELAY_ASSIGNMENT_STATUSES,
  RELAY_CELL_STATUSES,
  RELAY_ENDPOINT_STATUSES,
  RELAY_INSTALLATION_STATUSES,
  RELAY_INSTANCE_STATUSES,
  RELAY_OPERATION_ITEM_STATUSES,
  RELAY_OPERATION_LABELS,
  RELAY_OPERATION_STATUSES,
  RELAY_STATUS_LABELS,
  RELAY_TERMINOLOGY,
  relayOperationLabel,
  relayStatusLabel,
} from './relayTerminology'

describe('中继系统中文术语映射', () => {
  it('覆盖开发计划冻结的状态且每个状态使用唯一中文名称', () => {
    const plannedStatuses = {
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
      running: '正在执行',
      succeeded: '操作成功',
      cancelled: '已取消',
      timed_out: '操作超时',
    }

    expect(RELAY_STATUS_LABELS).toMatchObject(plannedStatuses)
    const labels = Object.values(RELAY_STATUS_LABELS)
    expect(new Set(labels).size).toBe(labels.length)
  })

  it('覆盖页面使用的中继组、地址、节点、归属和操作状态', () => {
    const statusGroups = [
      RELAY_CELL_STATUSES,
      RELAY_ENDPOINT_STATUSES,
      RELAY_INSTALLATION_STATUSES,
      RELAY_INSTANCE_STATUSES,
      RELAY_ASSIGNMENT_STATUSES,
      RELAY_OPERATION_STATUSES,
      RELAY_OPERATION_ITEM_STATUSES,
    ]

    for (const status of statusGroups.flat()) {
      expect(relayStatusLabel(status)).not.toBe('未知状态')
    }
  })

  it('冻结核心对象边界和异步操作名称', () => {
    expect(RELAY_TERMINOLOGY.cell.label).toBe('中继组')
    expect(RELAY_TERMINOLOGY.node.label).toBe('中继节点')
    expect(RELAY_TERMINOLOGY.installation.label).toBe('节点安装记录')
    expect(RELAY_TERMINOLOGY.instance.label).toBe('运行实例')
    expect(RELAY_TERMINOLOGY.installation.description).toContain('重启后保持不变')
    expect(RELAY_TERMINOLOGY.instance.description).toContain('重启后重新生成')
    expect(RELAY_TERMINOLOGY.accessKey.description).toContain('可随时吊销')

    for (const operation of Object.keys(RELAY_OPERATION_LABELS)) {
      expect(relayOperationLabel(operation)).not.toBe('中继操作')
    }
  })
})
