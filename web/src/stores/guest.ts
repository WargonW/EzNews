/**
 * 游客态本地数据（zustand + localStorage 持久化）。
 *
 * 承载 PRD-ACCOUNT §1 要求的「游客可用」三项本地能力：
 * - 收藏（localFavorite）
 * - 已读（localRead）
 * - 偏好（voice / speed / theme）
 *
 * ============================ 墓碑语义 ============================
 * 收藏/已读都是 **带墓碑的 Map<articleId, {updatedAt: number, deleted: boolean}>>**，
 * 而不是 Set。因为 Merge Policy v1 依赖墓碑表达"游客态取消过"：
 *   若本地只存 Set，合并时无法区分"从未收藏"与"明确取消"，
 *   后者会被云端的旧正向记录复活（US-ACC-04 明确要求不能复活）。
 * 墓碑会随 MergeItem.deleted=true 一并上传。
 *
 * `updatedAt` 用 **epoch 毫秒**，与 openapi MergeItem.updatedAt / StateItem.updatedAt
 * 同型（integer），并按 MEMORY.md 的要求做钳制（见 clampClientTimestamp）。
 */

import { create } from 'zustand';
import type { MergeItem, PreferenceKV, StateItem } from '@/api/types';
import { StorageKey, readJson, writeJson, removeKey } from '@/lib/storage';

/** 单条本地状态记录（含墓碑）。 */
export interface LocalStateEntry {
  /** epoch 毫秒。LWW 判定依据。 */
  updatedAt: number;
  /** true = 墓碑（已取消）。 */
  deleted: boolean;
}

/** articleId -> 状态记录。 */
export type LocalStateMap = Record<number, LocalStateEntry>;

/**
 * 批量操作前的状态快照：`articleId -> 改动前的条目`。
 *
 * 值可能是 `undefined`，表示**改动前该键不存在**（而非"存在且为墓碑"）。
 * 这个区分很重要：回滚时前者要删键，后者要写回墓碑 ——
 * 搞混的话会给一条从未操作过的文章凭空造出墓碑，
 * 登录合并时它会以 `deleted=true` 上传，白白在云端留一条垃圾记录。
 */
export type LocalStateSnapshot = Record<number, LocalStateEntry | undefined>;

/** 单条状态的序列化形态（Map 的键是数字，JSON 只支持字符串键）。 */
type SerializedStateMap = Record<string, LocalStateEntry>;

const MAX_CLOCK_SKEW_MS = 7 * 24 * 60 * 60 * 1000; // 7 天，与 MEMORY.md 一致

/**
 * 钳制客户端时间戳。
 *
 * MEMORY.md 明确的陷阱：合并时必须钳制客户端时间戳，
 * `updatedAt <= 0` 或超前 > 7 天一律压到"此刻"。
 * 原因：客户端时钟不准会让本地数据**永久赢过服务端**，之后服务端怎么改都同步不下去，
 * 且重装无法收敛——这是个用户看不见但永远修不好的坏状态。
 */
export function clampClientTimestamp(ts: number, now = Date.now()): number {
  if (!Number.isFinite(ts) || ts <= 0) return now;
  if (ts > now + MAX_CLOCK_SKEW_MS) return now;
  // 过早的时间戳同样可疑（1970 附近），压到 now 会让它变成"最新"，
  // 反而可能覆盖云端正确值；因此对极端过旧值也压到 now，由服务端 LWW 正常处理。
  return ts;
}

/** 反序列化时校验形状，非法的项直接丢弃（本地数据不可信）。 */
function reviveStateMap(raw: unknown): LocalStateMap | null {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return null;
  const out: LocalStateMap = {};
  for (const [k, v] of Object.entries(raw as SerializedStateMap)) {
    const id = Number(k);
    if (!Number.isInteger(id) || id <= 0) continue;
    if (typeof v !== 'object' || v === null) continue;
    const entry = v as Partial<LocalStateEntry>;
    if (typeof entry.updatedAt !== 'number' || typeof entry.deleted !== 'boolean') continue;
    out[id] = { updatedAt: entry.updatedAt, deleted: entry.deleted };
  }
  return out;
}

/** 序列化：把数字键转为字符串键。 */
function serializeStateMap(map: LocalStateMap): SerializedStateMap {
  const out: SerializedStateMap = {};
  for (const [id, entry] of Object.entries(map)) out[id] = entry;
  return out;
}

/**
 * 偏好默认值。
 *
 * ★ key 命名遵循 openapi `PreferenceKV.description` 的约定：**键含命名空间点号**
 *   （契约示例为 `tts.voice` / `tts.speed` / `ui.density`）。
 *   服务端实现是 `map[string]string` 不校验 key，但用带命名空间的 key 才能
 *   与契约示例一致，也避免将来与其它偏好域（如阅读器 ui.*）撞名。
 *
 * ⚠️ 契约漂移：openapi 声明 value 可为 `string|number|boolean|null`，
 *   服务端实现是 `map[string]string`。因此这里一律用字符串下发，
 *   数字/布尔在读取时用 prefNumber / prefBool 解析（见下方）。
 */
export const DEFAULT_PREFERENCES = {
  /** 音色 id（腾讯云 TTS 音色编号字符串）。 */
  'tts.voice': '101001',
  /** 语速，openapi 约束 [0.5, 2.0]。 */
  'tts.speed': '1.0',
  /** 列表密度：comfortable | compact。 */
  'ui.density': 'comfortable',
  /** 是否自动播放下一条（连续播报）。 */
  'player.autoNext': 'false',
  /**
   * 「全部新闻」视图下被本地隐藏的源 id 集合。
   *
   * ★ 只影响**客户端浏览过滤**（见 useArticleFeed 的 exclude 参数），
   *   不改变服务端 `enabled`；因此它与「管理来源」里的真·启停是两个语义，
   *   刻意分开存储，避免用户误以为隐藏 = 停用了源。
   * ★ 序列化用逗号分隔的十进制 id（如 "1,7,12"），空串 = 未隐藏任何源。
   *   选它而不是 JSON 数组：PreferenceKV 的值只能是字符串，而逗号串
   *   在服务端 `map[string]string` 与 localStorage 里都是原样保留的最简形态，
   *   且解析可复用下面的 prefIdSet（对脏数据足够宽容）。
   */
  'feed.hiddenSources': '',
} as const satisfies PreferenceKV;

export type PreferenceKey = keyof typeof DEFAULT_PREFERENCES;

interface GuestState {
  favorites: LocalStateMap;
  reads: LocalStateMap;
  preferences: PreferenceKV;

  /** 读取/设置收藏态，返回切换后的 `deleted`（true = 现已取消收藏）。 */
  toggleFavorite: (articleId: number) => boolean;
  /** 读取/设置已读态，返回切换后的 `deleted`。 */
  toggleRead: (articleId: number) => boolean;
  /**
   * 批量标记已读（"全部标记已读"）。
   *
   * ★ 返回**改动前的快照**（只含被改动的键），供调用方在服务端失败时回滚。
   *   快照里某个键的值可能是 `undefined` —— 表示"改动前这条根本不存在"，
   *   回滚时应当把它整个删掉，而不是写成一条墓碑。
   */
  markAllRead: (articleIds: readonly number[]) => LocalStateSnapshot;
  /** 回滚 `markAllRead`：按快照逐键还原（`undefined` = 删除该键）。 */
  rollbackReads: (snapshot: LocalStateSnapshot) => void;
  /** 写入单个偏好。 */
  setPreference: (key: string, value: string) => void;
  /** 批量写入偏好。 */
  setPreferences: (kv: PreferenceKV) => void;

  /** 登录合并成功后用云端数据覆盖本地。 */
  applyCloudState: (favorites: StateItem[], reads: StateItem[], preferences: PreferenceKV) => void;
  /** 登出 / 合并成功后清空游客态，避免下次登录重复合并（PRD §4.6）。 */
  resetGuestState: () => void;

  /** 是否已收藏（严格：墓碑不算收藏）。 */
  isFavorited: (articleId: number) => boolean;
  /** 是否已读。 */
  isRead: (articleId: number) => boolean;

  /** 导出为 merge 请求体（含墓碑，时间戳已钳制）。 */
  toMergeItems: (kind: 'favorites' | 'reads') => MergeItem[];
}

/**
 * 早期版本用的是不带命名空间的裸 key（`voice`/`speed`/`autoNext`），
 * 而 openapi `PreferenceKV.description` 约定"键含命名空间点号"。
 * 这里做一次性迁移，避免老用户升级后偏好被静默重置为默认值。
 */
const LEGACY_PREF_KEYS: ReadonlyArray<readonly [legacy: string, current: PreferenceKey]> = [
  ['voice', 'tts.voice'],
  ['speed', 'tts.speed'],
  ['autoNext', 'player.autoNext'],
  ['density', 'ui.density'],
];

/** 读偏好（带默认值兜底 + 旧 key 迁移）。 */
function readPreferences(): PreferenceKV {
  const stored = readJson<PreferenceKV>(StorageKey.guestPreferences, (raw) => {
    if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return null;
    const out: PreferenceKV = {};
    for (const [k, v] of Object.entries(raw as Record<string, unknown>)) {
      // openapi 声明 value 可为 string|number|boolean|null，
      // 但服务端实现是 map[string]string —— 统一收敛为字符串下发。
      if (v === null) continue;
      out[k] = typeof v === 'string' ? v : String(v);
    }
    return out;
  });

  if (stored) {
    let migrated = false;
    for (const [legacy, current] of LEGACY_PREF_KEYS) {
      const v = stored[legacy];
      if (v === undefined) continue;
      // 只在新 key 缺失时迁移，并把旧 key 从 KV 里摘掉。
      if (stored[current] === undefined) stored[current] = v;
      delete stored[legacy];
      migrated = true;
    }
    if (migrated) writeJson(StorageKey.guestPreferences, { ...DEFAULT_PREFERENCES, ...stored });
  }

  return { ...DEFAULT_PREFERENCES, ...(stored ?? {}) };
}

/** 把偏好值解析为数字，非法时回落到默认。 */
export function prefNumber(prefs: PreferenceKV, key: PreferenceKey, fallback: number): number {
  const n = Number.parseFloat(prefs[key] ?? '');
  return Number.isFinite(n) ? n : fallback;
}

/** 取字符串偏好；缺失或空串时回落到默认。 */
export function prefString(prefs: PreferenceKV, key: PreferenceKey, fallback: string): string {
  const v = prefs[key];
  return v !== undefined && v !== '' ? v : fallback;
}

/** 把偏好值解析为布尔。 */
export function prefBool(prefs: PreferenceKV, key: PreferenceKey, fallback = false): boolean {
  const v = prefs[key];
  if (v === undefined) return fallback;
  return v === 'true' || v === '1';
}

/**
 * 把偏好值解析为正整数 id 集合（逗号分隔，如 "1,7,12"）。
 *
 * ★ 用于 `feed.hiddenSources`。对脏数据宽容：非正整数、空段一律丢弃，
 *   重复项自然去重 —— 偏好是从本地/云端任意来源读回的字符串，
 *   不能假设它一定是我们写出去的那份（例如被手工改过、或旧版本遗留格式）。
 * 解析失败的段**跳过而不是整体失败**：丢一个 id 只是少隐藏一个源，
 * 整体失败则会让用户"隐藏了却全冒出来"，后者更糟。
 */
export function prefIdSet(prefs: PreferenceKV, key: PreferenceKey): ReadonlySet<number> {
  const raw = prefs[key];
  if (raw === undefined || raw === '') return new Set<number>();
  const out = new Set<number>();
  for (const part of raw.split(',')) {
    const id = Number(part.trim());
    if (Number.isInteger(id) && id > 0) out.add(id);
  }
  return out;
}

/** 序列化正整数 id 集合为偏好值（升序，便于比对与阅读）。 */
export function serializeIdSet(ids: ReadonlySet<number>): string {
  return [...ids].sort((a, b) => a - b).join(',');
}

export const useGuestStore = create<GuestState>((set, get) => ({
  favorites: readJson<LocalStateMap>(StorageKey.guestFavorites, reviveStateMap) ?? {},
  reads: readJson<LocalStateMap>(StorageKey.guestReads, reviveStateMap) ?? {},
  preferences: readPreferences(),

  toggleFavorite: (articleId) => {
    const now = clampClientTimestamp(Date.now());
    const cur = get().favorites;
    const wasDeleted = cur[articleId]?.deleted ?? true; // 从未收藏视为 deleted=true
    const next: LocalStateMap = {
      ...cur,
      [articleId]: { updatedAt: now, deleted: !wasDeleted },
    };
    set({ favorites: next });
    writeJson(StorageKey.guestFavorites, serializeStateMap(next));
    return !wasDeleted;
  },

  toggleRead: (articleId) => {
    const now = clampClientTimestamp(Date.now());
    const cur = get().reads;
    const wasDeleted = cur[articleId]?.deleted ?? true;
    const next: LocalStateMap = {
      ...cur,
      [articleId]: { updatedAt: now, deleted: !wasDeleted },
    };
    set({ reads: next });
    writeJson(StorageKey.guestReads, serializeStateMap(next));
    return !wasDeleted;
  },

  /**
   * 批量标记已读。
   *
   * ============================ 为什么只写"真正变化的"条目 ============================
   * 已读的条目**原样跳过**，不刷新它的 `updatedAt`。理由：
   *   - `updatedAt` 是 LWW 的判据（见 Merge Policy v1），无意义地把它推到现在
   *     会让这些条目在下次增量同步里被当成"变了"回传，白跑一趟流量；
   *   - 单条 `toggleRead` 做不到这件事（它必须翻转），所以这是批量场景特有的优化。
   * 也因此，返回的快照只含**实际改动**的键 —— 没动过的键回滚时也不该碰。
   *
   * 非法 id（<= 0 / 非整数）直接丢弃，与服务端 `BatchSet` 的清洗逻辑对齐，
   * 避免本地留下一个服务端根本不认的键。
   */
  markAllRead: (articleIds) => {
    const now = clampClientTimestamp(Date.now());
    const cur = get().reads;
    const next: LocalStateMap = { ...cur };
    const snapshot: LocalStateSnapshot = {};

    for (const id of articleIds) {
      if (!Number.isInteger(id) || id <= 0) continue;
      const prev = cur[id];
      // 已读（非墓碑）→ 已经是目标态，跳过，避免无谓的 updatedAt 推进。
      if (prev !== undefined && !prev.deleted) continue;
      snapshot[id] = prev; // 可能是 undefined（改动前不存在）
      next[id] = { updatedAt: now, deleted: false };
    }

    if (Object.keys(snapshot).length === 0) return snapshot;

    set({ reads: next });
    writeJson(StorageKey.guestReads, serializeStateMap(next));
    return snapshot;
  },

  rollbackReads: (snapshot) => {
    const cur = get().reads;
    const next: LocalStateMap = { ...cur };

    for (const [id, entry] of Object.entries(snapshot) as Array<[string, LocalStateEntry | undefined]>) {
      const key = Number(id);
      if (entry === undefined) delete next[key]; // 改动前不存在 → 还原为不存在
      else next[key] = entry; // 改动前是墓碑 → 原样写回
    }

    set({ reads: next });
    writeJson(StorageKey.guestReads, serializeStateMap(next));
  },

  setPreference: (key, value) => {
    const next = { ...get().preferences, [key]: value };
    set({ preferences: next });
    writeJson(StorageKey.guestPreferences, next);
  },

  setPreferences: (kv) => {
    const next = { ...get().preferences, ...kv };
    set({ preferences: next });
    writeJson(StorageKey.guestPreferences, next);
  },

  /**
   * 用云端数据覆盖本地（登录合并完成、或登录态下全量/增量拉取后）。
   *
   * ★ LWW：本地 updatedAt 更新则保留本地，否则采用云端。
   * ★ 墓碑：云端 deleted=true 且胜出 → 本地移除该条（DEC-11）。
   */
  applyCloudState: (favorites, reads, preferences) => {
    const now = clampClientTimestamp(Date.now());

    const mergeOne = (local: LocalStateMap, remote: StateItem[]): LocalStateMap => {
      const next: LocalStateMap = { ...local };
      for (const item of remote) {
        const id = item.articleId;
        if (!Number.isInteger(id) || id <= 0) continue;
        const cur = next[id];
        const remoteTs = clampClientTimestamp(item.updatedAt, now);
        if (cur && cur.updatedAt > remoteTs) continue; // LWW：本地更新，保留
        if (item.deleted) delete next[id]; // 墓碑胜出 → 本地移除
        else next[id] = { updatedAt: remoteTs, deleted: false };
      }
      return next;
    };

    const fav = mergeOne(get().favorites, favorites);
    const rd = mergeOne(get().reads, reads);
    const prefs = { ...get().preferences, ...preferences };

    set({ favorites: fav, reads: rd, preferences: prefs });
    writeJson(StorageKey.guestFavorites, serializeStateMap(fav));
    writeJson(StorageKey.guestReads, serializeStateMap(rd));
    writeJson(StorageKey.guestPreferences, prefs);
  },

  resetGuestState: () => {
    removeKey(StorageKey.guestFavorites);
    removeKey(StorageKey.guestReads);
    removeKey(StorageKey.mergeNonce);
    set({
      favorites: {},
      reads: {},
      preferences: { ...DEFAULT_PREFERENCES },
    });
    writeJson(StorageKey.guestPreferences, DEFAULT_PREFERENCES);
  },

  isFavorited: (articleId) => {
    const e = get().favorites[articleId];
    return e !== undefined && !e.deleted;
  },

  isRead: (articleId) => {
    const e = get().reads[articleId];
    return e !== undefined && !e.deleted;
  },

  toMergeItems: (kind) => {
    const map = kind === 'favorites' ? get().favorites : get().reads;
    const now = clampClientTimestamp(Date.now());
    return Object.entries(map).map(([id, entry]) => ({
      articleId: Number(id),
      deleted: entry.deleted,
      // 钳制：防止本地时钟不准让数据永久赢过云端。
      updatedAt: clampClientTimestamp(entry.updatedAt, now),
    }));
  },
}));

/**
 * ============================ 注意：不要把下面两个函数直接当 selector 用 ============================
 *
 * 它们**每次调用都返回新的 Set 实例**。React 18 的 `useSyncExternalStore`
 * 要求 `getSnapshot` 返回值在状态未变时保持引用稳定，否则会判定为"一直在变"，
 * 进而进入无限重渲染（Maximum update depth exceeded）。
 *
 * 所以组件里必须先订阅**原始的 state 引用**（favorites / reads 本身是稳定引用），
 * 再用下面两个纯函数在 useMemo 里派生 id 集合。
 */

/** 从 favorites map 派生收藏 id 集合（严格排除墓碑）。 */
export function favoriteIdsOf(favorites: LocalStateMap): ReadonlySet<number> {
  return new Set(
    Object.entries(favorites)
      .filter(([, e]) => !e.deleted)
      .map(([id]) => Number(id)),
  );
}

/** 从 reads map 派生已读 id 集合。 */
export function readIdsOf(reads: LocalStateMap): ReadonlySet<number> {
  return new Set(
    Object.entries(reads)
      .filter(([, e]) => !e.deleted)
      .map(([id]) => Number(id)),
  );
}