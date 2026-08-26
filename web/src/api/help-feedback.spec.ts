import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  archiveAdminHelpDocument,
  createAdminHelpDocument,
  listAdminFeedback,
  listAdminHelpDocuments,
  publishAdminHelpDocument,
  publishAdminRelease,
  updateAdminFeedback,
  updateAdminHelpDocument,
} from './admin'
import { apiClient } from './client'
import { createMyFeedback, listMyFeedback } from './feedback'
import { getPublishedHelpDocument, listPublishedHelpDocuments } from './help'

vi.mock('./client', () => ({
  apiClient: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), delete: vi.fn() },
}))

describe('help and feedback API clients', () => {
  beforeEach(() => vi.clearAllMocks())

  it('loads public static help snapshots and encodes article paths', async () => {
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: { items: [{ slug: 'managed' }] } })
    await expect(listPublishedHelpDocuments()).resolves.toEqual([{ slug: 'managed' }])
    expect(apiClient.get).toHaveBeenCalledWith('/help-documents')

    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: { document: { slug: 'a/b' } } })
    await getPublishedHelpDocument('a/b')
    expect(apiClient.get).toHaveBeenCalledWith('/help-documents/a%2Fb')
  })

  it('submits member feedback and loads only the current member history', async () => {
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: { items: [] } })
    await expect(listMyFeedback()).resolves.toEqual([])

    const request = { category: 'bug' as const, subject: 'Issue', content: 'Details' }
    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { feedback: { id: 'feedback-1' } } })
    await createMyFeedback(request)
    expect(apiClient.post).toHaveBeenCalledWith('/me/feedback', request)
  })

  it('supports document draft, explicit publication, archive, and feedback management', async () => {
    const documentRequest = {
      slug: 'managed',
      title: 'Managed',
      description: '',
      category: 'Basics',
      sortOrder: 1,
      contentMarkdown: '# Managed',
    }
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: { items: [], total: 0 } })
    await listAdminHelpDocuments({ status: 'draft' })
    expect(apiClient.get).toHaveBeenCalledWith('/admin/help-documents', {
      params: { status: 'draft' },
    })

    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { document: { id: 'doc-1' } } })
    await createAdminHelpDocument(documentRequest)
    vi.mocked(apiClient.put).mockResolvedValueOnce({ data: { document: { id: 'doc-1' } } })
    await updateAdminHelpDocument('doc/1', { ...documentRequest, version: 1 })
    expect(apiClient.put).toHaveBeenCalledWith('/admin/help-documents/doc%2F1', {
      ...documentRequest,
      version: 1,
    })

    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { document: { id: 'doc-1' } } })
    await publishAdminHelpDocument('doc/1')
    expect(apiClient.post).toHaveBeenCalledWith('/admin/help-documents/doc%2F1/publish')
    vi.mocked(apiClient.delete).mockResolvedValueOnce({ data: null })
    await archiveAdminHelpDocument('doc/1')

    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: { items: [], total: 0 } })
    await listAdminFeedback({ status: 'pending' })
    vi.mocked(apiClient.patch).mockResolvedValueOnce({ data: { feedback: { id: 'feedback-1' } } })
    await updateAdminFeedback('feedback/1', {
      status: 'resolved',
      adminReply: 'Fixed',
      internalNote: 'QA',
    })
    expect(apiClient.patch).toHaveBeenCalledWith('/admin/feedback/feedback%2F1', {
      status: 'resolved',
      adminReply: 'Fixed',
      internalNote: 'QA',
    })

    vi.mocked(apiClient.post).mockResolvedValueOnce({ data: { release: { id: 'release-1' } } })
    await publishAdminRelease('release/1')
    expect(apiClient.post).toHaveBeenCalledWith('/admin/releases/release%2F1/publish')
  })
})
