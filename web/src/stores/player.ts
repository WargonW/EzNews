/**
 * 播放器状态（zustand）。
 *
 * 只管"当前在播哪一条 + 播放进度"这类 UI 状态；
 * **合成任务的状态机在 useArticleAudio 里**（那是服务端交互）。
 *
 * 音频 URL 由服务端合成后给出（客户端不做 TTS，openapi 明确边界）：
 * `/api/v1/audio/{audioId}`，支持 Range 断点续传，可直接喂给 <audio src>。
 */

import { create } from 'zustand';

/** 播放器中的条目。 */
export interface PlayerTrack {
  articleId: number;
  title: string;
  /** 可直接播放的音频 URL。 */
  url: string;
  /** 合成时使用的音色与语速（用于展示与"重合成"判断）。 */
  voice: string;
  speed: number;
  /** 服务端 AudioDTO.durationMs（0 = 未知）。 */
  durationMsHint?: number;
}

interface PlayerState {
  /** 当前播放队列（连续播报）。 */
  queue: PlayerTrack[];
  /** 队列内当前索引。 */
  index: number;
  isPlaying: boolean;
  currentTimeMs: number;
  durationMs: number;
  /** 音量 0~1。 */
  volume: number;

  /** 设置队列并从指定条目开始播放。 */
  play: (queue: PlayerTrack[], startIndex: number) => void;
  /** 播放/暂停切换。 */
  toggle: () => void;
  setPlaying: (playing: boolean) => void;
  next: () => void;
  prev: () => void;
  /** 跳到指定队列条目。 */
  jumpTo: (index: number) => void;
  /** 从队列移除某条目（收藏取消/删除文章时用）。 */
  removeFromQueue: (articleId: number) => void;
  /** 清空队列并停止播放。 */
  stop: () => void;

  setCurrentTimeMs: (ms: number) => void;
  setDurationMs: (ms: number) => void;
  setVolume: (v: number) => void;
}

export const usePlayerStore = create<PlayerState>((set, get) => ({
  queue: [],
  index: -1,
  isPlaying: false,
  currentTimeMs: 0,
  durationMs: 0,
  volume: 1,

  play: (queue, startIndex) => {
    if (queue.length === 0) return;
    const idx = Math.max(0, Math.min(startIndex, queue.length - 1));
    set({
      queue,
      index: idx,
      isPlaying: true,
      currentTimeMs: 0,
      durationMs: 0,
    });
  },

  toggle: () => set((s) => ({ isPlaying: !s.isPlaying })),

  setPlaying: (playing) => set({ isPlaying: playing }),

  next: () => {
    const { queue, index } = get();
    if (index + 1 < queue.length) {
      set({ index: index + 1, isPlaying: true, currentTimeMs: 0, durationMs: 0 });
    } else {
      // 队列播完：停在末尾并置为暂停（不自动循环，避免用户被持续打扰）。
      set({ isPlaying: false });
    }
  },

  prev: () => {
    const { index } = get();
    if (index - 1 >= 0) {
      set({ index: index - 1, isPlaying: true, currentTimeMs: 0, durationMs: 0 });
    } else {
      set({ currentTimeMs: 0 });
    }
  },

  jumpTo: (index) => {
    const { queue } = get();
    if (index < 0 || index >= queue.length) return;
    set({ index, isPlaying: true, currentTimeMs: 0, durationMs: 0 });
  },

  removeFromQueue: (articleId) => {
    const { queue, index } = get();
    const nextQueue = queue.filter((t) => t.articleId !== articleId);
    if (nextQueue.length === queue.length) return;
    // 删除当前播放项时，把索引收敛到相邻的下一条（删末条则退到上一条）。
    const removedIdx = queue.findIndex((t) => t.articleId === articleId);
    let nextIndex = index;
    if (removedIdx === index) {
      nextIndex = Math.min(removedIdx, nextQueue.length - 1);
    } else if (removedIdx < index) {
      nextIndex = index - 1;
    }
    set({
      queue: nextQueue,
      index: nextQueue.length === 0 ? -1 : Math.max(0, nextIndex),
      isPlaying: nextQueue.length === 0 ? false : get().isPlaying,
      currentTimeMs: 0,
      durationMs: 0,
    });
  },

  stop: () => set({ queue: [], index: -1, isPlaying: false, currentTimeMs: 0, durationMs: 0 }),

  setCurrentTimeMs: (ms) => set({ currentTimeMs: ms }),
  setDurationMs: (ms) => set({ durationMs: ms }),
  setVolume: (v) => set({ volume: Math.max(0, Math.min(1, v)) }),
}));

/** 当前播放条目（未播放时为 null）。 */
export const selectCurrentTrack = (s: PlayerState): PlayerTrack | null =>
  s.index >= 0 && s.index < s.queue.length ? (s.queue[s.index] ?? null) : null;