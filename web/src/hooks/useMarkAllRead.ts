/**
 * 「全部标记已读」—— 列表级批量操作（游客本地生效，登录后同步云端）。
 *
 * ============================ 为什么单独一个 hook，而不是塞进 useFavoriteActions ============================
 * `useFavoriteActions` 管的是**单条**收藏/已读：一次一个 id、乐观写本地、
 * 失败**不回滚**（那里的约定是"网络问题不该让用户的操作消失"）。
 * 批量操作的语义完全不同，有两条它无法承载的硬约束：
 *
 * 1. **必须回滚**。单条不回滚是合理的 —— 用户明确点了那一篇，
 *    本地记着"已读"符合他的意图。但"全部标记已读"往往是**误点**
 *    （移动端按钮挨得近），而且一次可能影响上百篇。
 *    服务端拒绝后如果本地还留着"这上百篇都读过了"，
 *    下次 `useUserStateSync` 的 LWW 会因本地 `updatedAt` 更晚而**保留本地**
 *    —— 假状态永久生效且用户无法自愈，正是 `stores/guest.ts` 里
 *    clampClientTimestamp 注释警告的那类"用户看不见但永远修好的坏状态"。
 *
 * 2. **登录态的"已读"根本不来自本地 store**。
 *    `useArticleFeed.isRead()` 登录态优先读服务端返回的 `article.isRead`。
 *    所以只写本地 store 的话，登录态下**UI 一点都不会变** ——
 *    乐观更新必须同时打在 react-query 的 `['articles']` 缓存上。
 *    单条路径不存在这个问题（它靠 mutation 成功后的 invalidate 兜住，
 *    用户感知不到中间态）；批量上百篇的延迟用户会明显察觉。
 *
 * ============================ 乐观更新的两处落点 ============================
 * - `['articles']` query 缓存：**仅登录态需要**。游客读的也是这份缓存，
 *   但里面 `article.isRead` 是 undefined，回写它并不会改变显示 ——
 *   游客的已读态由 `useArticleFeed` 回落到本地 store 判定。写了反而多余。
 * - `guest.reads` 本地 store：**两种身份都要写**。游客它是唯一落点；
 *   登录态下它决定了下次 LWW 合并会不会把"已读"这个事实传上云端。
 *
 * 两处都在失败时回滚。缓存回滚靠 mutation 上下文里存的**全量快照**
 * （不用反向 patch —— 反向 patch 在"同一批里既有新增键又有覆盖键"时容易写错，
 * 而快照回放是无脑的 `setQueryData`，不需要考虑键的来龙去脉）。
 */

import { useCallback, useMemo } from 'react';
import { useMutation, useQueryClient, type InfiniteData, type QueryClient } from '@tanstack/react-query';
import { ApiError } from '@/api/client';
import { batchSetState } from '@/api/endpoints';
import type { ArticlePage, BatchStateResult } from '@/api/types';
import { articleKeys } from '@/lib/queryKeys';
import { useAuthStore } from '@/stores/auth';
import { useGuestStore, type LocalStateSnapshot } from '@/stores/guest';
/**
 * 单次提交条数上限，与服务端 `batchLimit` 对齐。
 *
 * ★ 必须由前端自己裁剪：超限服务端返回 413，整批被拒。
 *   裁剪而非分片，是因为服务端刻意保证了**整批原子**（同一写事务，
 *   中途失败全批回滚）。自己切片等于把这个保证撕碎——
 *   用户会得到"前 500 条生效、后 500 条没生效"的中间态，
 *   而这中间态既不是用户要的，也无法与"服务端只处理了一部分"区分开。
 *   被裁掉的条目不会丢：已读合并是**并集**，用户加载更多后再点一次即可。
 */
const BATCH_LIMIT = 500;

/** 轻提示回调（与 useFavoriteActions 的 Notify 同型）。 */
type Notify = (message: string, tone: 'info' | 'success' | 'warn') => void;

/**
 * 抓取 `['articles']` 下**全部文章列表**缓存的快照。
 *
 * ★ 为什么不直接用 `getQueriesData({ queryKey: articleKeys.all })`：
 *   `all` 是前缀 key，会把 `['articles','categories']`（`Category[]`）和
 *   `['articles','detail',id]`（`ArticleDTO`）一起抓进来。那些缓存本轮
 *   根本没被改动，回放它们纯属多余；而一旦将来有人把这份快照用于
 *   「反向 patch」而不是「原样回放」，写回一个无关缓存就是实打实的串数据。
 *   快照只记真正会被 patch 的那部分，回滚范围与改动范围严格一致。
 */
function listCacheSnapshots(
  queryClient: QueryClient,
): Array<[readonly unknown[], InfiniteData<ArticlePage> | undefined]> {
  return queryClient
    .getQueriesData<InfiniteData<ArticlePage>>({ queryKey: articleKeys.all })
    .filter(([, data]) => Boolean(data) && Array.isArray(data?.pages));
}

/** mutation 上下文：改动前的快照，供失败时回放。 */
interface RollbackContext {
  cacheSnapshots: Array<[readonly unknown[], InfiniteData<ArticlePage> | undefined]>;
  storeSnapshot: LocalStateSnapshot;
}

export interface UseMarkAllReadOptions {
  /** 当前列表**可见范围内**的文章 id（已含收藏筛选 / 隐藏源过滤的结果）。 */
  visibleIds: readonly number[];
  /**
   * 已读判定函数（直接传 `feed.isRead`）。
   *
   * ★ 刻意用函数而不是"已读 id 集合"：登录态下已读态来自服务端字段
   *   （`useArticleFeed.isRead` 内部的优先级），把它的实现细节复制一份到本 hook
   *   会立刻漂移 —— 服务端或该 hook 一改判定来源，这里就静默算错集合，
   *   而且错得没有任何报错。
   */
  isRead: (articleId: number) => boolean;
  /** 轻提示回调。 */
  notify: Notify;
}

export interface UseMarkAllReadResult {
  /** 执行批量标记。 */
  markAllRead: () => void;
  /** 当前可见范围内未读的条数（按钮上的计数 / disabled 判据）。 */
  unreadCount: number;
  /** 按钮是否可点：无内容 / 全部已读 / 提交中 → false。 */
  canMarkAll: boolean;
  /** 提交中（用于按钮内的 loading 态）。 */
  pending: boolean;
}

export function useMarkAllRead({
  visibleIds,
  isRead,
  notify,
}: UseMarkAllReadOptions): UseMarkAllReadResult {
  const loggedIn = useAuthStore((s) => s.status === 'authenticated');
  const markAllReadLocal = useGuestStore((s) => s.markAllRead);
  const rollbackReadsLocal = useGuestStore((s) => s.rollbackReads);
  const queryClient = useQueryClient();

  /**
   * 可见范围内未读的 id（已去重、已剔除非法值）。
   *
   * 只提交未读的：已读的传上去是纯浪费 —— 服务端会逐条做 LWW 判定后
   * 原样跳过（`readsChanged` 不增加），但仍占掉了 500 条的名额。
   * 在一个"多数文章已读"的列表里，这直接决定了这个功能还能不能用。
   */
  const unreadIds = useMemo(() => {
    const seen = new Set<number>();
    const out: number[] = [];
    for (const id of visibleIds) {
      if (!Number.isInteger(id) || id <= 0) continue;
      if (seen.has(id)) continue;
      seen.add(id);
      if (!isRead(id)) out.push(id);
    }
    return out;
  }, [visibleIds, isRead]);

  /** 把 `['articles']` 下所有列表缓存里的目标条目标成已读（仅登录态调用）。 */
  const patchCache = useCallback(
    (ids: ReadonlySet<number>): void => {
      queryClient.setQueriesData<InfiniteData<ArticlePage>>(
        { queryKey: articleKeys.all },
        (old) => {
          // ★ 必须做形状守卫：`articleKeys.all` 是**前缀** key，
          //   setQueriesData 会把 `['articles','categories']`（data 是 Category[]）
          //   和 `['articles','detail',id]`（data 是 ArticleDTO）一并匹配进来，
          //   它们没有 `pages`。直接 `old.pages.map` 会抛 TypeError。
          //
          //   而这里抛错的代价特别大：`onMutate` 抛错时 react-query 仍会调
          //   `onError`，但 context 是 undefined → 下面整段回滚被跳过 →
          //   乐观更新残留成"用户看不见但永远修不好的坏状态"。
          //   所以这里宁可漏改也不能抛。
          if (!old || !Array.isArray(old.pages)) return old;
          return {
            ...old,
            pages: old.pages.map((page) => ({
              ...page,
              items: page.items.map((item) => (ids.has(item.id) ? { ...item, isRead: true } : item)),
            })),
          };
        },
      );
    },
    [queryClient],
  );

  const mutation = useMutation<BatchStateResult, unknown, { ids: number[]; totalUnread: number }, RollbackContext>({
    mutationFn: ({ ids }) => batchSetState({ articleIds: ids, read: true }),

    // ★ 快照必须在改缓存**之前**抓，否则抓到的是改后的值，回滚等于没回滚。
    onMutate: ({ ids }) => {
      const cacheSnapshots = listCacheSnapshots(queryClient);
      const storeSnapshot = markAllReadLocal(ids);
      patchCache(new Set(ids));
      return { cacheSnapshots, storeSnapshot };
    },

    onError: (err, _vars, context) => {
      // 两处都必须回滚，少一处就是文件头注释里说的那种坏状态。
      if (context) {
        for (const [key, data] of context.cacheSnapshots) {
          queryClient.setQueryData(key, data);
        }
        rollbackReadsLocal(context.storeSnapshot);
      }

      // 404 有独立的含义，值得单独说：服务端在写入前会校验每个 id 是否存在，
      // 任一不存在就整批拒绝。列表是"上次加载时的快照"，
      // 期间文章被 retention 清理 / 源被删除就会撞上。
      // 这时给一句"刷新后重试"比笼统的"标记失败"有用得多 ——
      // 用户刷新后那篇文章就从列表里消失了，重试即可成功。
      if (err instanceof ApiError && err.status === 404) {
        notify?.('列表中已有文章被清理，整批未生效。请刷新后重试', 'warn');
        return;
      }
      notify?.('标记失败，已恢复原状，请稍后重试', 'warn');
    },

    onSuccess: (_result, variables) => {
      // 不回滚本地：这次真的成功了，本地与服务端一致。
      // 让列表的 isRead 失效重取，把服务端权威值（含它自己写的 updatedAt）拉回来。
      void queryClient.invalidateQueries({ queryKey: articleKeys.all });
      void queryClient.invalidateQueries({ queryKey: ['me'] });

      // ★ 用提交时刻的 totalUnread，而不是回调里现读 unreadIds：
      //   请求回来时列表可能已经变了（用户又点了别的、或者失败回滚已生效），
      //   那时的 unreadIds 描述的是另一个时刻的状态，拿它算差值会报错数字。
      const { ids, totalUnread } = variables;
      const dropped = totalUnread - ids.length;
      notify?.(
        dropped > 0
          ? `已标记 ${ids.length} 篇已读（单次上限 ${BATCH_LIMIT}，还有 ${dropped} 篇未标记）`
          : `已标记 ${ids.length} 篇为已读`,
        'success',
      );
      // ★ 刻意不用 `result.readsChanged` 当成功计数：实测它对每个去重后的 id
      //   无条件 +1（不比较库内现状），所以对同一批重复提交仍返回同一个数。
      //   它的语义是"提交了多少条"，不是"用户看到几篇变已读"——
      //   拿它报数会让用户以为少标了。上面用提交前自己算的 ids.length 才是用户视角。
    },
  });

  const { isPending, mutate } = mutation;

  const markAllRead = useCallback(() => {
    if (isPending || unreadIds.length === 0) return;

    const ids = unreadIds.length > BATCH_LIMIT ? unreadIds.slice(0, BATCH_LIMIT) : unreadIds;
    const dropped = unreadIds.length - ids.length;

    // ---- 游客态：只写本地，不发请求 ----
    // 与单条 `toggleRead` 的游客路径一致（那里也不发请求）。
    // 没有服务端就没有"失败"，也就不存在回滚的必要。
    if (!loggedIn) {
      markAllReadLocal(ids);
      notify?.(
        dropped > 0
          ? `已标记 ${ids.length} 篇已读（单次上限 ${BATCH_LIMIT}，还有 ${dropped} 篇未标记），登录后可跨设备同步`
          : `已标记 ${ids.length} 篇为已读，登录后可跨设备同步`,
        'info',
      );
      return;
    }

    // 不预发"正在标记"的 toast：按钮内已有 loading 态，再叠一个 toast 是噪音。
    mutate({ ids, totalUnread: unreadIds.length });
  }, [isPending, loggedIn, markAllReadLocal, mutate, notify, unreadIds]);

  return {
    markAllRead,
    unreadCount: unreadIds.length,
    // ★ 全部已读时禁用。"全部已读后按钮仍可点"是最容易被抓的体验缺陷：
    //   点了没有任何反馈，用户会以为按钮坏了。
    canMarkAll: unreadIds.length > 0 && !isPending,
    pending: isPending,
  };
}

/** 导出上限常量，供测试与调用方核对（避免两处各写一个 500 然后悄悄漂移）。 */
export { BATCH_LIMIT };
