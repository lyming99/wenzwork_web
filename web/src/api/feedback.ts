import { apiClient } from './client'
import type { components } from './schema'

export type FeedbackEntry = components['schemas']['FeedbackEntry']
export type CreateFeedbackRequest = components['schemas']['CreateFeedbackRequest']

export const listMyFeedback = async () =>
  (await apiClient.get<components['schemas']['FeedbackList']>('/me/feedback')).data.items

export const createMyFeedback = async (request: CreateFeedbackRequest) =>
  (await apiClient.post<components['schemas']['FeedbackResponse']>('/me/feedback', request)).data
    .feedback
