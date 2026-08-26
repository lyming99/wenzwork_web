import { apiClient } from './client'
import type { components } from './schema'

export type AdminUser = components['schemas']['AdminUser']
export type AdminUserList = components['schemas']['AdminUserList']
export type CreateAdminUserRequest = components['schemas']['CreateAdminUserRequest']
export type SetAdminMembershipRequest = components['schemas']['SetAdminMembershipRequest']
export type RemoteAccessPolicySettings = components['schemas']['RemoteAccessPolicySettings']
export type UpdateRemoteAccessPolicyRequest =
  components['schemas']['UpdateRemoteAccessPolicyRequest']
export type AdminRelease = components['schemas']['AdminRelease']
export type SaveAdminReleaseRequest = components['schemas']['SaveAdminReleaseRequest']
export type SaveAdminReleaseAsset = components['schemas']['SaveAdminReleaseAsset']
export type StoredReleaseAsset = components['schemas']['StoredReleaseAsset']
export type ImportReleaseAssetRequest = components['schemas']['ImportReleaseAssetRequest']
export type GitHubReleaseImport = components['schemas']['GitHubReleaseImport']
export type MirrorReleaseImport = components['schemas']['MirrorReleaseImport']
export type ReleaseSourceSettings = components['schemas']['ReleaseSourceSettings']
export type UpdateReleaseSourceSettingsRequest =
  components['schemas']['UpdateReleaseSourceSettingsRequest']
export type ReleaseDeliverySettings = components['schemas']['ReleaseDeliverySettings']
export type UpdateReleaseDeliverySettingsRequest =
  components['schemas']['UpdateReleaseDeliverySettingsRequest']
export type ReleaseAccessKeySettings = components['schemas']['ReleaseAccessKeySettings']
export type UpdateReleaseAccessKeySettingsRequest =
  components['schemas']['UpdateReleaseAccessKeySettingsRequest']
export type ReleaseProject = 'web' | 'desktop' | 'mobile'
export type AdminPricingPlan = components['schemas']['AdminPricingPlan']
export type CreateAdminPricingPlanRequest = components['schemas']['CreateAdminPricingPlanRequest']
export type UpdateAdminPricingPlanRequest = components['schemas']['UpdateAdminPricingPlanRequest']
export type AdminHelpDocument = components['schemas']['AdminHelpDocument']
export type AdminHelpDocumentList = components['schemas']['AdminHelpDocumentList']
export type SaveHelpDocumentRequest = components['schemas']['SaveHelpDocumentRequest']
export type AdminFeedbackEntry = components['schemas']['AdminFeedbackEntry']
export type AdminFeedbackList = components['schemas']['AdminFeedbackList']
export type UpdateFeedbackRequest = components['schemas']['UpdateFeedbackRequest']

export const listAdminUsers = async (params: {
  q?: string
  status?: '' | 'pending' | 'active' | 'disabled'
  limit?: number
  offset?: number
}) => (await apiClient.get<AdminUserList>('/admin/users', { params })).data

export const createAdminUser = async (request: CreateAdminUserRequest) =>
  (await apiClient.post<{ user: AdminUser }>('/admin/users', request)).data.user

export const updateAdminUserStatus = async (userId: string, status: 'active' | 'disabled') =>
  (
    await apiClient.patch<{ user: AdminUser }>(
      `/admin/users/${encodeURIComponent(userId)}/status`,
      { status },
    )
  ).data.user

export const setAdminUserMembership = async (userId: string, request: SetAdminMembershipRequest) =>
  (
    await apiClient.put<{ membership: components['schemas']['Membership'] }>(
      `/admin/users/${encodeURIComponent(userId)}/membership`,
      request,
    )
  ).data.membership

export const cancelAdminUserMembership = async (userId: string) => {
  await apiClient.delete(`/admin/users/${encodeURIComponent(userId)}/membership`)
}

export const getRemoteAccessPolicy = async () =>
  (await apiClient.get<RemoteAccessPolicySettings>('/admin/remote-access-policy')).data

export const updateRemoteAccessPolicy = async (request: UpdateRemoteAccessPolicyRequest) =>
  (await apiClient.put<RemoteAccessPolicySettings>('/admin/remote-access-policy', request)).data

export const listAdminReleases = async () =>
  (await apiClient.get<{ items: AdminRelease[] }>('/admin/releases')).data.items

export type ReleaseAssetUploadRequest = {
  version: string
  platform: SaveAdminReleaseAsset['platform']
  architecture: 'x64' | 'arm64' | 'universal'
  fileName: string
  fileSizeBytes: number
  sha256: string
}

export const uploadReleaseAsset = async (
  request: ReleaseAssetUploadRequest,
  file: File,
  onProgress: (progress: number) => void,
) =>
  (
    await apiClient.post<StoredReleaseAsset>('/admin/release-assets/upload', file, {
      params: request,
      headers: { 'Content-Type': 'application/octet-stream' },
      timeout: 0,
      onUploadProgress: (event) => {
        if (event.total) onProgress(Math.round((event.loaded / event.total) * 100))
      },
    })
  ).data

export const importReleaseAsset = async (request: ImportReleaseAssetRequest) =>
  (
    await apiClient.post<StoredReleaseAsset>('/admin/release-assets/import', request, {
      timeout: 0,
    })
  ).data

export const getLatestGitHubRelease = async (project: ReleaseProject) =>
  (
    await apiClient.get<GitHubReleaseImport>('/admin/github-releases/latest', {
      params: { project },
    })
  ).data

export const importLatestMirrorRelease = async (project: ReleaseProject) =>
  (
    await apiClient.post<MirrorReleaseImport>('/admin/mirror-releases/latest/import', null, {
      params: { project },
      timeout: 0,
    })
  ).data

export const getReleaseSourceSettings = async () =>
  (await apiClient.get<{ items: ReleaseSourceSettings[] }>('/admin/release-source-settings')).data
    .items

export const updateReleaseSourceSettings = async (request: UpdateReleaseSourceSettingsRequest) =>
  (await apiClient.put<ReleaseSourceSettings>('/admin/release-source-settings', request)).data

export const getReleaseDeliverySettings = async () =>
  (await apiClient.get<ReleaseDeliverySettings>('/admin/release-delivery-settings')).data

export const updateReleaseDeliverySettings = async (
  request: UpdateReleaseDeliverySettingsRequest,
) =>
  (await apiClient.put<ReleaseDeliverySettings>('/admin/release-delivery-settings', request)).data

export const getReleaseAccessKeySettings = async () =>
  (await apiClient.get<ReleaseAccessKeySettings>('/admin/release-access-key-settings')).data

export const updateReleaseAccessKeySettings = async (
  request: UpdateReleaseAccessKeySettingsRequest,
) =>
  (await apiClient.put<ReleaseAccessKeySettings>('/admin/release-access-key-settings', request))
    .data

export const createAdminRelease = async (request: SaveAdminReleaseRequest) =>
  (await apiClient.post<{ release: AdminRelease }>('/admin/releases', request)).data.release

export const updateAdminRelease = async (releaseId: string, request: SaveAdminReleaseRequest) =>
  (
    await apiClient.put<{ release: AdminRelease }>(
      `/admin/releases/${encodeURIComponent(releaseId)}`,
      request,
    )
  ).data.release

export const publishAdminRelease = async (releaseId: string) =>
  (
    await apiClient.post<{ release: AdminRelease }>(
      `/admin/releases/${encodeURIComponent(releaseId)}/publish`,
    )
  ).data.release

export const withdrawAdminRelease = async (releaseId: string) => {
  await apiClient.delete(`/admin/releases/${encodeURIComponent(releaseId)}`)
}

export const deleteAdminRelease = async (releaseId: string) => {
  await apiClient.delete(`/admin/releases/${encodeURIComponent(releaseId)}/permanent`)
}

export const listAdminPricingPlans = async () =>
  (await apiClient.get<{ items: AdminPricingPlan[] }>('/admin/pricing-plans')).data.items

export const createAdminPricingPlan = async (request: CreateAdminPricingPlanRequest) =>
  (await apiClient.post<{ plan: AdminPricingPlan }>('/admin/pricing-plans', request)).data.plan

export const updateAdminPricingPlan = async (
  planId: string,
  request: UpdateAdminPricingPlanRequest,
) =>
  (
    await apiClient.put<{ plan: AdminPricingPlan }>(
      `/admin/pricing-plans/${encodeURIComponent(planId)}`,
      request,
    )
  ).data.plan

export const publishAdminPricingPlan = async (planId: string, expectedVersion: number) =>
  (
    await apiClient.post<{ plan: AdminPricingPlan }>(
      `/admin/pricing-plans/${encodeURIComponent(planId)}/publish`,
      { expectedVersion, confirm: true },
    )
  ).data.plan

export const archiveAdminPricingPlan = async (planId: string, expectedVersion: number) =>
  (
    await apiClient.post<{ plan: AdminPricingPlan }>(
      `/admin/pricing-plans/${encodeURIComponent(planId)}/archive`,
      { expectedVersion, confirm: true },
    )
  ).data.plan

export const listAdminHelpDocuments = async (params: {
  q?: string
  status?: '' | 'draft' | 'published' | 'archived'
  limit?: number
  offset?: number
}) => (await apiClient.get<AdminHelpDocumentList>('/admin/help-documents', { params })).data

export const getAdminHelpDocument = async (documentId: string) =>
  (
    await apiClient.get<{ document: AdminHelpDocument }>(
      `/admin/help-documents/${encodeURIComponent(documentId)}`,
    )
  ).data.document

export const createAdminHelpDocument = async (request: SaveHelpDocumentRequest) =>
  (await apiClient.post<{ document: AdminHelpDocument }>('/admin/help-documents', request)).data
    .document

export const updateAdminHelpDocument = async (
  documentId: string,
  request: SaveHelpDocumentRequest,
) =>
  (
    await apiClient.put<{ document: AdminHelpDocument }>(
      `/admin/help-documents/${encodeURIComponent(documentId)}`,
      request,
    )
  ).data.document

export const publishAdminHelpDocument = async (documentId: string) =>
  (
    await apiClient.post<{ document: AdminHelpDocument }>(
      `/admin/help-documents/${encodeURIComponent(documentId)}/publish`,
    )
  ).data.document

export const archiveAdminHelpDocument = async (documentId: string) => {
  await apiClient.delete(`/admin/help-documents/${encodeURIComponent(documentId)}`)
}

export const listAdminFeedback = async (params: {
  q?: string
  status?: '' | components['schemas']['FeedbackStatus']
  category?: '' | components['schemas']['FeedbackCategory']
  limit?: number
  offset?: number
}) => (await apiClient.get<AdminFeedbackList>('/admin/feedback', { params })).data

export const updateAdminFeedback = async (feedbackId: string, request: UpdateFeedbackRequest) =>
  (
    await apiClient.patch<{ feedback: AdminFeedbackEntry }>(
      `/admin/feedback/${encodeURIComponent(feedbackId)}`,
      request,
    )
  ).data.feedback
