/**
 * ★ Merge Policy v1 —— 游客数据静默合并（客户端侧）。
 *
 * ============================ 关键设计 ============================
 * 1. **零打断**：登录成功后自动发起，无弹窗、无确认（PRD-ACCOUNT §4.2）。
 * 2. **nonce 幂等**：nonce 在"开始一批合并"时生成并**持久化**，
 *    直到该批成功才清除。网络超时重试时**必须沿用同一 nonce**，
 *    否则 merge_log 幂等键失效、同一批数据会被写入两次。
 *    服务端对收藏/已读有 `(user_id, article_id)` 唯一键兜底，
 *    但偏好整包覆盖 + 计数语义都依赖幂等，不能靠唯一键救。
 * 3. **失败不阻塞登录态**：合并失败只标 `pending` + 退避重试，
 *    应用功能完全可用（US-ACC-05）。
 * 4. **成功后清空游客态**：否则下次登录会重复上传同一批数据。
 * 5. **时间戳钳制**：见 guest.ts 的 clampClientTimestamp。
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { getClientId, getDeviceName } from '@/stores/auth';
import { useGuestStore } from '@/stores/guest';
import { mergeGuestData } from '@/api/endpoints';
import type { MergeRequest } from '@/api/types';
import { StorageKey, randomId, readString, removeKey, writeString } from '@/lib/storage';

/** 合并状态（供设置面板展示"待同步"标记，US-ACC-05）。 */
export type MergePhase = 'idle' | 'running' | 'done' | 'failed' | 'pending-retry';

/** 退避重试间隔序列（毫秒）。 */
const RETRY_BACKOFF_MS = [5_000, 15_000, 60_000, 300_000] as const;

/**
 * 读取或创建当前批次的 nonce。
 *
 * ★ 必须在"开始合并"时创建并持久化，重试时读取同一值。
 */
function acquireNonce(): string {
  const existing = readString(StorageKey.mergeNonce);
  if (existing && existing.length > 0 && existing.length <= 64) return existing;
  const fresh = randomId(22).slice(0, 22);
  writeString(StorageKey.mergeNonce, fresh);
  return fresh;
}

/** 该批成功：清除 nonce，允许下一批生成新键。 */
function releaseNonce(): void {
  removeKey(StorageKey.mergeNonce);
}

/** 是否还有待完成的合并批次（冷启动时用来自动重试，Merge Policy §4.2）。 */
export function hasPendingMerge(): boolean {
  return readString(StorageKey.mergeNonce) !== null;
}

export interface UseMergeGuestDataResult {
  phase: MergePhase;
  /** 手动/自动触发一次合并。 */
  merge: () => Promise<void>;
  /** 合并出的计数，用于"已同步 N 条收藏"轻提示（US-ACC-01）。 */
  lastResult: { favorites: number; reads: number } | null;
}

/**
 * 游客数据合并 hook。
 *
 * @param enabled 是否处于登录态（游客态不发起合并）。
 */
export function useMergeGuestData(enabled: boolean): UseMergeGuestDataResult {
  const [phase, setPhase] = useState<MergePhase>('idle');
  const [lastResult, setLastResult] = useState<{ favorites: number; reads: number } | null>(null);

  /** 防止 React 严格模式下的双调用（连续两次 merge 虽幂等，但会白跑一次请求）。 */
  const running = useRef(false);
  const retryTimer = useRef<number | null>(null);
  const retryIdx = useRef(0);

  const guest = useGuestStore();

  const runMerge = useCallback(async () => {
    if (running.current) return;
    running.current = true;
    setPhase('running');

    try {
      // 复用同一份 guest state 快照，避免合并过程中用户又点了收藏导致数据不一致。
      const favorites = useGuestStore.getState().toMergeItems('favorites');
      const reads = useGuestStore.getState().toMergeItems('reads');
      const preferences = useGuestStore.getState().preferences;

      const body: MergeRequest = {
        clientId: getClientId(),
        // ★ 复用同一 nonce：重试必须命中服务端的 merge_log 幂等键。
        nonce: acquireNonce(),
        favorites,
        reads,
        preferences,
      };

      const result = await mergeGuestData(body);

      // ---- 成功 ----
      releaseNonce();
      retryIdx.current = 0;

      // ★ 清空游客态，避免下次登录重复合并同一批数据。
      //
      // 注意这里**绝对不能**顺手调 putPreferences 把本地偏好写回服务端：
      // Merge Policy 规定「服务端有值以服务端为准」，本地值只在服务端**无值**时才生效，
      // 而该规则已由 merge 端点本身实现（preferenceApplied 标识是否采纳）。
      // 在合并后再整包覆盖会把服务端的多设备设置反向冲掉 —— 属于把增强功能改坏。
      //
      // 云端事实源由 useUserStateSync 拉取（收藏/已读增量 + 偏好全量）来落地。
      useGuestStore.getState().resetGuestState();

      setLastResult({ favorites: result.favorites.added, reads: result.reads.added });
      setPhase('done');
    } catch (err) {
      // 合并失败不影响登录态：标 pending 并退避重试（§4.2 / US-ACC-05）。
      // **nonce 不清除** —— 下一轮重试必须用同一 nonce，否则幂等失效。
      const idx = Math.min(retryIdx.current, RETRY_BACKOFF_MS.length - 1);
      const delay = RETRY_BACKOFF_MS[idx] ?? 30_000;
      retryIdx.current += 1;
      setPhase('failed');

      if (retryTimer.current !== null) window.clearTimeout(retryTimer.current);
      retryTimer.current = window.setTimeout(() => {
        retryTimer.current = null;
        void runMerge();
      }, delay);
      void err;
    } finally {
      running.current = false;
    }
  }, []);

  // 登录态成立且存在待完成批次 → 自动发起（零打断）。
  useEffect(() => {
    if (!enabled) return;
    void runMerge();
    return () => {
      if (retryTimer.current !== null) {
        window.clearTimeout(retryTimer.current);
        retryTimer.current = null;
      }
    };
  }, [enabled, runMerge]);

  // 组件卸载时清理定时器，避免内存泄漏与 setState-after-unmount。
  useEffect(
    () => () => {
      if (retryTimer.current !== null) window.clearTimeout(retryTimer.current);
    },
    [],
  );

  void guest; // 订阅 guest store 变化，重渲染时保持引用

  return { phase, merge: runMerge, lastResult };
}

/** 登录成功后需要携带的设备信息（供调用方组装 register/login 请求）。 */
export function mergeContextHints(): { clientId: string; deviceName: string } {
  return { clientId: getClientId(), deviceName: getDeviceName() };
}