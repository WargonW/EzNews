/**
 * 底部播放器条（<audio> 的控制层）。
 *
 * ============================ 客户端只播放 ============================
 * openapi 明确"客户端不做 TTS"，所以本组件**不含任何语音合成逻辑**，
 * 只负责：拿 URL → 交给 <audio> → 把播放状态映射为 UI。
 * 合成由 useArticleAudio（服务端交互）完成。
 *
 * 为什么用原生 <audio> 而不是 Web Audio API：
 * - 需要 Range 断点续传（服务端支持），原生元素自动处理；
 * - 需要浏览器级媒体控制（移动端锁屏控制、耳机按键）—— Web Audio 反而做不到；
 * - 包体与复杂度最小。
 */

import { useEffect, useRef } from 'react';
import { usePlayerStore, selectCurrentTrack } from '@/stores/player';
import { formatDuration } from '@/lib/datetime';
import { CloseIcon, NextIcon, PauseIcon, PlayIcon, PrevIcon, SpinnerIcon } from './icons';

export interface AudioPlayerBarProps {
  /** 当前条目的音频 URL；null 表示无音频可播。 */
  src: string | null;
  /** 合成中（显示进度而不可播）。 */
  loading?: boolean;
  /** 合成进度文案。 */
  progressText?: string | null;
}

export function AudioPlayerBar({ src, loading = false, progressText }: AudioPlayerBarProps): JSX.Element | null {
  const queue = usePlayerStore((s) => s.queue);
  const index = usePlayerStore((s) => s.index);
  const isPlaying = usePlayerStore((s) => s.isPlaying);
  const currentTimeMs = usePlayerStore((s) => s.currentTimeMs);
  const durationMs = usePlayerStore((s) => s.durationMs);
  const track = usePlayerStore(selectCurrentTrack);

  const audioRef = useRef<HTMLAudioElement>(null);

  const setPlaying = usePlayerStore((s) => s.setPlaying);
  const toggle = usePlayerStore((s) => s.toggle);
  const next = usePlayerStore((s) => s.next);
  const prev = usePlayerStore((s) => s.prev);
  const jumpTo = usePlayerStore((s) => s.jumpTo);
  const stop = usePlayerStore((s) => s.stop);
  const setCurrentTimeMs = usePlayerStore((s) => s.setCurrentTimeMs);
  const setDurationMs = usePlayerStore((s) => s.setDurationMs);

  // src 变化时重置进度（切换到另一条）。
  useEffect(() => {
    setCurrentTimeMs(0);
    setDurationMs(0);
  }, [src, setCurrentTimeMs, setDurationMs]);

  // 播放状态 → <audio>.play()/pause()
  useEffect(() => {
    const el = audioRef.current;
    if (!el || !src) return;
    if (isPlaying) {
      // play() 返回 Promise，用户可能因自动播放策略被拒 —— 捕获避免 unhandled rejection。
      void el.play().catch(() => setPlaying(false));
    } else {
      el.pause();
    }
  }, [isPlaying, src, setPlaying]);

  // 队列播完自动停。
  useEffect(() => {
    if (index >= queue.length && queue.length > 0) setPlaying(false);
  }, [index, queue.length, setPlaying]);

  if (!track && !loading) return null;

  // 优先用 <audio> 元素读到的真实时长，回退到合成时服务端给的 durationMs。
  const total = durationMs > 0 ? durationMs : (track?.durationMsHint ?? 0);
  const pct = total > 0 ? Math.min(100, (currentTimeMs / total) * 100) : 0;

  return (
    <>
      {/* 真正播放音频的元素。放在可视区域外，只作为播放引擎。 */}
      <audio
        ref={audioRef}
        src={src ?? undefined}
        preload="metadata"
        onTimeUpdate={(e) => setCurrentTimeMs(e.currentTarget.currentTime * 1000)}
        onLoadedMetadata={(e) => {
          const d = e.currentTarget.duration;
          // 部分服务端返回的 mp3 缺少时长元数据，duration 会是 Infinity/NaN。
          if (Number.isFinite(d) && d > 0) setDurationMs(d * 1000);
        }}
        onEnded={next}
        onError={() => setPlaying(false)}
      />

      <div className="fixed inset-x-0 bottom-0 z-30 border-t border-line bg-surface-raised/95 backdrop-blur-md">
        {/* 进度条 */}
        {src ? (
          <div
            className="h-0.5 w-full bg-surface-sunken"
            role="progressbar"
            aria-label="播放进度"
            aria-valuenow={Math.round(pct)}
            aria-valuemin={0}
            aria-valuemax={100}
          >
            <div className="h-full bg-brand transition-[width] duration-150" style={{ width: `${pct}%` }} />
          </div>
        ) : null}

        <div className="mx-auto flex max-w-[1600px] items-center gap-3 px-3 py-2 sm:gap-4 sm:px-5">
          {/* 封面：用源图标占位，避免额外请求 */}
          <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-brand-soft text-sm font-bold text-brand dark:text-blue-200">
            EZ
          </div>

          <div className="min-w-0 flex-1">
            {loading ? (
              <p className="flex items-center gap-1.5 truncate text-[13px] text-ink-muted">
                <SpinnerIcon />
                {progressText ?? '准备语音…'}
              </p>
            ) : (
              <>
                <p className="truncate text-[13px] font-medium text-ink" title={track?.title}>
                  {track?.title ?? ''}
                </p>
                <p className="mt-0.5 text-[11px] tabular-nums text-ink-faint">
                  {formatDuration(currentTimeMs)} / {total > 0 ? formatDuration(total) : '--:--'}
                  {track ? ` · 语速 ${track.speed}×` : ''}
                </p>
              </>
            )}
          </div>

          {/* 控制区 */}
          <div className="flex shrink-0 items-center gap-1">
            {queue.length > 1 ? (
              <button
                type="button"
                onClick={prev}
                disabled={loading}
                className="ez-btn ez-btn-sm ez-btn-ghost h-9 w-9 p-0"
                title="上一条"
                aria-label="上一条"
              >
                <PrevIcon className="text-base" />
              </button>
            ) : null}

            <button
              type="button"
              onClick={toggle}
              disabled={loading || !src}
              className="ez-btn ez-btn-md h-10 w-10 rounded-full bg-brand p-0 text-white shadow-card hover:bg-blue-700 disabled:opacity-40 dark:hover:bg-blue-300 dark:hover:text-slate-950"
              title={isPlaying ? '暂停' : '播放'}
              aria-label={isPlaying ? '暂停' : '播放'}
            >
              {isPlaying ? <PauseIcon className="text-sm" /> : <PlayIcon className="ml-0.5 text-sm" />}
            </button>

            {queue.length > 1 ? (
              <button
                type="button"
                onClick={next}
                disabled={loading}
                className="ez-btn ez-btn-sm ez-btn-ghost h-9 w-9 p-0"
                title="下一条"
                aria-label="下一条"
              >
                <NextIcon className="text-base" />
              </button>
            ) : null}
          </div>

          {/* 队列（>1 时显示可点条目） */}
          {queue.length > 1 ? (
            <select
              value={index}
              onChange={(e) => jumpTo(Number(e.target.value))}
              className="ez-input hidden h-9 w-40 shrink-0 truncate md:block"
              aria-label="播放队列"
            >
              {queue.map((t, i) => (
                <option key={t.articleId} value={i}>
                  {i + 1}. {t.title}
                </option>
              ))}
            </select>
          ) : null}

<button
            type="button"
            onClick={stop}
            disabled={loading}
            className="ez-btn ez-btn-sm ez-btn-ghost h-9 w-9 shrink-0 p-0"
            title="关闭播放器"
            aria-label="关闭播放器"
          >
            <CloseIcon className="text-base" />
          </button>
        </div>
      </div>
    </>
  );
}