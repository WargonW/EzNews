/**
 * 文章列表的数据层 hook。
 *
 * 把"首屏 + 翻页 + 增量合并 + 本地状态覆盖"这套复杂逻辑收敛在一处，
 * 让页面组件只负责渲染。
 *
 * ============================ 三个数据源如何合流 ============================
 * 1. **服务端首屏/翻页**（TanStack Query useInfiniteQuery）：`pageCursor` 翻页。
 * 2. **服务端增量**：见 lib/sync.ts，在回到前台时把新文章并进来。
 * 3. **本地状态**（游客收藏/已读 + 登录后服务端 isFavorited/isRead）：
 *    游客态根本没有服务端状态，所以本地 store 是唯一来源。
 *
 * 筛选放在服务端还是客户端？
 * - 分类 / 源筛选 / 搜索：**服务端参数**（category / sourceId / q），
 *   避免拉回大量无关数据；
 * - 侧栏的"隐藏某源"：**客户端过滤**（exclude 集合），因为它只是浏览偏好，
 *   放服务端会导致每次勾选都发一次请求（纯浪费）。
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useInfiniteQuery } from '@tanstack/react-query';
import { listArticles } from '@/api/endpoints';
import type { ArticleDTO } from '@/api/types';
import { articleKeys, filtersToParams, type FeedFilters } from '@/lib/queryKeys';
import { establishBaseline, mergeArticles } from '@/lib/sync';
import { useAuthStore } from '@/stores/auth';
import { favoriteIdsOf, readIdsOf, useGuestStore } from '@/stores/guest';

export interface UseArticleFeedResult {
  /** 已合并、去重、按 publishedAt DESC 排序后的列表。 */
  articles: ArticleDTO[];
  /** 首屏加载中（无任何数据）。 */
  isLoading: boolean;
  /** 翻页加载中。 */
  isFetchingMore: boolean;
  /** 是否还有下一页。 */
  hasMore: boolean;
  /** 首个错误。 */
  error: unknown;
  /** 加载下一页。 */
  loadMore: () => void;
  /** 手动刷新（重新拉首屏）。 */
  refresh: () => void;
  /** 当前是否有生效中的筛选（用于区分两种空态）。 */
  hasActiveFilters: boolean;
  /** 收藏/已读态查询。 */
  isFavorited: (id: number) => boolean;
  isRead: (id: number) => boolean;
  /** 增量同步拉到的文章数（用于"有 N 条新文章"提示）。 */
  freshCount: number;
  /** 把增量结果并入列表（幂等）。 */
  applyIncremental: (items: ArticleDTO[]) => void;
  /** 清除"新文章"提示。 */
  clearFresh: () => void;
}

/** 每页条数（openapi Limit：首屏默认 20，最大 100）。 */
const PAGE_SIZE = 20;

export function useArticleFeed(filters: FeedFilters, exclude: ReadonlySet<number>): UseArticleFeedResult {
  /** 增量同步新到的文章（合并进列表，不参与翻页）。 */
  const [freshArticles, setFreshArticles] = useState<ArticleDTO[]>([]);

  // 订阅原始 map（引用稳定），再用纯函数派生 id 集合 —— 见 stores/guest.ts 的说明。
  const favoritesMap = useGuestStore((s) => s.favorites);
  const readsMap = useGuestStore((s) => s.reads);
  const favorites = useMemo(() => favoriteIdsOf(favoritesMap), [favoritesMap]);
  const reads = useMemo(() => readIdsOf(readsMap), [readsMap]);
  const loggedIn = useAuthStore((s) => s.status === 'authenticated');

  const query = useInfiniteQuery({
    queryKey: articleKeys.list(filters),
    queryFn: ({ pageParam }) =>
      listArticles(filtersToParams(filters, { pageCursor: pageParam, limit: PAGE_SIZE })),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.nextPageCursor ?? undefined,
    // 30s 内不重复拉首屏（省流量）。
    staleTime: 30_000,
    // 窗口聚焦时**不**自动重拉：增量由 lib/sync 单通道负责，
    // 这里再自动重拉会造成双通道重复请求（正是我们要避免的流量浪费）。
    refetchOnWindowFocus: false,
  });

  /**
   * ★ 增量游标基线只能来自"不带 cursor 的首屏请求"。
   * openapi 明确：不传 cursor 时响应才带 `syncCursor`（服务端水位，now-2s 重叠）；
   * 增量模式响应的 `nextCursor` 语义是"这次增量读到哪了"，不能当新基线，
   * 否则会漏数据。
   */
  const syncCursor = query.data?.pages[0]?.syncCursor;
  useEffect(() => {
    if (syncCursor) establishBaseline(syncCursor);
  }, [syncCursor]);

  const serverArticles = useMemo(
    () => (query.data?.pages ?? []).flatMap((p) => p.items ?? []),
    [query.data],
  );

  const articles = useMemo(() => {
    // 增量结果与翻页结果按 id 合并（LWW：updatedAt 较新者胜出）。
    const merged = freshArticles.length > 0 ? mergeArticles(serverArticles, freshArticles) : serverArticles;
    // 客户端"隐藏源"过滤。
    return exclude.size === 0 ? merged : merged.filter((a) => !exclude.has(a.sourceId));
  }, [serverArticles, freshArticles, exclude]);

  const loadMore = useCallback(() => {
    if (query.hasNextPage && !query.isFetchingNextPage) void query.fetchNextPage();
  }, [query]);

  const refresh = useCallback(() => {
    setFreshArticles([]);
    void query.refetch();
  }, [query]);

  /**
   * 收藏态判定优先级：
   * 登录态且服务端给了 `isFavorited` → 以服务端为准（云端是唯一事实源）；
   * 否则读本地 store（游客态 / 服务端未返回该字段）。
   */
  const byId = useMemo(() => new Map(articles.map((a) => [a.id, a])), [articles]);

  const isFavorited = useCallback(
    (id: number): boolean => {
      const found = byId.get(id);
      if (loggedIn && found?.isFavorited !== undefined) return found.isFavorited;
      return favorites.has(id);
    },
    [byId, favorites, loggedIn],
  );

  const isRead = useCallback(
    (id: number): boolean => {
      const found = byId.get(id);
      if (loggedIn && found?.isRead !== undefined) return found.isRead;
      return reads.has(id);
    },
    [byId, reads, loggedIn],
  );

  const applyIncremental = useCallback((items: ArticleDTO[]) => {
    if (items.length === 0) return;
    setFreshArticles((prev) => mergeArticles(prev, items));
  }, []);

  const clearFresh = useCallback(() => setFreshArticles([]), []);

  // 切换筛选时清掉"新文章"标记：筛选语境下它已无意义。
  useEffect(() => {
    setFreshArticles([]);
  }, [filters.category, filters.sourceId, filters.q, filters.includeDisabled]);

  return {
    articles,
    isLoading: query.isLoading,
    isFetchingMore: query.isFetchingNextPage,
    hasMore: Boolean(query.hasNextPage),
    error: query.error,
    loadMore,
    refresh,
    hasActiveFilters: Boolean(filters.category || filters.sourceId || filters.q || filters.includeDisabled),
    isFavorited,
    isRead,
    freshCount: freshArticles.length,
    applyIncremental,
    clearFresh,
  };
}