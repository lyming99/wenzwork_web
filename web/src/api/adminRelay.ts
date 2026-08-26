import { apiClient } from './client'
import type { components } from './schema'

export type RelayTopology = components['schemas']['RelayTopologyList']
export type RelayRegion = components['schemas']['RelayTopologyRegion']
export type RelayPool = components['schemas']['RelayTopologyPool']
export type RelayCell = components['schemas']['RelayTopologyCell']
export type RelayInstallation = components['schemas']['RelayInstallation']
export type RelayNodeInstance = components['schemas']['RelayNodeInstance']
export type RelayDeploymentChecklist = components['schemas']['RelayDeploymentChecklist']
export type RelayServerArtifact = Omit<components['schemas']['RelayServerArtifact'], 'id'> & {
  id?: string
  objectKey: string
}
export type RelayServerRelease = Omit<
  components['schemas']['RelayServerRelease'],
  'status' | 'artifacts'
> & {
  status: 'draft' | 'published' | 'retired' | 'revoked'
  artifacts: RelayServerArtifact[]
}
export type SaveRelayServerReleaseRequest = Omit<
  RelayServerRelease,
  'id' | 'status' | 'artifacts'
> & {
  artifacts: Array<Omit<RelayServerArtifact, 'id'>>
}
export type CreateRelayInstallationRequest = components['schemas']['CreateRelayInstallationRequest']
export type UpdateRelayInstallationRequest = components['schemas']['UpdateRelayInstallationRequest']
export type RelayEnrollmentToken = components['schemas']['RelayEnrollmentToken']
export type RelayAccessKey = components['schemas']['RelayAccessKey']
export type RelayInstallSessionResponse = components['schemas']['RelayInstallSessionResponse']
export type RelayManagedEndpoint = components['schemas']['RelayManagedEndpoint']
export type RelayNode = components['schemas']['RelayNode']
export type RelayAssignment = components['schemas']['RelayAssignment']
export type RelayOperation = components['schemas']['AsyncOperation']
export type UpdateRelayCellRequest = components['schemas']['UpdateRelayCellRequest']
export type CreateRelayEndpointRequest = components['schemas']['CreateRelayEndpointRequest']

let idempotencySequence = 0
const idempotencyHeaders = () => ({
  'Idempotency-Key':
    globalThis.crypto?.randomUUID?.() ?? `relay-web-${Date.now()}-${++idempotencySequence}`,
})

export const listRelayTopology = async () =>
  (await apiClient.get<RelayTopology>('/admin/relay/regions')).data

export const listRelayInstallations = async () =>
  (
    await apiClient.get<components['schemas']['RelayInstallationList']>(
      '/admin/relay/node-installations',
    )
  ).data.items

export const getRelayInstallation = async (installationId: string) =>
  (
    await apiClient.get<RelayInstallation>(
      `/admin/relay/node-installations/${encodeURIComponent(installationId)}`,
    )
  ).data

export const createRelayInstallation = async (request: CreateRelayInstallationRequest) =>
  (await apiClient.post<RelayInstallation>('/admin/relay/node-installations', request)).data

export const updateRelayInstallation = async (
  installationId: string,
  request: UpdateRelayInstallationRequest,
) =>
  (
    await apiClient.patch<RelayInstallation>(
      `/admin/relay/node-installations/${encodeURIComponent(installationId)}`,
      request,
    )
  ).data

export const deleteRelayInstallation = async (installationId: string) => {
  await apiClient.delete(`/admin/relay/node-installations/${encodeURIComponent(installationId)}`)
}

export const createRelayEnrollmentToken = async (installationId: string) =>
  (
    await apiClient.post<RelayEnrollmentToken>(
      `/admin/relay/node-installations/${encodeURIComponent(installationId)}/enrollment-tokens`,
    )
  ).data

export const createRelayAccessKey = async (installationId: string) =>
  (
    await apiClient.post<RelayAccessKey>(
      `/admin/relay/node-installations/${encodeURIComponent(installationId)}/access-keys`,
    )
  ).data

export const createRelayInstallSession = async (
  installationId: string,
  releaseId: string,
  mode: 'download' | 'script' | 'manual',
  action: 'install' | 'upgrade' = 'install',
) =>
  (
    await apiClient.post<RelayInstallSessionResponse>(
      `/admin/relay/node-installations/${encodeURIComponent(installationId)}/install-sessions`,
      { releaseId, mode, action },
    )
  ).data

export const activateRelayInstallation = async (
  installationId: string,
  expectedThumbprint: string,
  deploymentChecklist: RelayDeploymentChecklist,
) =>
  (
    await apiClient.post<RelayInstallation>(
      `/admin/relay/node-installations/${encodeURIComponent(installationId)}/activations`,
      {
        expectedThumbprint,
        deploymentChecklist,
        confirmation: 'activate_relay_installation',
      },
    )
  ).data

export const revokeRelayInstallation = async (installationId: string) => {
  await apiClient.post(
    `/admin/relay/node-installations/${encodeURIComponent(installationId)}/revocations`,
    { confirmation: 'revoke_relay_installation' },
  )
}

export const listRelayServerReleases = async () =>
  (await apiClient.get<{ items: RelayServerRelease[] }>('/admin/relay/releases')).data.items

export const createRelayServerRelease = async (request: SaveRelayServerReleaseRequest) =>
  (await apiClient.post<RelayServerRelease>('/admin/relay/releases', request)).data

export const updateRelayServerRelease = async (
  releaseId: string,
  request: SaveRelayServerReleaseRequest,
) =>
  (
    await apiClient.put<RelayServerRelease>(
      `/admin/relay/releases/${encodeURIComponent(releaseId)}`,
      request,
    )
  ).data

export const publishRelayServerRelease = async (releaseId: string) =>
  (
    await apiClient.post<RelayServerRelease>(
      `/admin/relay/releases/${encodeURIComponent(releaseId)}/publications`,
    )
  ).data

export const retireRelayServerRelease = async (releaseId: string) =>
  (
    await apiClient.post<RelayServerRelease>(
      `/admin/relay/releases/${encodeURIComponent(releaseId)}/retirements`,
    )
  ).data

export const deleteRelayServerRelease = async (releaseId: string) => {
  await apiClient.delete(`/admin/relay/releases/${encodeURIComponent(releaseId)}`)
}

export const listRelayCellNodes = async (cellId: string) =>
  (
    await apiClient.get<components['schemas']['RelayNodeList']>(
      `/admin/relay/cells/${encodeURIComponent(cellId)}/nodes`,
    )
  ).data

export const listRelayCellEndpoints = async (cellId: string) =>
  (
    await apiClient.get<components['schemas']['RelayManagedEndpointList']>(
      `/admin/relay/cells/${encodeURIComponent(cellId)}/endpoints`,
    )
  ).data.items

export const updateRelayCell = async (cellId: string, request: UpdateRelayCellRequest) =>
  (
    await apiClient.patch<components['schemas']['AsyncOperationResponse']>(
      `/admin/relay/cells/${encodeURIComponent(cellId)}`,
      request,
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const createRelayEndpoint = async (cellId: string, request: CreateRelayEndpointRequest) =>
  (
    await apiClient.post<components['schemas']['AsyncOperationResponse']>(
      `/admin/relay/cells/${encodeURIComponent(cellId)}/endpoints`,
      request,
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const updateRelayEndpoint = async (
  endpointId: string,
  request: CreateRelayEndpointRequest,
) =>
  (
    await apiClient.patch<components['schemas']['AsyncOperationResponse']>(
      `/admin/relay/endpoints/${encodeURIComponent(endpointId)}`,
      request,
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const validateRelayEndpoint = async (endpointId: string) =>
  (
    await apiClient.post<components['schemas']['AsyncOperationResponse']>(
      `/admin/relay/endpoints/${encodeURIComponent(endpointId)}/validations`,
      undefined,
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const activateRelayEndpoint = async (endpointId: string) =>
  (
    await apiClient.post<components['schemas']['AsyncOperationResponse']>(
      `/admin/relay/endpoints/${encodeURIComponent(endpointId)}/activations`,
      { confirmation: 'activate_relay_endpoint' },
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const drainRelayNode = async (nodeId: string) =>
  (
    await apiClient.post<components['schemas']['AsyncOperationResponse']>(
      `/admin/relay/nodes/${encodeURIComponent(nodeId)}/drain-operations`,
      { confirmation: 'drain_relay_node' },
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const drainRelayCell = async (cellId: string) =>
  (
    await apiClient.post<components['schemas']['AsyncOperationResponse']>(
      `/admin/relay/cells/${encodeURIComponent(cellId)}/drain-operations`,
      { confirmation: 'drain_relay_cell' },
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const listRelayAssignments = async (userId: string) =>
  (
    await apiClient.get<components['schemas']['RelayAssignmentList']>('/admin/relay/assignments', {
      params: { userId },
    })
  ).data.items

export const migrateRelayUser = async (
  userId: string,
  mode: 'auto' | 'pinned',
  targetCellId?: string,
) =>
  (
    await apiClient.post<components['schemas']['AsyncOperationResponse']>(
      `/admin/relay/users/${encodeURIComponent(userId)}/migration-operations`,
      {
        mode,
        targetCellId: mode === 'pinned' ? targetCellId : null,
        confirmation: 'migrate_relay_user',
      },
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const unpinRelayUser = async (userId: string) =>
  (
    await apiClient.delete<components['schemas']['AsyncOperationResponse']>(
      `/admin/relay/users/${encodeURIComponent(userId)}/pin`,
      { headers: idempotencyHeaders() },
    )
  ).data.operation

export const getRelayOperation = async (operationId: string) =>
  (
    await apiClient.get<components['schemas']['AsyncOperationResponse']>(
      `/admin/relay/operations/${encodeURIComponent(operationId)}`,
    )
  ).data.operation
