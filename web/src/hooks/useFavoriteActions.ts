/**
 * 收藏 / 已读 的状态切换（游客本地优先，登录后同步云端）。
 *
 * ============================ 为什么"先本地后同步" ============================
 * PRD-ACCOUNT §1.1 R3：**游客点击收藏/已读必须立即生效于本地**，
 * 不得先要求登录。所以顺序是：
 *   1. 立刻写本地 store（含墓碑）→ UI 立即反馈；
 *   2. 若已登录，**异步**调 `POST /me/favorites` / `/me/reads`；
 *   3. 同步失败**不弹窗、不回滚本地**：网络问题不该让用户的操作消失。
 *      失败时保留本地态并在下次成功同步时被覆盖（服务端是权威）。
 *
 * ★ 墓碑：取消收藏写 `deleted=true`（不是物理删除），
 *   否则"游客态取消过"的信息丢失，合并后会被云端旧记录复活（US-ACC-04）。
 */

import { useCallback } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { setFavorite, setRead } from '@/api/endpoints';
import { useAuthStore } from '@/stores/auth';
import { clampClientTimestamp, useGuestStore } from '@/stores/guest';

/** 乐观更新失败后的提示回调类型。 */
type Notify = (message: string, tone: 'info' | 'warn') => void;

export interface UseFavoriteActionsResult {
  /** 切换收藏；返回切换后的状态（true = 已收藏）。 */
  toggleFavorite: (articleId: number) => boolean;
  /** 切换已读；返回切换后的状态（true = 已读）。 */
  toggleRead: (articleId: number) => boolean;
  /** 同步中的文章 id 集合（可据此禁用按钮）。 */
  syncing: ReadonlySet<number>;
}

/**
 * @param notify 轻提示回调（用于告知"已登录可跨设备同步"这类**非阻断**信息）。
 */
export function useFavoriteActions(notify?: Notify): UseFavoriteActionsResult {
  const loggedIn = useAuthStore((s) => s.status === 'authenticated');
  const toggleFavoriteLocal = useGuestStore((s) => s.toggleFavorite);
  const toggleReadLocal = useGuestStore((s) => s.toggleRead);
  const queryClient = useQueryClient();

  /** 正在同步的文章（避免重复点击打服务端）。 */
  const favoriteMutation = useMutation({
    mutationFn: (input: { articleId: number; deleted: boolean }) => setFavorite(input),
    onSuccess: () => {
      // 服务端状态变了，让列表的 isFavorited/isRead 失效重取。
      void queryClient.invalidateQueries({ queryKey: ['articles'] });
      void queryClient.invalidateQueries({ queryKey: ['me'] });
    },
    onError: () => {
      // 不回滚本地：本地操作已生效，静默等下次同步覆盖。
      notify?.('已保存在本机，登录后会自动同步到云端', 'warn');
    },
  });

  const readMutation = useMutation({
    mutationFn: (input: { articleId: number; deleted: boolean }) => setRead(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['articles'] });
      void queryClient.invalidateQueries({ queryKey: ['me'] });
    },
    onError: () => {
      notify?.('已保存在本机，登录后会自动同步到云端', 'warn');
    },
  });

  const toggleFavorite = useCallback(
    (articleId: number): boolean => {
      // 1) 本地立即生效（游客也可用，R3）
      const nowFavorite = toggleFavoriteLocal(articleId);

      // 2) 登录后异步同步
      if (loggedIn) {
        favoriteMutation.mutate({
          articleId,
          deleted: !nowFavorite,
        });
      } else {
        // 轻提示：**非阻断**、不要求登录（R3 明示允许的可关闭轻提示）。
        notify?.('收藏已保存到本机，登录后可跨设备同步', 'info');
      }
      return nowFavorite;
    },
    [toggleFavoriteLocal, loggedIn, favoriteMutation, notify],
  );

  const toggleRead = useCallback(
    (articleId: number): boolean => {
      const nowRead = toggleReadLocal(articleId);

      if (loggedIn) {
        readMutation.mutate({ articleId, deleted: !nowRead });
      }
      return nowRead;
    },
    [toggleReadLocal, loggedIn, readMutation],
  );

  // 同步中的 id：mutation 只知道"最近一次"的变量，用 Set 累积当前在途项。
  const syncing = new Set<number>();
  if (favoriteMutation.isPending && favoriteMutation.variables) syncing.add(favoriteMutation.variables.articleId);
  if (readMutation.isPending && readMutation.variables) syncing.add(readMutation.variables.articleId);

  return { toggleFavorite, toggleRead, syncing };
}

/** 导出时间戳钳制，供其它模块复用。 */
export { clampClientTimestamp };