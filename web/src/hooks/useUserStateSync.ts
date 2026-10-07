/**
 * 登录态的数据同步：收藏 / 已读 / 偏好 从云端拉取（增量优先）。
 *
 * ============================ 游标语义（DEC-11） ============================
 * `GET /me/favorites?since=<cursor>` 与 `GET /me/reads?since=<cursor>`
 * 的语义与文章列表的增量 `cursor` **完全一致**（PRD-ACCOUNT §5.3）：
 *   - 复合键 `(updated_at, id)` 排序，opaque base64url 游标；
 *   - `hasMore=true` 时用 `nextCursor` 继续翻，直到 `hasMore=false`；
 *   - ★ **墓碑项（deleted=true）必须随流返回**，客户端收到后**本地移除该条**。
 *     若把取消收藏实现为物理删除，它会从增量流彻底消失，
 *     其他设备永远收不到"对方已取消"，跨端删除无法同步。
 *
 * 首次登录（无游标）走全量；之后只拉变化。
 *
 * ============================ 为什么这里 withArticles=false ============================
 * 本 hook 的职责是**把云端状态灌进本地 store**（LWW 合并），只消费
 * `articleId` / `deleted` / `updatedAt` 三个字段。标题/摘要/来源对合并毫无作用，
 * 因此显式传 `withArticles=false` 跳过服务端的文章表批量查询。
 * 需要展示摘要的页面（如"我的收藏"）走自己的 `withArticles=true` 查询，
 * 两条路径互不干扰 —— 本 hook 也不会因为多出摘要字段而出错
 * （`applyCloudState` 只读三个基础字段，多余字段被忽略）。
 */

import { useCallback, useEffect } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { getMe, getPreferences, listFavorites, listReads } from '@/api/endpoints';
import type { StateItem, StatePage } from '@/api/types';
import { StorageKey, readString, removeKey, writeString } from '@/lib/storage';
import { useAuthStore } from '@/stores/auth';
import { useGuestStore } from '@/stores/guest';

/** 单页条数（openapi StateLimit：默认 200，最大 1000）。 */
const STATE_PAGE_LIMIT = 200;

/** 单轮拉取的最大页数（防御性上限）。 */
const MAX_PAGES = 20;

/** 循环拉完一页增量流。 */
async function drainState(
  since: string | null,
  fetchPage: (cursor?: string) => Promise<StatePage>,
): Promise<{ items: StateItem[]; cursor: string | null; complete: boolean }> {
  const all: StateItem[] = [];
  // localStorage 读出来是 null（无水位），query 参数里要表达成"不传"。
  let cursor: string | undefined = since ?? undefined;
  let pages = 0;
  let complete = false;

  for (;;) {
    const page: StatePage = await fetchPage(cursor);
    all.push(...(page.items ?? []));
    pages += 1;

    if (!page.hasMore || page.nextCursor === null) {
      complete = true;
      cursor = page.nextCursor ?? cursor;
      break;
    }
    // 未读完就达到上限：**不推进游标**，下一轮仍从旧水位重来。
    // 宁可多拉，绝不漏拉（漏拉会导致跨设备删除不同步，且很难被发现）。
    if (pages >= MAX_PAGES) {
      complete = false;
      break;
    }
    cursor = page.nextCursor;
  }

  return { items: all, cursor: cursor ?? null, complete };
}

/**
 * 登录后同步云端状态。
 *
 * 触发时机：
 * 1. 登录态成立时（首次登录 / refresh 恢复会话后）；
 * 2. 回到前台时（由 App 层的增量同步统一触发，这里通过 queryClient 失效间接复用）。
 */
export function useUserStateSync(): void {
  const loggedIn = useAuthStore((s) => s.status === 'authenticated');
  const setUser = useAuthStore((s) => s.setUser);
  const queryClient = useQueryClient();

  const run = useCallback(async (): Promise<void> => {
    if (useAuthStore.getState().status !== 'authenticated') return;

    const favoritesCursor = readString(StorageKey.favoriteSyncCursor);
    const readsCursor = readString(StorageKey.readSyncCursor);

    // ---- 收藏 ----
    try {
      // ★ withArticles=false：本流程只做 LWW 合并，只读 articleId/deleted/updatedAt，
      //   跳过文章表查询即可（收藏页要展示标题，那是另一条走 withArticles=true 的路径）。
      const fav = await drainState(favoritesCursor, (cursor) =>
        listFavorites({ since: cursor, limit: STATE_PAGE_LIMIT, withArticles: false }),
      );
      // applyCloudState 内部是"读当前 → 合并 → 写回"，多次调用不会互相覆盖。
      if (fav.items.length > 0) useGuestStore.getState().applyCloudState(fav.items, [], {});
      if (fav.complete && fav.cursor) writeString(StorageKey.favoriteSyncCursor, fav.cursor);
    } catch {
      // 单项失败不影响其它项：收藏不同步只是"云端收藏没显示"，功能不受损。
    }

    // ---- 已读 ----
    try {
      const rd = await drainState(readsCursor, (cursor) =>
        listReads({ since: cursor, limit: STATE_PAGE_LIMIT, withArticles: false }),
      );
      if (rd.items.length > 0) useGuestStore.getState().applyCloudState([], rd.items, {});
      if (rd.complete && rd.cursor) writeString(StorageKey.readSyncCursor, rd.cursor);
    } catch {
      /* 同上 */
    }

    // ---- 偏好（整包 KV：服务端有值以服务端为准） ----
    try {
      const prefs = await getPreferences();
      if (Object.keys(prefs.preferences).length > 0) {
        useGuestStore.getState().applyCloudState([], [], prefs.preferences);
      }
    } catch {
      /* 偏好同步失败不影响使用 */
    }

    // ---- 账号资料（确保 store 里的 user 是最新的） ----
    try {
      const me = await getMe();
      const cur = useAuthStore.getState().user;
      // 期间换了账号（me.id 与内存态不符）：丢弃本次结果，避免串号。
      if (cur && cur.id !== me.id) return;
      setUser(me);
    } catch {
      /* 同上 */
    }

    // 列表里的 isFavorited/isRead 可能已变化。
    await queryClient.invalidateQueries({ queryKey: ['articles'] });
  }, [setUser, queryClient]);

  useEffect(() => {
    if (!loggedIn) return;
    void run();
  }, [loggedIn, run]);

  /** 登出时清掉同步游标，避免下个账号从这个水位续拉而看不到自己的数据。 */
  useEffect(() => {
    if (loggedIn) return;
    removeKey(StorageKey.favoriteSyncCursor);
    removeKey(StorageKey.readSyncCursor);
  }, [loggedIn]);
}

/**
 * 收藏/已读的完整回填（进入"我的收藏"页时调用）。
 *
 * 正常情况下增量游标已足够；这里提供"强制全量重来"的入口，
 * 用于本地数据疑似与云端不一致时的自愈（设置面板的"重新同步"）。
 */
export async function fullResyncUserState(): Promise<void> {
  removeKey(StorageKey.favoriteSyncCursor);
  removeKey(StorageKey.readSyncCursor);

  const fav = await drainState(null, (cursor) =>
    listFavorites({ since: cursor, limit: STATE_PAGE_LIMIT, withArticles: false }),
  );
  const rd = await drainState(null, (cursor) =>
    listReads({ since: cursor, limit: STATE_PAGE_LIMIT, withArticles: false }),
  );

  if (fav.cursor) writeString(StorageKey.favoriteSyncCursor, fav.cursor);
  if (rd.cursor) writeString(StorageKey.readSyncCursor, rd.cursor);

  useGuestStore.getState().applyCloudState(fav.items, rd.items, {});
}