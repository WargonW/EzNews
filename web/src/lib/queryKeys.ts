/**
 * TanStack Query 的 queryKey 工厂。
 *
 * 集中管理避免 key 拼写漂移 —— key 写错会导致缓存命中不上、
 * 表现为"数据莫名其妙不刷新"，这类 bug 极难排查。
 */

import type { Category } from '@/api/types';
import type { ListArticlesParams } from '@/api/endpoints';

/** 列表页查询参数（UI 侧的筛选态）。 */
export interface FeedFilters {
  category?: Category;
  sourceId?: number;
  q?: string;
  /** 仅在用户主动勾选"包含已停用源"时为 true。 */
  includeDisabled?: boolean;
  withAudio?: boolean;
}

/** articles 列表（首屏/翻页）。 */
export const articleKeys = {
  /** 根 key：所有文章相关查询的前缀，便于批量失效。 */
  all: ['articles'] as const,
  /** 列表：把 filters 编进 key，保证不同筛选互不串数据。 */
  list: (filters: FeedFilters) => ['articles', 'list', filters] as const,
  /** 详情。 */
  detail: (id: number) => ['articles', 'detail', id] as const,
  /** 分类计数。 */
  categories: ['articles', 'categories'] as const,
};

/** sources 源列表。 */
export const sourceKeys = {
  all: ['sources'] as const,
  list: (includeDisabled: boolean) => ['sources', 'list', includeDisabled] as const,
};

/** audio 合成任务。 */
export const audioKeys = {
  all: ['audio'] as const,
  /** 任务状态轮询。 */
  task: (taskId: number) => ['audio', 'task', taskId] as const,
  /** 文章在指定音色/语速下的音频元数据。 */
  articleAudio: (articleId: number, voice?: string, speed?: number) =>
    ['audio', 'article', articleId, voice ?? '', speed ?? ''] as const,
};

/** 账号相关。 */
export const meKeys = {
  all: ['me'] as const,
  profile: ['me', 'profile'] as const,
  preferences: ['me', 'preferences'] as const,
  sessions: ['me', 'sessions'] as const,
  favorites: ['me', 'favorites'] as const,
  reads: ['me', 'reads'] as const,
};

/** 管理后台（`/api/v1/admin/**`）。 */
export const adminKeys = {
  all: ['admin'] as const,
  overview: ['admin', 'overview'] as const,
  /** 用户列表：把筛选条件编进 key，避免不同条件串数据。 */
  users: (params: { q?: string; role?: string }) => ['admin', 'users', params] as const,
  contentStats: ['admin', 'stats', 'content'] as const,
  /** 语音任务列表：status 是筛选条件的一部分（含总数核算口径）。 */
  audioTasks: (status: string) => ['admin', 'audio-tasks', status] as const,
  system: ['admin', 'system'] as const,
};

/** 把 ListArticlesParams 转成 FeedFilters（供 key 与请求共用）。 */
export function filtersToParams(
  filters: FeedFilters,
  cursor: { pageCursor?: string; limit?: number },
): ListArticlesParams {
  return {
    category: filters.category,
    sourceId: filters.sourceId,
    q: filters.q?.trim() || undefined,
    includeDisabled: filters.includeDisabled,
    withAudio: filters.withAudio,
    pageCursor: cursor.pageCursor,
    limit: cursor.limit,
  };
}