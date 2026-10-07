/**
 * 语音播报（服务端 TTS 合成 + 本地播放）。
 *
 * ============================ 客户端不做 TTS ============================
 * openapi 明确边界：音频由服务端合成，客户端只播放。因此流程是：
 *   1. 先查缓存 `GET /api/v1/articles/{id}/audio`（**不触发合成**，未命中返回 404）；
 *   2. 未命中 → `POST /api/v1/audio/tasks` 提交合成
 *      （200 = 缓存命中 ready；202 = 已入队 pending/processing）；
 *   3. pending/processing → 按 openapi 建议的 **1s → 2s → 4s 退避**轮询
 *      `GET /api/v1/audio/tasks/{taskId}`，总时长上限 30s；
 *   4. ready → 用 audioId 拉元数据 → 交给 <audio src> 播放；
 *   5. failed → 展示 errorMsg，并提供 `POST /audio/tasks/{taskId}/retry` 重试。
 *
 * ★ 为什么先查缓存再提交：POST 已声明"命中缓存直接返回 ready"，
 *   但多一次 GET 能避免在已就绪时产生无意义的 POST；
 *   更关键的是 GET 不触发合成，可安全用于"只查不建"的场景。
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { ApiError } from '@/api/client';
import { audioFileUrl, createAudioTask, getArticleAudio, getAudioTask, retryAudioTask } from '@/api/endpoints';
import type { AudioDTO } from '@/api/types';

/** 轮询退避序列（openapi `getAudioTask` 描述：1s → 2s → 4s 退避，上限 30s）。 */
const POLL_BACKOFF_MS = [1000, 2000, 4000] as const;
/** 轮询总时长上限。 */
const POLL_TIMEOUT_MS = 30_000;

export type SpeechPhase =
  | 'idle'
  | 'checking'
  | 'submitting'
  | 'synthesizing'
  | 'ready'
  | 'failed';

export interface UseArticleAudioResult {
  phase: SpeechPhase;
  /** 可直接播放的音频 URL；未就绪为 null。 */
  audioUrl: string | null;
  /** 音频元数据（用于展示时长/大小）。 */
  audio: AudioDTO | null;
  error: string | null;
  /** 进度描述，如"合成中…（已查询 3 次，约 7 秒）"。 */
  progressText: string | null;
  /** 触发合成；就绪后返回可播放 URL。 */
  play: () => Promise<string | null>;
  /** 失败后重试。 */
  retry: () => Promise<string | null>;
}

/** 合成态的中文描述。 */
export const SPEECH_PHASE_TEXT: Record<SpeechPhase, string> = {
  idle: '',
  checking: '查询缓存…',
  submitting: '提交合成任务…',
  synthesizing: '服务端合成中…',
  ready: '',
  failed: '',
};

/**
 * 请求某篇文章的音频。
 *
 * @param articleId 文章 id
 * @param voice 音色（来自偏好 `tts.voice`）
 * @param speed 语速（来自偏好 `tts.speed`，openapi 约束 [0.5, 2.0]）
 */
export function useArticleAudio(articleId: number, voice: string, speed: number): UseArticleAudioResult {
  const [phase, setPhase] = useState<SpeechPhase>('idle');
  const [audio, setAudio] = useState<AudioDTO | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [progressText, setProgressText] = useState<string | null>(null);
  const queryClient = useQueryClient();

  /** 组件是否已卸载。 */
  const unmounted = useRef(false);
  /** 当前正在处理的 articleId：防止上一篇的异步结果污染当前文章。 */
  const currentId = useRef(articleId);
  /** 最近一次的合成 taskId，供 retry 使用（服务端 retry 端点需要）。 */
  const lastTaskId = useRef<number | null>(null);

  useEffect(() => {
    currentId.current = articleId;
    lastTaskId.current = null;
    // 切换文章时重置为 idle。
    setPhase('idle');
    setAudio(null);
    setError(null);
    setProgressText(null);
  }, [articleId]);

  useEffect(
    () => () => {
      unmounted.current = true;
    },
    [],
  );

  /** 状态写入守卫：已卸载或文章已切换则丢弃，并返回 false 表示应中止。 */
  const alive = useCallback((id: number): boolean => !unmounted.current && currentId.current === id, []);

  /** 拉取音频元数据；失败时降级为"仅知道 audioId"的最小可播放状态。 */
  const loadMeta = useCallback(
    async (id: number, audioId: number): Promise<string> => {
      const fallbackUrl = audioFileUrl(audioId);
      try {
        const meta = await getArticleAudio(id, voice, speed);
        if (!alive(id)) return fallbackUrl;
        setAudio(meta);
        return meta.url ?? fallbackUrl;
      } catch {
        // 元数据拉取失败不阻断播放：<audio> 直接用 /api/v1/audio/{id} 也能播。
        if (!alive(id)) return fallbackUrl;
        setAudio({
          id: audioId,
          articleId: id,
          voice,
          speed,
          format: 'mp3',
          durationMs: 0,
          sizeBytes: 0,
          sampleRate: 0,
          provider: null,
          url: fallbackUrl,
          createdAt: new Date().toISOString(),
        });
        return fallbackUrl;
      }
    },
    [voice, speed, alive],
  );

  /** 退避轮询直到 ready / failed / 超时。 */
  const poll = useCallback(
    async (id: number, taskId: number): Promise<string | null> => {
      const started = Date.now();
      let attempt = 0;
      setPhase('synthesizing');

      for (;;) {
        if (!alive(id)) return null;

        // 退避：1s → 2s → 4s → 4s → …
        const delay = POLL_BACKOFF_MS[Math.min(attempt, POLL_BACKOFF_MS.length - 1)] ?? 4000;
        attempt += 1;
        await new Promise<void>((resolve) => window.setTimeout(resolve, delay));

        if (!alive(id)) return null;

        if (Date.now() - started > POLL_TIMEOUT_MS) {
          setPhase('failed');
          setError('合成超时，可稍后重试');
          return null;
        }

        let task;
        try {
          task = await getAudioTask(taskId);
        } catch (err) {
          if (err instanceof ApiError && err.status === 404) {
            setPhase('failed');
            setError('合成任务不存在');
            return null;
          }
          // 瞬时网络抖动：连续失败 4 次才判负，避免一次抖动就前功尽弃。
          if (attempt >= 4) {
            setPhase('failed');
            setError(err instanceof ApiError ? err.message : '查询合成状态失败，请检查网络');
            return null;
          }
          continue;
        }

        if (!alive(id)) return null;

        const elapsedSec = Math.round((Date.now() - started) / 1000);
        setProgressText(`合成中…（已查询 ${attempt} 次，约 ${elapsedSec} 秒）`);

        if (task.status === 'ready') {
          setPhase('ready');
          setProgressText(null);
          if (task.audioId !== null) {
            await loadMeta(id, task.audioId);
          }
          void queryClient.invalidateQueries({ queryKey: ['audio'] });
          return null;
        }

        if (task.status === 'failed') {
          setPhase('failed');
          setProgressText(null);
          setError(task.errorMsg ?? task.errorCode ?? '语音合成失败');
          lastTaskId.current = taskId;
          return null;
        }
        // pending / processing → 继续轮询
      }
    },
    [alive, loadMeta, queryClient],
  );

  /** 触发合成；就绪后返回可播放 URL（未就绪返回 null）。 */
  const play = useCallback(async (): Promise<string | null> => {
    const id = articleId;
    if (!alive(id)) return null;

    setError(null);
    setProgressText(null);
    setPhase('checking');

    // ---------- 步骤 1：查缓存（不触发合成） ----------
    try {
      const cached = await getArticleAudio(id, voice, speed);
      if (!alive(id)) return null;
      setAudio(cached);
      setPhase('ready');
      return cached.url ?? audioFileUrl(cached.id);
    } catch (err) {
      if (!alive(id)) return null;
      // 404 = 尚未合成（正常路径）；网络错误也继续尝试提交（服务端可能已有缓存）。
      if (err instanceof ApiError && err.status === 401) {
        setPhase('failed');
        setError('登录状态已失效，请稍后重试');
        return null;
      }
    }

    // ---------- 步骤 2：提交合成 ----------
    setPhase('submitting');
    let task;
    try {
      task = await createAudioTask({ articleId: id, voice, speed });
    } catch (err) {
      if (!alive(id)) return null;
      setPhase('failed');
      setError(
        err instanceof ApiError
          ? // 429 = 合成队列已满：这是可退避的临时态，明确提示比"失败"更有用。
            err.code === 'RATE_LIMITED' || err.status === 429
            ? `合成队列繁忙${err.retryAfterSec ? `，请 ${err.retryAfterSec} 秒后重试` : '，请稍后重试'}`
            : err.message
          : '提交合成任务失败，请检查网络后重试',
      );
      return null;
    }

    if (!alive(id)) return null;
    lastTaskId.current = task.id;

    // ---------- 步骤 3：按状态机分支 ----------
    if (task.status === 'ready') {
      setPhase('ready');
      if (task.audioId !== null) await loadMeta(id, task.audioId);
      return audioFileUrl(task.audioId ?? 0);
    }
    if (task.status === 'failed') {
      setPhase('failed');
      setError(task.errorMsg ?? '语音合成失败');
      return null;
    }
    return poll(id, task.id);
  }, [articleId, voice, speed, alive, loadMeta, poll]);

  /** 失败后重试：优先用 retry 端点（重置 retry_count 并重新入队）。 */
  const retry = useCallback(async (): Promise<string | null> => {
    const id = articleId;
    const taskId = lastTaskId.current;
    if (taskId === null) return play();

    setError(null);
    setPhase('submitting');
    setProgressText('重新提交合成…');

    try {
      const task = await retryAudioTask(taskId);
      if (!alive(id)) return null;

      if (task.status === 'failed') {
        setPhase('failed');
        setProgressText(null);
        setError(task.errorMsg ?? '重试后仍然失败');
        return null;
      }
      if (task.status === 'ready') {
        setPhase('ready');
        setProgressText(null);
        if (task.audioId !== null) await loadMeta(id, task.audioId);
        return audioFileUrl(task.audioId ?? 0);
      }
      return poll(id, task.id);
    } catch (err) {
      if (!alive(id)) return null;
      setPhase('failed');
      setProgressText(null);
      setError(err instanceof ApiError ? err.message : '重试失败，请检查网络');
      return null;
    }
  }, [articleId, play, poll, loadMeta, alive]);

  const audioUrl = audio ? (audio.url ?? audioFileUrl(audio.id)) : null;

  return { phase, audioUrl, audio, error, progressText, play, retry };
}