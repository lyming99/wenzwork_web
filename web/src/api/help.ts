import { apiClient } from './client'
import type { components } from './schema'

export type HelpDocumentSummary = components['schemas']['HelpDocumentSummary']
export type PublicHelpDocument = components['schemas']['PublicHelpDocument']

export const listPublishedHelpDocuments = async () =>
  (await apiClient.get<components['schemas']['HelpDocumentList']>('/help-documents')).data.items

export const getPublishedHelpDocument = async (slug: string) =>
  (
    await apiClient.get<components['schemas']['PublicHelpDocumentResponse']>(
      `/help-documents/${encodeURIComponent(slug)}`,
    )
  ).data.document
