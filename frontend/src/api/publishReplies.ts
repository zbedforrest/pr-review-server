import { apiGet } from './client';
import type { ReplyMode } from './settings';

export interface PublishReplies {
  mode: ReplyMode;
  total: number;
  by_action: Record<string, number>;
  unlinked_roots: number;
}

export async function fetchPublishReplies(): Promise<PublishReplies> {
  return apiGet<PublishReplies>('/api/publish/replies');
}
