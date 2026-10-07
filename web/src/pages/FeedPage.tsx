/**
 * 首页：文章流（主区）+ 源筛选（侧栏）。
 *
 * PRD §8.1 桌面布局：顶栏 / 左侧栏（源）/ 右侧卡片流 / 底部播放器。
 * 响应式：<1024px 侧栏收折为抽屉；<768px 搜索与筛选收敛为顶部入口。
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useSearchParams } from 'react-router-dom';
import { listSources, audioFileUrl } from '@/api/endpoints';
import type { ArticleDTO, Category } from '@/api/types';
import { sourceKeys, type FeedFilters } from '@/lib/queryKeys';
import { runIncrementalSync, fetchIncrementalPage, writeArticleCursor } from '@/lib/sync';
import { useArticleFeed } from '@/hooks/useArticleFeed';
import { useFavoriteActions } from '@/hooks/useFavoriteActions';
import { useMarkAllRead } from '@/hooks/useMarkAllRead';
import { useOnlineStatus } from '@/hooks/useOnlineStatus';
import { selectCurrentTrack, usePlayerStore, type PlayerTrack } from '@/stores/player';
import { DEFAULT_PREFERENCES, prefIdSet, prefNumber, prefString, serializeIdSet, useGuestStore } from '@/stores/guest';
import { AudioPlayerBar } from '@/components/AudioPlayerBar';
import { ArticleCard, ListFooter } from '@/components/ArticleCard';
import { Sidebar } from '@/components/Sidebar';
import { ArticleListSkeleton, EmptyState, ErrorState, NoSearchResults, OfflineBanner, Toast } from '@/components/states';
import { CloseIcon, CheckAllIcon, FilterIcon, StarIcon } from '@/components/icons';

export function FeedPage({
  onOpenManage,
  drawerOpen,
  onDrawerChange,
}: {
  onOpenManage: () => void;
  /** 移动端筛选抽屉的开合状态（由外壳统一管理，便于顶栏按钮控制）。 */
  drawerOpen: boolean;
  onDrawerChange: (open: boolean) => void;
}): JSX.Element {
  const [params, setParams] = useSearchParams();
  const online = useOnlineStatus();

  /* ---------------- 筛选态：与 URL 同步（可分享、可后退） ---------------- */
  const category = (params.get('category') as Category | null) ?? undefined;
  const sourceId = params.get('source') ? Number(params.get('source')) : undefined;
  const q = params.get('q') ?? undefined;

  /* ---------------- 分页与本地筛选态 ---------------- */
  // 「隐藏源」集合从收件人偏好读入（游客落 localStorage，登录后随 preferences 同步）。
  // 注意：这是**浏览器级偏好**，不随「全部新闻 / 某个源」的切换而重置 ——
  // 用户隐藏了 A、点开 B、再回「全部新闻」，隐藏状态必须还在。
  const preferences = useGuestStore((s) => s.preferences);
  const setPreference = useGuestStore((s) => s.setPreference);
  const exclude = useMemo(() => prefIdSet(preferences, 'feed.hiddenSources'), [preferences]);
  const [toast, setToast] = useState<{ message: string; tone: 'info' | 'success' | 'warn' } | null>(null);
  /** 高级筛选区（隐藏源）是否展开；默认折叠。 */
  const [showHiddenSources, setShowHiddenSources] = useState(false);

  const filters: FeedFilters = useMemo(
    () => ({
      ...(category ? { category } : {}),
      ...(sourceId !== undefined && Number.isFinite(sourceId) ? { sourceId } : {}),
      ...(q ? { q } : {}),
      // withAudio=true：让服务端附带每篇的合成状态，
      // 省去 N 次请求（openapi 明确这是为省请求设计的）。
      withAudio: true,
    }),
    [category, sourceId, q],
  );

  /* ---------------- 数据 ---------------- */
  const feed = useArticleFeed(filters, exclude);

  const sourcesQuery = useQuery({
    queryKey: sourceKeys.list(true),
    queryFn: () => listSources(true),
    staleTime: 5 * 60_000,
  });

/* ---------------- 偏好（音色/语速） ---------------- */
  const voice = prefString(preferences, 'tts.voice', DEFAULT_PREFERENCES['tts.voice']);
  const speed = prefNumber(preferences, 'tts.speed', 1);

  const currentTrack = usePlayerStore(selectCurrentTrack);
  const playQueue = usePlayerStore((s) => s.play);

  // 抽成具名常量而不是在每处内联 useCallback：这两个 hook 各自都要它，
  // 内联两次会让"同一个提示回调"实际上有两个函数实例，
  // 下游 useMemo/useCallback 的依赖比对会因此多出无意义的重算。
  const notify = useCallback((message: string, tone: 'info' | 'success' | 'warn') => {
    setToast({ message, tone });
  }, []);

  const { toggleFavorite } = useFavoriteActions(notify);

  /* ---------------- 轻提示自动消失（非阻断） ---------------- */
  useEffect(() => {
    if (!toast) return;
    const t = window.setTimeout(() => setToast(null), 3200);
    return () => window.clearTimeout(t);
  }, [toast]);

  /* ---------------- 收藏筛选 ---------------- */
  const [onlyFavorites, setOnlyFavorites] = useState(false);
  const favoriteCount = useGuestStore((s) => Object.values(s.favorites).filter((e) => !e.deleted).length);

  const visibleArticles = useMemo(
    () => (onlyFavorites ? feed.articles.filter((a) => feed.isFavorited(a.id)) : feed.articles),
    [onlyFavorites, feed],
  );

  /* ---------------- 全部标记已读（游客本地生效，登录后同步云端） ---------------- */
  // 只作用于**当前可见范围**（已含收藏筛选与隐藏源过滤），
  // 而不是"云端所有未读"——后者会让用户在一个分类页点一下、
  // 结果几百篇其它分类的文章也变了状态，这是不可预期的。
  const visibleIds = useMemo(() => visibleArticles.map((a) => a.id), [visibleArticles]);
  const markAll = useMarkAllRead({ visibleIds, isRead: feed.isRead, notify });

  /* ---------------- 增量：手动刷新按钮 ---------------- */
  const [syncing, setSyncing] = useState(false);
  const doIncremental = useCallback(async (): Promise<void> => {
    setSyncing(true);
    try {
      const result = await runIncrementalSync(fetchIncrementalPage);
      if (result.skippedNoBaseline) {
        // 无基线：先做一次首屏建立水位（这本身就是"全量拉一次"的必要代价）。
        feed.refresh();
        setToast({ message: '已建立同步基线，下次起只拉增量', tone: 'info' });
        return;
      }
      if (!result.changed) {
        setToast({ message: '已是最新，没有新文章', tone: 'info' });
        return;
      }
      if (result.cursor) writeArticleCursor(result.cursor);
      feed.applyIncremental(result.items);
      setToast({ message: `已同步 ${result.items.length} 条新文章`, tone: 'success' });
    } catch {
      setToast({ message: '增量同步失败，请检查网络', tone: 'warn' });
    } finally {
      setSyncing(false);
    }
  }, [feed]);

  /* ---------------- 播放 ---------------- */
  const handlePlay = useCallback(
    (article: ArticleDTO) => {
      // 队列：当前文章 + 其后的文章（用于连续播报，F-TTS-10）。
      // 只入队已 ready 的文章，避免点了一堆合成不出来的。
      const startIndex = Math.max(
        0,
        visibleArticles.findIndex((a) => a.id === article.id),
      );
      const tracks: PlayerTrack[] = [];
      for (const a of visibleArticles.slice(startIndex)) {
        const audioId = a.audio?.audioId;
        if (a.audio?.status !== 'ready' || audioId == null) continue;
        tracks.push({
          articleId: a.id,
          title: a.title,
          url: audioFileUrl(audioId),
          voice,
          speed,
        });
      }
      if (tracks.length === 0) return;
      playQueue(tracks, 0);
    },
    [visibleArticles, playQueue, voice, speed],
  );

  /* ---------------- 隐藏源（客户端过滤，持久化到 preferences） ---------------- */
  // 勾上 = 隐藏该源。写回 `feed.hiddenSources`：游客落 localStorage，
  // 登录后随偏好跨设备同步（复用现有机制，不另造持久化）。
  const toggleExclude = useCallback(
    (id: number) => {
      const next = new Set(exclude);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      setPreference('feed.hiddenSources', serializeIdSet(next));
    },
    [exclude, setPreference],
  );

  /** 一键恢复全部（清空隐藏集合）。 */
  const clearExclude = useCallback(() => {
    setPreference('feed.hiddenSources', '');
  }, [setPreference]);

  /* ---------------- 源计数（用于侧栏徽标） ---------------- */
  const counts = useMemo(() => {
    const m = new Map<number, number>();
    for (const s of sourcesQuery.data?.items ?? []) {
      if (s.articleCount !== undefined && s.articleCount > 0) m.set(s.id, s.articleCount);
    }
    return m;
  }, [sourcesQuery.data]);

  /* ---------------- 当前选中源名称（用于页头标题） ---------------- */
  // 侧栏点选某个源后，右侧整体切换为该源的新闻，标题也随之变成源名。
  // 来源列表尚未加载完（或 id 不存在）时回落默认文案，避免标题闪成空白。
  const activeSourceName = useMemo(() => {
    if (sourceId === undefined || !Number.isFinite(sourceId)) return undefined;
    return sourcesQuery.data?.items.find((s) => s.id === sourceId)?.name;
  }, [sourceId, sourcesQuery.data]);

  /* ---------------- 渲染 ---------------- */
  const hasSidebar = (
    <Sidebar
      sources={sourcesQuery.data?.items}
      loading={sourcesQuery.isLoading}
      error={sourcesQuery.error}
      activeSourceId={sourceId ?? null}
      onSelectSource={(id) => {
        const p = new URLSearchParams(params);
        if (id === null) {
          // 「全部新闻」= 回到全量聚合流：source 与 category 都要清掉。
          p.delete('source');
          p.delete('category');
        } else {
          // 点某个源 = **独占**该源：清掉 category，避免与分类叠加。
          // 保留 q（用户在某个源里继续搜索是自然预期）。
          p.set('source', String(id));
          p.delete('category');
        }
        setParams(p, { replace: true });
        onDrawerChange(false);
      }}
      onOpenManage={onOpenManage}
      counts={counts}
    />
  );

  return (
    <div className="mx-auto flex max-w-[1600px]">
      {/* ---------- 桌面侧栏（常驻） ---------- */}
      <aside className="sticky top-14 hidden h-[calc(100vh-3.5rem)] w-64 shrink-0 border-r border-line lg:block xl:w-72">
        {hasSidebar}
      </aside>

      {/* ---------- 移动端抽屉 ---------- */}
      {drawerOpen ? (
        <div
          className="fixed inset-0 z-40 flex bg-slate-900/40 backdrop-blur-sm animate-fade-in lg:hidden"
          onMouseDown={(e) => {
            if (e.target === e.currentTarget) onDrawerChange(false);
          }}
        >
          <div className="h-full w-[17rem] max-w-[85vw] border-r border-line bg-surface shadow-pop">
            <div className="flex h-14 items-center justify-between border-b border-line px-4">
              <span className="text-[15px] font-semibold text-ink">筛选与来源</span>
              <button
                type="button"
                onClick={() => onDrawerChange(false)}
                className="ez-btn ez-btn-sm ez-btn-ghost h-8 w-8 p-0"
                aria-label="关闭"
              >
                <CloseIcon className="text-base" />
              </button>
            </div>
            <div className="h-[calc(100%-3.5rem)]">{hasSidebar}</div>
          </div>
        </div>
      ) : null}

      {/* ---------- 主区 ---------- */}
      <main className="min-w-0 flex-1 pb-24">
        {!online ? <OfflineBanner /> : null}

        {/* 页头：标题 + 排序说明 + 刷新 */}
        <div className="flex flex-wrap items-center justify-between gap-3 px-4 pt-5 sm:px-6">
          <div>
            <h1 className="text-lg font-semibold tracking-tight text-ink">
              {onlyFavorites ? '我的收藏' : activeSourceName ?? '全部新闻'}
            </h1>
            <p className="mt-0.5 text-[12px] text-ink-muted">
              {visibleArticles.length > 0 ? `${visibleArticles.length} 篇` : ''}
              {q ? ` · 搜索「${q}」` : ''}
            </p>
          </div>

          <div className="flex items-center gap-2">
            {/* 收藏筛选（游客也可用 —— 收藏存在本地） */}
            <button
              type="button"
              onClick={() => setOnlyFavorites((v) => !v)}
              className={`ez-btn ez-btn-sm h-9 px-2.5 ${
                onlyFavorites ? 'ez-btn-primary' : 'ez-btn-secondary'
              }`}
              title="只看收藏"
            >
              <StarIcon filled={onlyFavorites} className="text-sm" />
              收藏
              {favoriteCount > 0 ? (
                <span className="ml-0.5 text-[11px] tabular-nums opacity-75">{favoriteCount}</span>
              ) : null}
            </button>

            {/* 全部标记已读：与「收藏」筛选同属"作用于当前列表"的操作，
                放在它之后、增量刷新之前 —— 顺序即"筛选 → 批量处理 → 拉数据"。 */}
            <button
              type="button"
              onClick={markAll.markAllRead}
              disabled={!markAll.canMarkAll}
              className="ez-btn ez-btn-sm ez-btn-secondary h-9 px-2.5"
              title={
                markAll.unreadCount > 0
                  ? `把当前列表的 ${markAll.unreadCount} 篇未读标记为已读`
                  : '当前列表没有未读文章'
              }
            >
              {markAll.pending ? (
                <span className="h-3.5 w-3.5 animate-spin rounded-full border-2 border-slate-300 border-t-brand" />
              ) : (
                <CheckAllIcon className="text-sm" />
              )}
              <span className="hidden sm:inline">全部已读</span>
              <span className="ml-0.5 hidden text-[11px] tabular-nums opacity-75 sm:inline">
                {markAll.unreadCount > 0 ? markAll.unreadCount : null}
              </span>
              {/*
                ★ 窄屏（< sm）只留图标 —— 但计数要留着，且**只能有一份**。
                计数是这个按钮在窄屏下唯一的"我现在有多少事可做"信号：
                没有它，未读时是一个匿名双勾、已读后变成匿名灰双勾，
                用户看不出差别，也就没有理由不点它。数字只占 ~14px，代价很小。

                断点用 sm 而不是 md：与相邻的「增量刷新」保持一致。
                「收藏」在窄屏下不隐藏文字是既有代码的行为，不在本次范围内，
                跟着它改会让本按钮在别的断点上和另外两个错开。
              */}
              {markAll.unreadCount > 0 ? (
                <span className="text-[11px] tabular-nums opacity-75 sm:hidden">{markAll.unreadCount}</span>
              ) : null}
            </button>

            {/* 增量刷新 */}
            <button
              type="button"
              onClick={() => void doIncremental()}
              disabled={syncing}
              className="ez-btn ez-btn-sm ez-btn-secondary h-9 px-2.5"
              title="只拉取变化的部分，省流量"
            >
              {syncing ? (
                <span className="h-3.5 w-3.5 animate-spin rounded-full border-2 border-slate-300 border-t-brand" />
              ) : (
                <FilterIcon className="text-sm" />
              )}
              <span className="hidden sm:inline">增量刷新</span>
            </button>
          </div>
        </div>

        {/* ---------- 高级筛选：隐藏源（仅「全部新闻」视图，默认折叠） ----------
            为何只在全量流出现：隐藏某个源唯一合理的场景就是"在一大堆源里
            屏蔽不想看的那些"。一旦用户已经点开某个具体来源，该视图下只有这一个源，
            隐藏它等于清空列表，没有意义。
            默认折叠 + 文本级触发器：这是低频的"降噪"手段，不该抢主流程的视觉重量。 */}
        {!onlyFavorites && sourceId === undefined ? (
          <div className="px-4 pt-3 sm:px-6">
            <button
              type="button"
              onClick={() => setShowHiddenSources((v) => !v)}
              aria-expanded={showHiddenSources}
              className="text-[12px] text-ink-muted underline-offset-2 hover:text-brand hover:underline"
            >
              {showHiddenSources ? '收起高级筛选' : '高级筛选'}
              {exclude.size > 0 ? (
                <span className="ml-1.5 rounded-full bg-brand-soft px-1.5 py-0.5 text-[11px] tabular-nums text-brand dark:bg-slate-800 dark:text-blue-200">
                  已隐藏 {exclude.size} 个源
                </span>
              ) : null}
            </button>

            {/* ★ 筛选生效必须有可见反馈：隐藏了源、列表变短却不知道原因，是静默行为。
                折叠态下用上面那个角标承担这个提示；展开后这里再给一行说明 + 清除入口。 */}
            {exclude.size > 0 && !showHiddenSources ? (
              <p className="mt-1 text-[11px] leading-relaxed text-ink-faint">
                列表已排除 {exclude.size} 个来源，点「高级筛选」调整。
              </p>
            ) : null}

            {showHiddenSources ? (
              <div className="mt-2 rounded-lg border border-line bg-surface-raised p-3" data-testid="hidden-sources-panel">
                <p className="text-[11px] leading-relaxed text-ink-faint">
                  勾选即从「全部新闻」里隐藏该来源（只影响你的浏览，不改变来源的启用状态）。
                </p>
                <ul className="mt-2 flex flex-wrap gap-x-4 gap-y-1.5">
                  {(sourcesQuery.data?.items ?? []).map((s) => (
                    <li key={s.id}>
                      <label className="flex cursor-pointer items-center gap-1.5 text-[12px] text-ink-muted">
                        <input
                          type="checkbox"
                          checked={exclude.has(s.id)}
                          onChange={() => toggleExclude(s.id)}
                          className="h-3.5 w-3.5 accent-brand"
                        />
                        <span className={exclude.has(s.id) ? 'text-ink-faint line-through' : ''}>{s.name}</span>
                      </label>
                    </li>
                  ))}
                </ul>
                {exclude.size > 0 ? (
                  <button
                    type="button"
                    onClick={clearExclude}
                    className="mt-2 text-[12px] text-brand underline underline-offset-2"
                  >
                    恢复全部来源
                  </button>
                ) : (
                  <p className="mt-2 text-[11px] text-ink-faint">暂无隐藏的来源。</p>
                )}
              </div>
            ) : null}
          </div>
        ) : null}

        {/* ---------- 列表 ---------- */}
        <div className="px-4 pt-4 sm:px-6">
          {feed.error && feed.articles.length === 0 ? (
            <ErrorState error={feed.error} onRetry={feed.refresh} />
          ) : feed.isLoading ? (
            <ArticleListSkeleton />
          ) : visibleArticles.length === 0 ? (
            onlyFavorites ? (
              <EmptyState
                title="还没有收藏"
                description="点击文章卡片右上角的星标即可收藏，收藏会保存在本机，登录后自动同步到云端。"
              />
            ) : q ? (
              <NoSearchResults keyword={q} />
            ) : (
              <EmptyState
                title={feed.hasActiveFilters ? '当前筛选条件下没有文章' : '还没有任何新闻'}
                description={
                  feed.hasActiveFilters
                    ? '试试放宽筛选条件，或到「管理来源」里启用更多新闻源。'
                    : '新闻由外部采集器写入服务端。请确认采集器已配置并运行，或先添加一个 RSS 源。'
                }
                action={
                  feed.hasActiveFilters ? (
                    <button
                      type="button"
                      onClick={() => {
                        setPreference('feed.hiddenSources', '');
                        setParams(new URLSearchParams(), { replace: true });
                      }}
                      className="ez-btn ez-btn-md ez-btn-secondary"
                    >
                      清除全部筛选
                    </button>
                  ) : (
                    <button type="button" onClick={onOpenManage} className="ez-btn ez-btn-md ez-btn-secondary">
                      管理新闻来源
                    </button>
                  )
                }
              />
            )
          ) : (
            <>
              {/* 有新文章提示 */}
              {feed.freshCount > 0 ? (
                <div className="ez-enter mb-3 flex items-center justify-between gap-3 rounded-lg bg-brand-soft px-3.5 py-2 text-[12px] text-blue-800 dark:bg-slate-800 dark:text-blue-200">
                  <span>已并入 {feed.freshCount} 条新文章</span>
                  <button type="button" onClick={feed.clearFresh} className="underline underline-offset-2">
                    知道了
                  </button>
                </div>
              ) : null}

              <ul className="space-y-3">
                {visibleArticles.map((a) => (
                  <li key={a.id} className="ez-enter">
                    <ArticleCard
                      article={a}
                      favorited={feed.isFavorited(a.id)}
                      read={feed.isRead(a.id)}
                      hasAudio={a.audio?.status === 'ready' && a.audio.audioId !== null}
                      onToggleFavorite={toggleFavorite}
                      onPlay={handlePlay}
                    />
                  </li>
                ))}
              </ul>

              <ListFooter
                loading={feed.isFetchingMore}
                hasMore={feed.hasMore}
                onLoadMore={feed.loadMore}
                total={visibleArticles.length}
              />

              {feed.isFetchingMore ? <ArticleListSkeleton count={2} /> : null}
            </>
          )}
        </div>
      </main>

      {/* ---------- 底部播放器 ---------- */}
      {currentTrack ? <AudioPlayerBar src={currentTrack.url} /> : null}

      {/* ---------- 轻提示 ---------- */}
      {toast ? (
        <div className="pointer-events-none fixed bottom-20 left-1/2 z-50 w-full max-w-sm -translate-x-1/2 px-4">
          <Toast message={toast.message} tone={toast.tone} onClose={() => setToast(null)} />
        </div>
      ) : null}
    </div>
  );
}
