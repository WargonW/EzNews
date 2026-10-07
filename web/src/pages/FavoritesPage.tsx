/**
 * 我的收藏页（路由 `/favorites`）。
 *
 * ============================ 数据源：游客 vs 登录 ============================
 * 两条路径的数据结构**本质不同**，必须分开处理，不能强行统一：
 *
 * 1. **已登录** → 数据源是 `GET /api/v1/me/favorites?withArticles=true`。
 *    服务端以**一次批量 `id IN (...)`** 查询为整页附加文章摘要
 *    （`title` / `summary` / `sourceName` / `publishedAt`），代价与页大小无关，
 *    因此这里**直接全量分页拉取**，无需再逐条 `GET /articles/{id}` 补齐。
 *    用 `nextCursor` + `hasMore` 翻页（openapi StatePage）。
 *
 * 2. **游客** → 收藏只存在于 `stores/guest.ts` 的 localStorage，
 *    而本地结构是 `LocalStateMap = {updatedAt, deleted}` ——
 *    ★ **只有 articleId 与墓碑态，根本没有标题/摘要**。
 *    而 `/me/favorites` 需要 Bearer，游客拿不到。
 *    所以游客路径仍然只能"从文章缓存里按 id 匹配"，
 *    但比旧实现多了一件事：**匹配不到的也照样列出来**（占位行），
 *    不再静默隐藏（静默隐藏会让人以为收藏丢了）。
 *
 * ============================ 两种"取不到内容"的情况 ============================
 * 服务端 4 个摘要字段全部 optional，两种截然不同的原因，必须区分文案：
 * - **文章已被清理**（登录态）：retention 清理（`retention.days`，默认 90 天）
 *   或源被删除 → article 行被级联物理删除。收藏行还在（墓碑语义要求保留），
 *   但标题已无从取回。这是**服务端事实**，用户无能为力，只能如实告知。
 * - **尚未加载到**（游客态）：文章可能只是在本地缓存里还没有，联网后自然出现。
 *
 * ============================ 墓碑（deleted=true） ============================
 * 取消收藏写的是墓碑而非物理删除（DEC-11），墓碑**必须**随增量流返回，
 * 否则其他设备永远收不到"对方已取消"，跨端删除无法同步。
 * 因此本页读到 `deleted=true` 的条目要**跳过渲染，但必须继续翻页**
 * —— 一页里全是墓碑是正常现象，不能因此中断或判定为"没有收藏"。
 */

import { useCallback, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query';
import { audioFileUrl, listArticles, listFavorites } from '@/api/endpoints';
import type { ArticleDTO, StateItem } from '@/api/types';
import { articleKeys, meKeys } from '@/lib/queryKeys';
import { formatRelative } from '@/lib/datetime';
import { useFavoriteActions } from '@/hooks/useFavoriteActions';
import { useAuthStore } from '@/stores/auth';
import { DEFAULT_PREFERENCES, favoriteIdsOf, useGuestStore } from '@/stores/guest';
import { usePlayerStore } from '@/stores/player';
import { ArticleListSkeleton, EmptyState, ErrorState, Toast } from '@/components/states';
import { AlertIcon, InboxIcon, PlayIcon, RefreshIcon, StarIcon } from '@/components/icons';

/** 登录态每页条数（openapi StateLimit：默认 200，最大 1000）。 */
const PAGE_SIZE = 100;

/** 游客态「加载完整列表」最多翻几页（limit=100，即最多 1000 篇）。 */
const MAX_PAGES = 10;

/**
 * 收藏行的统一视图模型。
 *
 * 登录态由 `StateItem`（服务端已附摘要）投影而来；游客态由本地
 * `favoriteIds` + 文章缓存合成。两者字段可空性一致，故可共用一套渲染。
 */
interface FavoriteRow {
  articleId: number;
  title?: string;
  summary?: string;
  sourceName?: string;
  /** ISO 8601 UTC。 */
  publishedAt?: string;
  /**
   * 内容不可用。`reason` 决定文案：
   * - `'cleaned'`：服务端已无此文章（retention 清理 / 源被删除），**不可恢复**；
   * - `'not-loaded'`：本地缓存里还没有，联网后可能出现。
   */
  unavailable?: 'cleaned' | 'not-loaded';
}

/** 把 StateItem 投影为视图模型：过滤墓碑，缺摘要的标记为 `cleaned`。 */
function toRow(item: StateItem): FavoriteRow | null {
  // 墓碑：跳过渲染，但调用方仍需继续翻页（不能中断整页解析）。
  if (item.deleted) return null;
  // 标题为空 ⟺ withArticles=false（本页不传它）或 article 行已被物理删除。
  // 本页恒传 withArticles=true，故只可能是后者 —— 不可恢复。
  const title = item.title?.trim();
  return {
    articleId: item.articleId,
    title,
    summary: item.summary?.trim() || undefined,
    sourceName: item.sourceName?.trim() || undefined,
    publishedAt: item.publishedAt,
    unavailable: title ? undefined : 'cleaned',
  };
}

export function FavoritesPage(): JSX.Element {
  const [toast, setToast] = useState<string | null>(null);
  const notify = useCallback((message: string) => setToast(message), []);
  const { toggleFavorite } = useFavoriteActions(notify);

  const favoritesMap = useGuestStore((s) => s.favorites);
  const favoriteIds = useMemo(() => favoriteIdsOf(favoritesMap), [favoritesMap]);
  const loggedIn = useAuthStore((s) => s.status === 'authenticated');

  /**
   * 刚被本机取消收藏的 id。
   *
   * 服务端列表来自 query 缓存，取消收藏后要等 mutation 成功 + 失效重取才会消失，
   * 期间那一行会"赖"在页面上。记录下来立即隐藏，让操作即时反馈。
   * 不能改成"按本地 store 过滤"：本地 store 在 `useUserStateSync` 跑完前是空的，
   * 那样会把整页都过滤掉。
   */
  const [justRemoved, setJustRemoved] = useState<ReadonlySet<number>>(() => new Set());

  const handleToggleFavorite = useCallback(
    (articleId: number) => {
      const nowFavorited = toggleFavorite(articleId);
      if (nowFavorited) {
        // 又收藏回来：从隐藏集合里移除。
        setJustRemoved((prev) => {
          if (!prev.has(articleId)) return prev;
          const next = new Set(prev);
          next.delete(articleId);
          return next;
        });
        return;
      }
      setJustRemoved((prev) => new Set(prev).add(articleId));
    },
    [toggleFavorite],
  );

  /* ======================================================================== *
   * 登录态：云端分页
   * ======================================================================== */
  const cloudQuery = useInfiniteQuery({
    // 游客态不发这个请求：/me/favorites 需要 Bearer，且没有"游客的云端收藏"。
    queryKey: [...meKeys.favorites, 'feed'],
    queryFn: ({ pageParam }) =>
      listFavorites({ since: pageParam, limit: PAGE_SIZE, withArticles: true }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) =>
      // ★ 墓碑已在 toRow 里过滤，但翻页判定必须基于**原始响应**：
      //   一页可能整页都是墓碑，若用"本页剩 0 条"来判定就会误判为到底。
      lastPage.hasMore && lastPage.nextCursor ? lastPage.nextCursor : undefined,
    enabled: loggedIn,
    staleTime: 30_000,
    refetchOnWindowFocus: false,
  });

  const cloudRows = useMemo(() => {
    const pages = cloudQuery.data?.pages ?? [];
    const rows: FavoriteRow[] = [];
    for (const page of pages) {
      for (const item of page.items ?? []) {
        const row = toRow(item);
        if (row) rows.push(row);
      }
    }
    // 同一 articleId 在多页里重复出现时，保留信息更全的那条
    //（墓碑可能出现在后续页，把先前的正向记录覆盖掉）。
    const byId = new Map<number, FavoriteRow>();
    for (const row of rows) {
      const prev = byId.get(row.articleId);
      byId.set(row.articleId, !prev || (!prev.title && row.title) ? row : prev);
    }
    return [...byId.values()];
  }, [cloudQuery.data]);

  /* ======================================================================== *
   * 游客态：本地 id + 文章缓存
   * ======================================================================== */
  const queryClient = useQueryClient();

  /**
   * 已缓存列表里命中的收藏文章。
   *
   * `getQueryData` 是快照读取：不会订阅后续变化。
   * 只读首页的**无筛选**缓存 key —— 首页若有筛选，缓存 key 也不同（见 articleKeys.list）。
   */
  const cached = useMemo(() => {
    const q = queryClient.getQueryData<{ pages: ArticleDTO[][] }>(
      articleKeys.list({ withAudio: true }),
    );
    return (q?.pages ?? []).flat();
  }, [queryClient]);

  /** 兜底全量拉取的结果（游客主动触发）。 */
  const [loaded, setLoaded] = useState<ArticleDTO[] | null>(null);
  const [loadingAll, setLoadingAll] = useState(false);
  const [loadAllError, setLoadAllError] = useState<unknown>(null);

  const loadAll = useCallback(async () => {
    setLoadingAll(true);
    setLoadAllError(null);
    try {
      const collected: ArticleDTO[] = [];
      let pageCursor: string | undefined;
      for (let page = 0; page < MAX_PAGES; page += 1) {
        const res = await listArticles({ pageCursor, limit: 100, withAudio: true });
        collected.push(...(res.items ?? []));
        pageCursor = res.nextPageCursor ?? undefined;
        if (!pageCursor) break;
      }
      setLoaded(collected);
    } catch (err) {
      setLoadAllError(err);
    } finally {
      setLoadingAll(false);
    }
  }, []);

  const guestRows = useMemo<FavoriteRow[]>(() => {
    if (loggedIn) return [];
    const source = loaded ?? cached;
    const byId = new Map(source.map((a) => [a.id, a]));
    return [...favoriteIds].map((id) => {
      const a = byId.get(id);
      if (!a) {
        // 本地记着收藏，但缓存里没有这篇文章。**如实列出**，
        // 联网后或浏览到该文章时会自动补上标题。
        return { articleId: id, unavailable: 'not-loaded' as const };
      }
      return {
        articleId: a.id,
        title: a.title,
        summary: a.summary || undefined,
        sourceName: a.sourceName,
        publishedAt: a.publishedAt,
      };
    });
  }, [loggedIn, loaded, cached, favoriteIds]);

  const rows = loggedIn ? cloudRows : guestRows;
  const visibleRows = useMemo(
    () => rows.filter((r) => !justRemoved.has(r.articleId)),
    [rows, justRemoved],
  );

  /** 音频播放：仅对本地缓存里能查到完整 ArticleDTO 的条目可用（游客态专属能力）。 */
  const cachedById = useMemo(() => {
    const source = loaded ?? cached;
    return new Map(source.map((a) => [a.id, a]));
  }, [loaded, cached]);

  const handlePlay = useCallback((article: ArticleDTO) => {
    const audioId = article.audio?.audioId;
    if (article.audio?.status !== 'ready' || audioId === null || audioId === undefined) return;
    usePlayerStore.getState().play(
      [
        {
          articleId: article.id,
          title: article.title,
          url: audioFileUrl(audioId),
          voice: DEFAULT_PREFERENCES['tts.voice'],
          speed: 1,
        },
      ],
      0,
    );
  }, []);

  /* ---------- 游客态完全没有收藏：干净的空态 ---------- */
  if (!loggedIn && favoriteIds.size === 0) {
    return (
      <div className="mx-auto max-w-2xl px-4 py-16">
        <EmptyState
          icon={<StarIcon className="text-2xl" />}
          title="还没有收藏"
          description="点击文章卡片右上角的星标即可收藏。收藏保存在本机浏览器里，登录后会自动同步到你的账号。"
          action={
            <Link to="/" className="ez-btn ez-btn-md ez-btn-primary">
              去浏览新闻
            </Link>
          }
        />
      </div>
    );
  }

  const isInitialLoading = loggedIn ? cloudQuery.isLoading : loadingAll && rows.length === 0;
  const error = loggedIn ? cloudQuery.error : loadAllError;
  const hasMore = loggedIn ? Boolean(cloudQuery.hasNextPage) : false;
  const isFetchingMore = loggedIn ? cloudQuery.isFetchingNextPage : false;
  const unavailableCount = visibleRows.filter((r) => r.unavailable).length;

  return (
    <div className="mx-auto max-w-3xl px-4 py-8 sm:px-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-bold tracking-tight text-ink">我的收藏</h1>
          <p className="mt-1 text-[12px] text-ink-muted">
            {loggedIn
              ? `共 ${visibleRows.length} 条 · 已同步到账号`
              : `共 ${favoriteIds.size} 条 · 保存在本机`}
          </p>
        </div>
        {loggedIn ? (
          <button
            type="button"
            onClick={() => void cloudQuery.refetch()}
            disabled={cloudQuery.isFetching}
            className="ez-btn ez-btn-md ez-btn-secondary"
          >
            <RefreshIcon />
            {cloudQuery.isFetching ? '刷新中…' : '刷新'}
          </button>
        ) : (
          <button
            type="button"
            onClick={() => void loadAll()}
            disabled={loadingAll}
            className="ez-btn ez-btn-md ez-btn-secondary"
          >
            <RefreshIcon />
            {loadingAll ? '加载中…' : '加载完整列表'}
          </button>
        )}
      </div>

      {error ? (
        <div className="mt-4">
          <ErrorState
            error={error}
            onRetry={() => {
              if (loggedIn) void cloudQuery.refetch();
              else void loadAll();
            }}
            compact
          />
        </div>
      ) : null}

      {isInitialLoading ? (
        <div className="mt-6">
          <ArticleListSkeleton count={4} />
        </div>
      ) : visibleRows.length === 0 ? (
        <div className="mt-6">
          <EmptyState
            compact
            icon={loggedIn ? <StarIcon className="text-xl" /> : <InboxIcon className="text-xl" />}
            title="还没有收藏"
            description="点击文章卡片右上角的星标即可收藏。"
            action={
              <Link to="/" className="ez-btn ez-btn-md ez-btn-secondary">
                去浏览新闻
              </Link>
            }
          />
        </div>
      ) : (
        <>
          <ul className="mt-5 space-y-3">
            {visibleRows.map((row) => (
              <li key={row.articleId} className="ez-enter">
                <FavoriteRowCard
                  row={row}
                  onToggleFavorite={handleToggleFavorite}
                  playArticle={loggedIn ? undefined : cachedById.get(row.articleId)}
                  onPlay={handlePlay}
                />
              </li>
            ))}
          </ul>

          {/* 如实告知"有几条内容已不可用"，但不夸大：登录态是服务端清理，游客态只是没加载到 */}
          {unavailableCount > 0 ? (
            <div className="mt-4 rounded-lg border border-line bg-surface-sunken p-3 text-[12px] leading-relaxed text-ink-muted">
              {loggedIn ? (
                <>
                  有 {unavailableCount} 条收藏的文章内容已不可用 —— 文章已被清理或来源已被删除，
                  服务端已无法返回标题与摘要。这类收藏记录仍会保留。
                </>
              ) : (
                <>
                  有 {unavailableCount} 条收藏还没加载到内容。游客态的收藏只存在本机（仅有文章编号），
                  点上方「加载完整列表」可从服务端抓取一次。
                </>
              )}
            </div>
          ) : null}

          {hasMore ? (
            <div className="py-6 text-center">
              {isFetchingMore ? (
                <span className="inline-flex items-center gap-2 text-[13px] text-ink-muted">
                  <span className="h-3.5 w-3.5 animate-spin rounded-full border-2 border-slate-300 border-t-brand" />
                  加载中…
                </span>
              ) : (
                <button
                  type="button"
                  onClick={() => void cloudQuery.fetchNextPage()}
                  className="ez-btn ez-btn-md ez-btn-secondary"
                >
                  加载更多
                </button>
              )}
            </div>
          ) : (
            <p className="py-8 text-center text-[12px] text-ink-faint">
              已经到底了 · 共 {visibleRows.length} 条
            </p>
          )}
        </>
      )}

      {toast ? (
        <div className="pointer-events-none fixed bottom-20 left-1/2 z-50 w-full max-w-sm -translate-x-1/2 px-4">
          <Toast message={toast} onClose={() => setToast(null)} />
        </div>
      ) : null}
    </div>
  );
}

/**
 * 单条收藏行。
 *
 * 刻意**不复用** `ArticleCard`：它要求完整 `ArticleDTO`
 * （`category` / `imageUrl` / `url` / `audio` 等），
 * 而 `StateItem` 只有 4 个摘要字段。硬凑就得臆造契约外字段，
 * 那些字段只能填假值，属于典型的"看起来能跑、实际骗人"。
 *
 * ★ 原文链接：StateItem **没有 url 字段**（服务端契约如此），
 *   所以不给"跳原文"链接，只给站内详情页 —— 详情页自己去拉文章。
 *   内容已不可用的行连详情页都不给（点进去必然 404）。
 */
function FavoriteRowCard({
  row,
  onToggleFavorite,
  playArticle,
  onPlay,
}: {
  row: FavoriteRow;
  onToggleFavorite: (articleId: number) => void;
  /** 仅游客态可能提供：命中的本地 ArticleDTO，用于"播放语音"。 */
  playArticle?: ArticleDTO;
  onPlay: (article: ArticleDTO) => void;
}): JSX.Element {
  const hasAudio = playArticle?.audio?.status === 'ready' && playArticle.audio.audioId != null;

  return (
    <article className="ez-card ez-card-interactive group relative p-4">
      <div className="flex gap-3">
        <div className="min-w-0 flex-1">
          {row.unavailable ? (
            <>
              <h3 className="flex items-center gap-1.5 text-[15px] font-semibold leading-snug text-ink-faint">
                <AlertIcon className="shrink-0 text-sm" />
                {row.unavailable === 'cleaned' ? '文章已删除或内容已清理' : '尚未加载到这篇文章'}
              </h3>
              <p className="mt-1.5 text-[13px] leading-relaxed text-ink-muted">
                {row.unavailable === 'cleaned'
                  ? '服务端已无法返回它的标题与摘要（文章可能已被清理或来源已被删除）。'
                  : '联网后浏览到该文章时会自动补上标题与摘要。'}
                <span className="ml-1 font-mono text-[11px] text-ink-faint">
                  #{row.articleId}
                </span>
              </p>
            </>
          ) : (
            <>
              <h3 className="ez-line-clamp-2 text-[15px] font-semibold leading-snug text-ink">
                {/* 内容可用时给站内详情页链接（StateItem 无 url，无法给原文链接） */}
                <Link to={`/article/${row.articleId}`} className="hover:text-brand">
                  {row.title}
                </Link>
              </h3>

              {row.summary ? (
                <p className="ez-line-clamp-2 mt-1.5 text-[13px] leading-relaxed text-ink-muted">
                  {row.summary}
                </p>
              ) : null}

              <div className="mt-2.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-[11px] text-ink-faint">
                {row.sourceName ? <span className="ez-badge bg-brand-soft text-brand">{row.sourceName}</span> : null}
                {row.publishedAt ? (
                  <time dateTime={row.publishedAt}>{formatRelative(row.publishedAt)}</time>
                ) : null}
              </div>
            </>
          )}
        </div>

        <div className="absolute right-3 top-3 flex items-center gap-1 opacity-0 transition-opacity focus-within:opacity-100 group-hover:opacity-100">
          {hasAudio && playArticle ? (
            <button
              type="button"
              onClick={() => onPlay(playArticle)}
              className="ez-btn ez-btn-sm h-8 w-8 rounded-lg bg-surface-raised/90 p-0 text-ink-muted shadow-card backdrop-blur hover:text-brand"
              title="播放语音"
              aria-label={`播放《${playArticle.title}》的语音`}
            >
              <PlayIcon className="text-xs" />
            </button>
          ) : null}
          <button
            type="button"
            onClick={() => onToggleFavorite(row.articleId)}
            className="ez-btn ez-btn-sm h-8 w-8 rounded-lg bg-surface-raised/90 p-0 text-amber-500 shadow-card backdrop-blur"
            title="取消收藏"
            aria-label="取消收藏"
            aria-pressed
          >
            <StarIcon filled className="text-sm" />
          </button>
        </div>
      </div>
    </article>
  );
}