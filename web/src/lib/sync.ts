/**
 * 增量同步游标管理。
 *
 * ============================ 核心省流量设计 ============================
 * 服务端不推送（D3），客户端靠 `cursor` 主动拉增量（PRD-ACCOUNT DEC-11）。
 * 两种模式**互斥**（openapi `GET /api/v1/articles` 描述）：
 *
 *   - 不传 cursor → **首屏/翻页模式**：published_at DESC, id DESC，
 *     用 pageCursor 翻页；响应额外返回 `syncCursor`（服务端水位，now-2s 重叠）。
 *   - 传 cursor   → **增量同步模式**：updated_at ASC, id ASC，
 *     用 nextCursor 续拉，hasMore=false 结束。
 *
 * 客户端算法（openapi 明示）：
 *   1. 首次请求不带 cursor → 保存返回的 `syncCursor`；
 *   2. 之后带 `cursor=<syncCursor>` 循环拉取直到 hasMore=false，
 *      按 `id` 本地合并去重，并更新 syncCursor；
 *   3. 游标持久化到 localStorage —— 刷新页面不丢，下次启动直接增量而非全量。
 *
 * ★ **必须处理"游标为空"的情况**：本地游标被清 / 首次使用 / 换设备时，
 *   此时只能走全量（不带 cursor）。
 * ★ **必须处理"增量模式下一条都没有"**：hasMore=false 且 items 为空是合法的
 *   "无变化"，不是错误。
 * ★ **不能把增量结果直接当列表用**：增量是"变化的子集"，顺序是 ASC（先旧后新），
 *   且不保证满足当前筛选条件。必须与现有列表合并后按 publishedAt DESC 重排。
 */

import type { ArticleDTO } from '@/api/types';
import { listArticles } from '@/api/endpoints';
import { StorageKey, readString, writeString } from '@/lib/storage';

/** 增量拉取选项。 */
export interface IncrementalSyncOptions {
  /**
   * 携带哪些筛选条件。
   *
   * ⚠️ 服务端在增量模式下同样接受这些筛选参数。但**为了不漏拉**，
   *   本实现默认**不传筛选条件**做全量增量：筛选只作用于展示层。
   *   原因：若某篇文章先在筛选 A 下被改、后被移出 A，客户端带 A 拉增量会漏掉
   *   "移出 A" 这个变化（此时它已不匹配 A），列表里会残留一条已不满足筛选的文章。
   *   由于增量数据量通常很小（远小于全量），多拉一点远比漏拉代价低。
   */
  withFilters?: boolean;
  /** 带筛选条件时的筛选参数（仅在 withFilters=true 时使用）。 */
  filters?: { sourceId?: number; category?: string };
  /** 单页条数，增量模式默认 100。 */
  limit?: number;
  /** 单次同步的最大页数，防止异常情况下无限翻页。 */
  maxPages?: number;
  /** 拉取间隔下限（ms），防止频繁回到前台打爆服务端。默认 15s。 */
  minIntervalMs?: number;
}

/** 增量同步结果。 */
export interface IncrementalSyncResult {
  /** 本次拉到的全部文章（跨页合并、已按 id 去重）。 */
  items: ArticleDTO[];
  /** 是否发生了任何变化。 */
  changed: boolean;
  /** 新游标（应持久化）。 */
  cursor: string;
  /** 是否因冷却期未真正发起请求。 */
  skippedByCooldown: boolean;
  /** 是否因游标为空（无基线）而未真正发起请求。 */
  skippedNoBaseline: boolean;
}

const DEFAULT_LIMIT = 100;
const DEFAULT_MAX_PAGES = 20;
const DEFAULT_MIN_INTERVAL_MS = 15_000;
const LAST_RUN_KEY = StorageKey.lastSyncAt;

/** 读取已保存的文章增量游标；无则返回 null（表示必须先做一次全量建立基线）。 */
export function readArticleCursor(): string | null {
  return readString(StorageKey.articleSyncCursor);
}

/** 保存文章增量游标。 */
export function writeArticleCursor(cursor: string): void {
  writeString(StorageKey.articleSyncCursor, cursor);
}

/** 清除文章增量游标（下次访问将退回全量模式）。 */
export function clearArticleCursor(): void {
  writeString(StorageKey.articleSyncCursor, '');
  writeString(LAST_RUN_KEY, '');
}

/** 上次同步时刻（epoch ms），无记录返回 0。 */
export function getLastSyncAt(): number {
  const raw = readString(LAST_RUN_KEY);
  const n = raw ? Number.parseInt(raw, 10) : 0;
  return Number.isFinite(n) ? n : 0;
}

/**
 * 建立/更新游标基线。
 *
 * 必须在一次**不带 cursor** 的列表请求成功后调用 —— 该响应的 `syncCursor`
 * 才是"服务端当前水位"，用作下次增量的起点。
 * 增量模式响应里的 `nextCursor` 语义不同（是"这次增量拉取读到哪了"），
 * 不能直接当作新的基线，否则会漏数据。
 */
export function establishBaseline(syncCursor: string): void {
  if (!syncCursor) return;
  writeArticleCursor(syncCursor);
}

/**
 * 拉取一个页的增量。
 *
 * 拆成"一页一次请求"是为了让调用方（React hook）能在每页之间更新 UI，
 * 用户能看到新文章逐批出现，而不是等全部拉完。
 */
export async function fetchIncrementalPage(
  cursor: string,
  options: IncrementalSyncOptions = {},
): Promise<{ items: ArticleDTO[]; nextCursor: string | null; hasMore: boolean }> {
  const page = await listArticles({
    cursor,
    limit: options.limit ?? DEFAULT_LIMIT,
    ...(options.withFilters && options.filters
      ? {
          sourceId: options.filters.sourceId,
          category: options.filters.category as never,
        }
      : {}),
  });
  return {
    items: page.items ?? [],
    nextCursor: page.nextCursor,
    hasMore: page.hasMore,
  };
}

/**
 * 完整的一次增量同步（循环翻页直到 hasMore=false）。
 *
 * 幂等性：同一游标重复执行会得到同一批数据（服务端按 (updated_at, id) 行值比较，
 * 纯读操作无副作用），因此**网络失败后重试是安全的**。
 *
 * @param fetchPage 注入的取页函数（便于测试与复用已有的 QueryClient）。
 */
export async function runIncrementalSync(
  fetchPage: (cursor: string) => Promise<{ items: ArticleDTO[]; nextCursor: string | null; hasMore: boolean }>,
  options: IncrementalSyncOptions = {},
): Promise<IncrementalSyncResult> {
  const cursor = readArticleCursor();
  const now = Date.now();

  // 冷却：回到前台 / 频繁挂载时不做无谓请求（省流量的第一道闸）。
  const lastRun = getLastSyncAt();
  const minInterval = options.minIntervalMs ?? DEFAULT_MIN_INTERVAL_MS;
  if (lastRun > 0 && now - lastRun < minInterval) {
    return {
      items: [],
      changed: false,
      cursor: cursor ?? '',
      skippedByCooldown: true,
      skippedNoBaseline: false,
    };
  }

  // 无基线：必须先做一次不带 cursor 的全量请求来建立水位。
  // 此时**不能**调用增量，否则游标为空的语义不明。
  if (!cursor) {
    return {
      items: [],
      changed: false,
      cursor: '',
      skippedByCooldown: false,
      skippedNoBaseline: true,
    };
  }

  writeString(LAST_RUN_KEY, String(now));

  const maxPages = options.maxPages ?? DEFAULT_MAX_PAGES;
  /** id -> ArticleDTO，跨页去重（服务端按 (updated_at,id) 分页，同一条理论上不重复）。 */
  const byId = new Map<number, ArticleDTO>();

  let cur: string | null = cursor;
  let pages = 0;
  let hasMore = true;

  while (hasMore && cur && pages < maxPages) {
    const page = await fetchPage(cur);
    pages += 1;

    for (const item of page.items) {
      // 按 id 去重：服务端已保证不重复，这里是防御。
      byId.set(item.id, item);
    }

    hasMore = page.hasMore;
    // ★ nextCursor 为 null 时立即结束：契约里 hasMore=false 时 nextCursor 必为 null，
    //   但若服务端违约返回 hasMore=true + nextCursor=null，循环靠 `cur` 为空退出。
    cur = page.nextCursor;
  }

  const items = Array.from(byId.values());

  // 只有真正拉到末页才推进游标；中途达到 maxPages 上限时**不推进**，
  // 否则剩余增量会被永久跳过（下次同步从旧水位重来，最坏只是多拉，不会漏拉）。
  let newCursor = cursor;
  if (!hasMore) {
    // 增量模式下 nextCursor 即"读完后的水位"，作为新基线。
    newCursor = cur ?? cursor;
  }

  return {
    items,
    changed: items.length > 0,
    cursor: newCursor,
    skippedByCooldown: false,
    skippedNoBaseline: false,
  };
}

/**
 * 按 id 合并两批文章，**保留本地较新的一份**。
 *
 * 增量数据与列表数据的 updatedAt 可比（同为 ISO8601 UTC），
 * 所以用 updatedAt 做 LWW 能避免"增量返回的旧快照覆盖列表里的新数据"。
 */
export function mergeArticles(existing: ArticleDTO[], incoming: ArticleDTO[]): ArticleDTO[] {
  if (incoming.length === 0) return existing;

  const byId = new Map<number, ArticleDTO>();
  for (const a of existing) byId.set(a.id, a);

  for (const a of incoming) {
    const cur = byId.get(a.id);
    if (!cur) {
      byId.set(a.id, a);
      continue;
    }
    const curMs = Date.parse(cur.updatedAt);
    const newMs = Date.parse(a.updatedAt);
    // 时间不可解析时以新到者为准（宁可多更新一次，也不卡住不动）。
    const incomingWins = Number.isNaN(newMs) || Number.isNaN(curMs) || newMs >= curMs;
    if (incomingWins) byId.set(a.id, { ...cur, ...a });
  }

  // 统一按 publishedAt DESC, id DESC 重排（与服务端首屏排序一致）。
  return Array.from(byId.values()).sort((a, b) => {
    const pa = Date.parse(a.publishedAt);
    const pb = Date.parse(b.publishedAt);
    if (!Number.isNaN(pa) && !Number.isNaN(pb) && pa !== pb) return pb - pa;
    return b.id - a.id;
  });
}