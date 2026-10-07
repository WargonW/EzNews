/**
 * 会话引导：应用启动与刷新页面后的静默会话恢复。
 *
 * ============================ 为什么需要它 ============================
 * Access Token **只在内存**（安全要求，见 stores/auth.ts），
 * 所以刷新页面后内存必然为空。此时 UI 是"游客态"还是"其实已登录"未知。
 *
 * 做法：启动时显式调一次 `restoreSession()`（内部走 `/auth/refresh`，
 * Cookie 自动携带，读不到 refreshToken 也无需读到），拿到新 token 即静默恢复为登录态。
 *
 * ============================ 三个必须注意的点 ============================
 * 1. **不弹窗、不跳转**：恢复期间 UI 展示完整的游客浏览能力，
 *    仅顶栏登录入口呈骨架态。绝不能因为在恢复就拦出任何内容（PRD R1）。
 * 2. **失败即游客态**：`status='guest'`，功能不受限（R5）。
 * 3. **复用单飞通道**：本 hook 调的是 client.ts 内部的同一个 performRefresh，
 *    不会开出第二条刷新路径。
 */

import { useEffect } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { restoreSession } from '@/api/client';
import { fetchIncrementalPage, runIncrementalSync, writeArticleCursor } from '@/lib/sync';
import { useAuthStore } from '@/stores/auth';
import { useMergeGuestData } from '@/hooks/useMergeGuestData';

/**
 * 引导流程。必须挂在 App 根组件且**只挂一次**。
 */
export function useSessionBootstrap(): void {
  const queryClient = useQueryClient();
  const status = useAuthStore((s) => s.status);

  // 登录态下自动完成待处理的合并批次（冷启动重试，Merge Policy §4.2）。
  useMergeGuestData(status === 'authenticated');

  // ---- 启动时静默恢复会话 ----
  useEffect(() => {
    let cancelled = false;

    void (async () => {
      try {
        await restoreSession();
      } catch {
        // restoreSession 内部已吞掉所有错误；走到这里说明是意外异常，忽略即可。
      } finally {
        // 无论成败都必须把 status 推进出 'initializing'，
        // 否则 UI 会永久停在"初始化中"（看起来像加载不出来）。
        if (!cancelled) useAuthStore.getState().markInitialized();
      }
    })();

    return () => {
      cancelled = true;
    };
  }, []);

  // ---- 回到前台 / 网络恢复时做增量同步 ----
  //
  // 这是"省流量"最关键的一个时机：用户切去看别的标签页、几分钟后再回来，
  // 此时只拉增量（而不是重新拉全量）。
  useEffect(() => {
    const onWake = (): void => {
      if (document.visibilityState !== 'visible') return;

      void (async () => {
        const result = await runIncrementalSync(fetchIncrementalPage);
        if (!result.changed) return;

        // 读到了新水位才推进游标；中途异常时 runIncrementalSync 会抛出，
        // 游标保持不变 → 下次仍从旧水位重来（宁可多拉，绝不漏拉）。
        if (result.cursor) writeArticleCursor(result.cursor);

        await Promise.all([
          queryClient.invalidateQueries({ queryKey: ['articles'] }),
          queryClient.invalidateQueries({ queryKey: ['categories'] }),
        ]);
      })().catch(() => {
        // 增量失败（离线/500）静默忽略：已有列表仍可读，下次唤醒再试。
      });
    };

    document.addEventListener('visibilitychange', onWake);
    window.addEventListener('online', onWake);
    return () => {
      document.removeEventListener('visibilitychange', onWake);
      window.removeEventListener('online', onWake);
    };
  }, [queryClient]);
}