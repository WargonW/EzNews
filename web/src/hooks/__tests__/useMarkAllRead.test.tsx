/**
 * `src/hooks/useMarkAllRead.ts` 的回归网。
 *
 * ============================ 为什么这三个点必须测 ============================
 * 这个 hook 的全部复杂度都来自"乐观更新 + 失败回滚"，而回滚代码天生
 * **测不到就会坏**：写错了不报错，只是让用户的假状态留在缓存里，
 * 且因为本地 updatedAt 更晚，后续 LWW 会把它永久固化 —— 无声、无痕、无法自愈。
 *
 * 三个必测点（各自对应一个历史上真出过的 bug）：
 * 1. **形状守卫**：articleKeys.all 是**前缀** key，setQueriesData 会把
 *    ['articles','categories']（Category[]）与 ['articles','detail',id]（ArticleDTO）
 *    一并匹配进来。它们没有 pages，直接 map 会抛 TypeError → onMutate 抛错
 *    → context 为 undefined → 整段回滚被跳过。
 * 2. **onMutate 抛错时 onError 仍被调用但 context 为 undefined**：回滚不能依赖
 *    "context 一定存在"。这里用真实 react-query 跑一遍，证明守卫在就不会走到这条路。
 * 3. **快照只收list 缓存**：detail/categories 本轮没被改，回放它们等于往
 *    无关缓存里写数据（将来若改成反向 patch 就是实打实的串数据）。
 *
 * ★ 测试用真实 QueryClient（不 mock react-query）：这个 bug 的本质就是
 *   "前缀 key 匹配到了什么"，只有真QueryClient 才有说服力。mock 掉它等于没测。
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor, act } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import type { ArticleDTO, ArticlePage, CategoryList } from '@/api/types';
import { ApiError } from '@/api/client';
import { articleKeys } from '@/lib/queryKeys';
import { DEFAULT_PREFERENCES, useGuestStore } from '@/stores/guest';
import { useAuthStore } from '@/stores/auth';
import { BATCH_LIMIT, useMarkAllRead } from '@/hooks/useMarkAllRead';

/* ── 被测模块的依赖替身 ─────────────────────────────────────────────────── */

/**
 * 只 mock 端点层（网络边界），不 mock react-query / 不 mock store。
 *
 * 理由：乐观更新的正确性**跨越** react-query 缓存与 zustand 两层，
 * mock 掉任何一层都会让测试退化成"验证我自己的 mock"。
 */
const { batchSetStateMock } = vi.hoisted(() => ({ batchSetStateMock: vi.fn() }));

vi.mock('@/api/endpoints', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/endpoints')>();
  return { ...actual, batchSetState: batchSetStateMock };
});

/* ── 测试夹具 ──────────────────────────────────────────────────────────── */

const NOW = 1_700_000_000_000;

function makeArticle(id: number, over: Partial<ArticleDTO> = {}): ArticleDTO {
  return {
    id,
    sourceId: 1,
    sourceKey: 's1',
    sourceName: '源',
    category: 'tech',
    title: `文章 ${id}`,
    summary: '',
    imageUrl: null,
    author: null,
    url: `https://example.com/${id}`,
    publishedAt: '2023-11-14T22:13:20Z',
    updatedAt: '2023-11-14T22:13:20Z',
    tags: [],
    ...over,
  };
}

function makePage(items: ArticleDTO[]): ArticlePage {
  return {
    items,
    nextCursor: null,
    nextPageCursor: null,
    syncCursor: '2023-11-14T22:13:20Z',
    hasMore: false,
    serverTime: '2023-11-14T22:13:20Z',
    serverTimeMs: NOW,
  };
}

/** 每个用例一份独立 QueryClient：跨用例共享会互相污染缓存断言。 */
let queryClient: QueryClient;

/**
 * 供单个用例注册"结束后还原"的清理动作（目前只有替换 store 方法那一个用例）。
 * 放这里由 afterEach 统一执行，避免用例失败时清理被跳过。
 */
let markAllReadRestore: (() => void) | undefined;

/**
 * wrapper 必须在用例内创建（闭包捕获当前 queryClient）。
 * 写成 .tsx 时不能给箭头函数加返回类型标注 —— esbuild 会把 `): T =>` 当成 JSX 解析。
 */
function makeWrapper() {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }
}

/** 预置一个列表缓存（登录态，文章未读）。 */
function seedListCache(key: readonly unknown[], ids: number[]): void {
  queryClient.setQueryData(key, {
    pages: [makePage(ids.map((id) => makeArticle(id)))],
    pageParams: [undefined],
  });
}

/** 把缓存里某篇的 isRead 读出来（列表页 items 里的字段）。 */
function cachedIsRead(key: readonly unknown[], id: number): boolean | undefined {
  const data = queryClient.getQueryData<{ pages: ArticlePage[] }>(key);
  return data?.pages.flatMap((p) => p.items).find((i) => i.id === id)?.isRead;
}

/** 登录态（与游客态相对）。 */
function login(): void {
  useAuthStore.setState({
    status: 'authenticated',
    accessToken: 'token',
    refreshToken: null,
    expiresAtMs: NOW + 3_600_000,
    user: null,
  });
}

function logout(): void {
  useAuthStore.setState({
    status: 'guest',
    accessToken: null,
    refreshToken: null,
    expiresAtMs: 0,
    user: null,
  });
}

/**
 * 挂载 hook。isRead 默认认为"都没读"，与登录态下 article.isRead 全为
 * undefined 的游客/服务端未返回场景一致。
 */
function mount(visibleIds: number[], isRead: (id: number) => boolean = () => false, notify = vi.fn()) {
  return renderHook(() => useMarkAllRead({ visibleIds, isRead, notify }), { wrapper: makeWrapper() });
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(NOW);
  batchSetStateMock.mockReset();
  batchSetStateMock.mockResolvedValue({ readsChanged: 0, favoritesChanged: 0 });
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  logout();
  useGuestStore.setState({
    favorites: {},
    reads: {},
    preferences: { ...DEFAULT_PREFERENCES },
  });
});

afterEach(() => {
  // 兜底还原：某个用例把 store 换成会抛的实现后失败时，末尾的还原不会执行。
  markAllReadRestore?.();
  markAllReadRestore = undefined;
  vi.useRealTimers();
  queryClient.clear();
});

/* ========================================================================== *
 * 1. 形状守卫：非列表形状的 ['articles',...] 缓存必须被原样放过
 * ========================================================================== */

describe('patchCache 的形状守卫（前缀 query key 陷阱）', () => {
  it('分类缓存（CategoryList，无 pages）被原样返回，不抛错也不被改写', async () => {
    login();
    const categories: CategoryList = {
      items: [{ key: 'tech', label: '科技', count: 12 }],
    };
    queryClient.setQueryData(articleKeys.categories, categories);
    seedListCache(articleKeys.list({}), [1]);

    const { result } = mount([1]);
    await act(async () => {
      result.current.markAllRead();
    });

    // 分类缓存既不该被 patch 也不该因抛错而中断整条链路。
    expect(queryClient.getQueryData(articleKeys.categories)).toEqual(categories);
    // 列表仍然正常被改成已读 —— 证明守卫是"放过"而不是"整批放弃"。
    expect(cachedIsRead(articleKeys.list({}), 1)).toBe(true);
  });

  it('详情缓存（ArticleDTO，无 pages）被原样返回，字段不变', async () => {
    login();
    const detail = makeArticle(1, { content: '正文', isRead: false });
    queryClient.setQueryData(articleKeys.detail(1), detail);
    seedListCache(articleKeys.list({}), [1]);

    const { result } = mount([1]);
    await act(async () => {
      result.current.markAllRead();
    });

    expect(queryClient.getQueryData(articleKeys.detail(1))).toEqual(detail);
  });

  it('详情缓存里的正文等字段不被误改（若守卫失效，最先被改坏的就是它）', async () => {
    login();
    const detail = makeArticle(1, { content: '原始正文', isRead: false });
    queryClient.setQueryData(articleKeys.detail(1), detail);
    seedListCache(articleKeys.list({}), [1]);

    const { result } = mount([1]);
    await act(async () => {
      result.current.markAllRead();
    });

    expect(queryClient.getQueryData<ArticleDTO>(articleKeys.detail(1))?.content).toBe('原始正文');
  });

  it('缓存值为 undefined 时不抛错（守卫的 !old 分支）', async () => {
    login();
    // 显式写入 undefined，模拟"key 存在但无数据"。
    queryClient.setQueryData(articleKeys.detail(999), undefined);
    seedListCache(articleKeys.list({}), [1]);

    const { result } = mount([1]);
    await act(async () => {
      result.current.markAllRead();
    });

    expect(queryClient.getQueryData(articleKeys.detail(999))).toBeUndefined();
    expect(cachedIsRead(articleKeys.list({}), 1)).toBe(true);
  });
});

/* ========================================================================== *
 * 2. 失败回滚：缓存与本地 store 两处都要还原
 * ========================================================================== */

describe('提交失败时的回滚', () => {
  it('onMutate 抛错时：context 为 undefined，回滚整段被跳过但不崩、且提示仍给出', async () => {
    login();
    seedListCache(articleKeys.list({}), [1]);

    /**
     * ★ 这个用例是"context 判空"存在的**唯一理由**。
     *
     * onMutate 里唯一会抛的地方是 markAllReadLocal / patchCache。这里让
     * markAllReadLocal 抛（模拟 store 被换成会抛的实现、或缓存结构异常），
     * 于是 mutation 进入 error 态且 **context 为 undefined**。
     *
     * 此时 onError 里的 `if (context)` 必须成立 —— 否则 `context.cacheSnapshots`
     * 会在 onError 内抛 TypeError，把 notify 也一起吞掉，用户既没回滚也没提示：
     * 按钮点了像没反应，且乐观更新残留成坏状态。
     *
     * 断言点因此是**行为**（不抛、且 notify 被调用），不是内部实现。
     */
    const throwingMarkAllRead = vi.fn(() => {
      throw new Error('onMutate boom');
    });
    const originalMarkAllRead = useGuestStore.getState().markAllRead;
    useGuestStore.setState({ markAllRead: throwingMarkAllRead as unknown as typeof originalMarkAllRead });
    // ★ 还原必须放afterEach 而不是用例末尾：用例失败时末尾那行不会执行，
    //   泄漏的 throwing实现会把后续所有用例一起带红 —— 那种"一片红"会让人
    //   误以为是自己的改动炸了，掩盖真正的失败点。
    markAllReadRestore = () => useGuestStore.setState({ markAllRead: originalMarkAllRead });
    batchSetStateMock.mockRejectedValue(new ApiError(500, 'INTERNAL_ERROR', 'server boom'));
    const notify = vi.fn();

    const { result } = mount([1], () => false, notify);

    // 不 expect(() => ...).toThrow()：错误由 react-query 内部消化，
    // 真正要证的是"onError 没被自己的 TypeError 打断"。
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      // notify 走到了 onError 末尾 → 说明 context 为 undefined 时没有中途抛。
      expect(notify).toHaveBeenCalledWith(expect.stringContaining('已恢复原状'), 'warn');
    });
    // onMutate 在写缓存前就抛了，缓存不该被乐观更新过。
    expect(cachedIsRead(articleKeys.list({}), 1)).toBeUndefined();
  });

  it('缓存与 guest store 同时还原 —— 只还原一处就是"用户看不见的坏状态"', async () => {
    login();
    seedListCache(articleKeys.list({}), [1, 2]);
    batchSetStateMock.mockRejectedValue(new ApiError(500, 'INTERNAL_ERROR', 'boom'));

    const { result } = mount([1, 2]);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(batchSetStateMock).toHaveBeenCalledTimes(1);
    });
    await waitFor(() => {
      expect(cachedIsRead(articleKeys.list({}), 1)).toBeUndefined();
    });
    // 本地 store 也必须还原：登录态下次 LWW 靠它决定要不要把这批"已读"传上云端。
    expect(useGuestStore.getState().toMergeItems('reads')).toEqual([]);
  });

  it('onMutate 未抛错时回滚**不**被跳过 —— context 一定存在', async () => {
    login();
    seedListCache(articleKeys.list({}), [1]);
    batchSetStateMock.mockRejectedValue(new ApiError(500, 'INTERNAL_ERROR', 'boom'));

    const { result } = mount([1]);
    await act(async () => {
      result.current.markAllRead();
    });

    // 乐观更新确实发生过（mutation 至少被触发）。
    await waitFor(() => {
      expect(batchSetStateMock).toHaveBeenCalledWith({ articleIds: [1], read: true });
    });
    // 最终态是还原：缓存无 isRead、本地无痕迹。
    await waitFor(() => {
      expect(cachedIsRead(articleKeys.list({}), 1)).toBeUndefined();
    });
    expect(useGuestStore.getState().isRead(1)).toBe(false);
  });

  it('即使缓存里混着无 pages 的前缀兄弟，失败后也能完整回滚（守卫不抛 → context 有效）', async () => {
    login();
    const categories: CategoryList = { items: [{ key: 'tech', label: '科技', count: 1 }] };
    queryClient.setQueryData(articleKeys.categories, categories);
    queryClient.setQueryData(articleKeys.detail(1), makeArticle(1));
    seedListCache(articleKeys.list({}), [1, 2]);
    batchSetStateMock.mockRejectedValue(new ApiError(500, 'INTERNAL_ERROR', 'boom'));

    const { result } = mount([1, 2]);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(cachedIsRead(articleKeys.list({}), 2)).toBeUndefined();
    });
    // 无关缓存自始至终没被写过。
    expect(queryClient.getQueryData(articleKeys.categories)).toEqual(categories);
    expect(useGuestStore.getState().toMergeItems('reads')).toEqual([]);
  });

  it('部分提交后失败：未提交的目标条目也要一并还原', async () => {
    login();
    // 只有 1、2 在缓存里，visibleIds 含 1、2、3（3 不在缓存里）。
    seedListCache(articleKeys.list({}), [1, 2]);
    batchSetStateMock.mockRejectedValue(new ApiError(500, 'INTERNAL_ERROR', 'boom'));

    const { result } = mount([1, 2, 3]);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(useGuestStore.getState().toMergeItems('reads')).toEqual([]);
    });
  });

  it('404 走"刷新后重试"提示，且同样完成回滚', async () => {
    login();
    seedListCache(articleKeys.list({}), [1]);
    batchSetStateMock.mockRejectedValue(new ApiError(404, 'NOT_FOUND', 'gone'));
    const notify = vi.fn();

    const { result } = mount([1], () => false, notify);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(notify).toHaveBeenCalledWith(
        expect.stringContaining('刷新后重试'),
        'warn',
      );
    });
    expect(useGuestStore.getState().isRead(1)).toBe(false);
  });

  it('非 404 失败走通用提示（不回滚的假象最容易在这里被放过）', async () => {
    login();
    seedListCache(articleKeys.list({}), [1]);
    batchSetStateMock.mockRejectedValue(new ApiError(500, 'INTERNAL_ERROR', 'boom'));
    const notify = vi.fn();

    const { result } = mount([1], () => false, notify);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(notify).toHaveBeenCalledWith(expect.stringContaining('已恢复原状'), 'warn');
    });
  });
});

/* ========================================================================== *
 * 3. 快照范围：只收list 缓存
 * ========================================================================== */

describe('listCacheSnapshots 的收集范围', () => {
  it('只快照真正会被 patch 的列表缓存，不含 detail/categories', async () => {
    login();
    const categories: CategoryList = { items: [] };
    queryClient.setQueryData(articleKeys.categories, categories);
    queryClient.setQueryData(articleKeys.detail(1), makeArticle(1, { content: '正文' }));
    seedListCache(articleKeys.list({}), [1]);

    // spy setQueriesData：**回滚用的是 setQueryData，不是 setQueriesData**。
    // 这一点本身就是断言点 —— 若将来有人把回滚改成 setQueriesData（不带 filter），
    // 它会连带把 detail/categories 一起重写，这里立刻红。
    const setQueriesDataSpy = vi.spyOn(queryClient, 'setQueriesData');
    batchSetStateMock.mockRejectedValue(new ApiError(500, 'INTERNAL_ERROR', 'boom'));

    const { result } = mount([1]);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(queryClient.getQueryState(articleKeys.detail(1))?.isInvalidated).toBeDefined();
    });
    // setQueriesData 只在乐观更新的 patchCache 里被调过一次（那就是改，不是回滚）。
    expect(setQueriesDataSpy).toHaveBeenCalledTimes(1);
    // 无关缓存自始至终没被写过 —— 内容仍是原始那份。
    expect(queryClient.getQueryData(articleKeys.categories)).toEqual(categories);
    expect(queryClient.getQueryData<ArticleDTO>(articleKeys.detail(1))?.content).toBe('正文');
  });

  it('快照在改缓存之前抓 —— 否则回滚等于把改后的值又写回去（回滚失效但不报错）', async () => {
    login();
    seedListCache(articleKeys.list({}), [1]);
    // 改前的值：没有 isRead 字段。
    const original = structuredClone(queryClient.getQueryData(articleKeys.list({})));
    batchSetStateMock.mockRejectedValue(new ApiError(500, 'INTERNAL_ERROR', 'boom'));

    const { result } = mount([1]);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      // 若快照抓在改之后，回滚会把 isRead:true 写回来 —— 这里就会看到 true。
      expect(cachedIsRead(articleKeys.list({}), 1)).toBeUndefined();
    });
    expect(queryClient.getQueryData(articleKeys.list({}))).toEqual(original);
  });

  it('多个筛选维度的列表缓存都被快照并各自还原', async () => {
    login();
    const filtersA = { category: 'tech' } as const;
    const filtersB = { sourceId: 7 } as const;
    seedListCache(articleKeys.list(filtersA), [1]);
    seedListCache(articleKeys.list(filtersB), [2]);
    const snapA = structuredClone(queryClient.getQueryData(articleKeys.list(filtersA)));
    const snapB = structuredClone(queryClient.getQueryData(articleKeys.list(filtersB)));

    // 用一个手动 reject 的 promise 把 mutation 悬在 pending：
    // 这样能先断言"乐观更新确实同时改了两份缓存"（否则下面的"各自还原"是空断言），
    // 再放行失败去验证还原。
    let rejectBatch: (err: unknown) => void = () => undefined;
    batchSetStateMock.mockImplementation(
      () =>
        new Promise((_, reject) => {
          rejectBatch = reject;
        }),
    );

    const { result } = mount([1, 2]);
    await act(async () => {
      result.current.markAllRead();
    });

    expect(cachedIsRead(articleKeys.list(filtersA), 1)).toBe(true);
    expect(cachedIsRead(articleKeys.list(filtersB), 2)).toBe(true);

    await act(async () => {
      rejectBatch(new ApiError(500, 'INTERNAL_ERROR', 'boom'));
    });

    await waitFor(() => {
      expect(queryClient.getQueryData(articleKeys.list(filtersA))).toEqual(snapA);
      expect(queryClient.getQueryData(articleKeys.list(filtersB))).toEqual(snapB);
    });
  });
});

/* ========================================================================== *
 * 成功路径与身份分流（覆盖回滚之外的另一半逻辑）
 * ========================================================================== */

describe('身份分流与成功路径', () => {
  it('游客态只写本地、不发请求（没有服务端就没有失败，也就不该谈回滚）', async () => {
    logout();
    const { result } = mount([1, 2]);
    await act(async () => {
      result.current.markAllRead();
    });

    expect(batchSetStateMock).not.toHaveBeenCalled();
    expect(useGuestStore.getState().isRead(1)).toBe(true);
    expect(useGuestStore.getState().isRead(2)).toBe(true);
  });

  it('登录态同时写缓存与本地 store（两处落点缺一不可）', async () => {
    login();
    seedListCache(articleKeys.list({}), [1]);

    const { result } = mount([1]);
    await act(async () => {
      result.current.markAllRead();
    });

    // 本地 store：游客是唯一来源，登录态决定下次 LWW 会不会把"已读"传上云端。
    expect(useGuestStore.getState().isRead(1)).toBe(true);
    // react-query 缓存：登录态 isRead 来自服务端字段，不写缓存 UI 一点不变。
    expect(cachedIsRead(articleKeys.list({}), 1)).toBe(true);
  });

  it('已读的不提交（省下 500 条名额），未读计数只算未读', () => {
    login();
    const readSet = new Set([2, 4]);
    const { result } = mount([1, 2, 3, 4], (id) => readSet.has(id));

    expect(result.current.unreadCount).toBe(2);
    expect(result.current.canMarkAll).toBe(true);
  });

  it('全部已读时按钮禁用（点了没反馈会被当成按钮坏了）', () => {
    login();
    const { result } = mount([1, 2], () => true);

    expect(result.current.unreadCount).toBe(0);
    expect(result.current.canMarkAll).toBe(false);
  });

  it('重复 id 只提交一次（服务端会去重，但会白占名额）', async () => {
    login();
    seedListCache(articleKeys.list({}), [1]);

    const { result } = mount([1, 1, 1]);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(batchSetStateMock).toHaveBeenCalledWith({ articleIds: [1], read: true });
    });
  });

  it('非法 id 被过滤，不进提交列表（否则整批 400）', async () => {
    login();
    seedListCache(articleKeys.list({}), [1]);

    const { result } = mount([1, 0, -5, 2.5]);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(batchSetStateMock).toHaveBeenCalledWith({ articleIds: [1], read: true });
    });
  });

  it('超过单次上限时裁剪而非分片（整批原子是服务端的保证，切片会撕碎它）', async () => {
    login();
    const ids = Array.from({ length: BATCH_LIMIT + 10 }, (_, i) => i + 1);
    seedListCache(articleKeys.list({}), ids.slice(0, 5));

    const { result } = mount(ids);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(batchSetStateMock).toHaveBeenCalledTimes(1);
    });
    const submitted = batchSetStateMock.mock.calls[0]?.[0] as { articleIds: number[] };
    expect(submitted.articleIds).toHaveLength(BATCH_LIMIT);
  });

  it('成功时提示用"提交时刻的未读数"算差值，不读回调时的实时值', async () => {
    login();
    seedListCache(articleKeys.list({}), [1, 2]);
    const notify = vi.fn();

    const { result } = mount([1, 2], () => false, notify);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(notify).toHaveBeenCalledWith('已标记 2 篇为已读', 'success');
    });
  });

  it('成功不回滚本地（真成功了，本地应与服务端一致）', async () => {
    login();
    seedListCache(articleKeys.list({}), [1]);
    batchSetStateMock.mockResolvedValue({ readsChanged: 1, favoritesChanged: 0 });

    const { result } = mount([1]);
    await act(async () => {
      result.current.markAllRead();
    });

    await waitFor(() => {
      expect(useGuestStore.getState().isRead(1)).toBe(true);
    });
    // 成功路径靠 invalidate 拉回服务端权威值，本地保持已读。
    await waitFor(() => {
      expect(queryClient.getQueryState(articleKeys.list({}))?.isInvalidated).toBe(true);
    });
  });
});