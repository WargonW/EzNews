/**
 * 本地存储封装。
 *
 * 设计要点：
 * 1. **命名空间隔离**：所有 key 以 `eznews.` 前缀，避免与同域其它应用冲突。
 * 2. **绝不抛异常**：Safari 无痕模式 / 配额超限时 localStorage.setItem 会抛。
 *    存储是增强能力而非前提，读写失败必须降级为内存态而不是让整页白屏。
 * 3. **凭证不落盘**：accessToken 与 refreshToken **绝不**写入 localStorage。
 *    见 `assertNoCredentialLeak` 与 README 的安全说明。
 */

const NS = 'eznews.';

/** 本地存储可用的键。集中声明避免拼写漂移。 */
export const StorageKey = {
  /** 客户端实例标识（首次启动生成并持久化，reinstall 重置）。对应 MergeRequest.clientId。 */
  clientId: `${NS}clientId`,
  /** 当前待完成的 merge 批次（含 nonce），成功后才清除。对应 MergeRequest.nonce。 */
  mergeNonce: `${NS}mergeNonce`,
  /** 文章列表增量游标（服务端 syncCursor / nextCursor）。 */
  articleSyncCursor: `${NS}cursor.articles`,
  /** 收藏增量游标。 */
  favoriteSyncCursor: `${NS}cursor.favorites`,
  /** 已读增量游标。 */
  readSyncCursor: `${NS}cursor.reads`,
  /** 游客态收藏（含墓碑）。游客登录合并成功后清空。 */
  guestFavorites: `${NS}guest.favorites`,
  /** 游客态已读（含墓碑）。游客登录合并成功后清空。 */
  guestReads: `${NS}guest.reads`,
  /** 游客态偏好 KV。 */
  guestPreferences: `${NS}guest.preferences`,
  /** 主题偏好：system | light | dark。 */
  theme: `${NS}theme`,
  /** 上次同步的服务器时间戳（诊断用）。 */
  lastSyncAt: `${NS}lastSyncAt`,
} as const;

export type StorageKeyName = (typeof StorageKey)[keyof typeof StorageKey];

/**
 * localStorage 是否可用。
 * 部分环境下访问 localStorage 本身就会抛 SecurityError（如被禁用的 cookie 策略）。
 */
function getStore(): Storage | null {
  try {
    const s = window.localStorage;
    // 触发一次读写以确认真正可用（Safari 无痕下存在但 setItem 抛配额错）。
    const probe = `${NS}__probe__`;
    s.setItem(probe, '1');
    s.removeItem(probe);
    return s;
  } catch {
    return null;
  }
}

/** 读字符串；不存在或解析环境不支持时返回 null。 */
export function readString(key: StorageKeyName): string | null {
  const s = getStore();
  if (!s) return null;
  try {
    return s.getItem(key);
  } catch {
    return null;
  }
}

/**
 * 写字符串；失败静默降级（存储是增强能力，不该让功能崩溃）。
 *
 * ★ 凭证护栏在此层生效，而不是在调用方（authStore.applyTokens）。
 *   护栏的本意是「凭证绝不落盘」，那就必须守在**唯一真正写盘**的函数上。
 *   放在 authStore 里是错位：那里token 只进内存（zustand state），
 *   根本不碰 localStorage，却让开发模式下登录 100% 失败 ——
 *   一个纯内存操作被持久化护栏拦下，属于把无关功能打残。
 */
export function writeString(key: StorageKeyName, value: string): void {
  assertNoCredentialLeak(value);
  const s = getStore();
  if (!s) return;
  try {
    s.setItem(key, value);
  } catch {
    /* 配额超限 / 无痕模式：放弃持久化，功能仍可用（仅丢跨刷新持久性） */
  }
}

/** 删除键。 */
export function removeKey(key: StorageKeyName): void {
  const s = getStore();
  if (!s) return;
  try {
    s.removeItem(key);
  } catch {
    /* 同上 */
  }
}

/**
 * 读取并反序列化 JSON。
 * 返回 null 表示"无数据"或"数据损坏"——两者对调用方等价，都走默认值重建。
 */
export function readJson<T>(key: StorageKeyName, revive: (raw: unknown) => T | null): T | null {
  const raw = readString(key);
  if (raw === null) return null;
  try {
    return revive(JSON.parse(raw) as unknown);
  } catch {
    // 数据损坏：主动清理，避免每次启动都失败。
    removeKey(key);
    return null;
  }
}

/** 序列化并写入 JSON。 */
export function writeJson(key: StorageKeyName, value: unknown): void {
  try {
    writeString(key, JSON.stringify(value));
  } catch {
    /* 循环引用等：忽略 */
  }
}

/**
 * 生成 RFC4122 v4 风格的随机 ID。
 *
 * 用 `crypto.randomUUID`（需安全上下文），不可用时退化为
 * `crypto.getRandomValues`，再不可用才用 Math.random（此时仅影响幂等键强度，
 * 不影响功能，故只记 warn 不抛）。
 */
export function randomId(len = 22): string {
  const c: Crypto | undefined = typeof crypto !== 'undefined' ? crypto : undefined;

  if (c && typeof c.randomUUID === 'function') {
    return c.randomUUID();
  }

  const bytes = new Uint8Array(Math.ceil((len * 4) / 3));
  if (c && typeof c.getRandomValues === 'function') {
    c.getRandomValues(bytes);
  } else {
    for (let i = 0; i < bytes.length; i += 1) {
      bytes[i] = Math.floor(Math.random() * 256);
    }
  }
  // base64url 编码
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/**
 * 开发期护栏：确认没有任何凭证被写进 localStorage。
 * 只在 import.meta.env.DEV 下由 authStore 在写入前调用。
 */
export function assertNoCredentialLeak(value: unknown): void {
  if (!import.meta.env.DEV) return;
  if (typeof value === 'string' && value.split('.').length === 3) {
    // 形如 xxx.yyy.zzz 的三段串 —— 极可能是 JWT。
    // 这里的 console 是刻意保留的护栏日志：它只在 DEV 下触发，
    // 目的是让开发者一眼看到"凭证被误传进持久化"这个本该在开发期暴露的问题。
    // eslint-disable-next-line no-console
    console.warn('[eznews] 检测到疑似 JWT 被传入持久化存储，已阻止写入');
    throw new Error('refuse to persist credential');
  }
}