/**
 * 首页「隐藏源」高级筛选区的回归网。
 *
 * ============================ 为什么这几条必须测 ============================
 * 本次改造把"隐藏某源"从侧栏每行的对号搬到了「全部新闻」视图下的高级筛选区。
 * 这条链路有几个**无声**的坑：
 *
 * 1. **筛选区在具体源视图里也冒出来**：那个视图下只有一个源，勾掉它 = 空列表，
 *    毫无意义，还会让人以为"这个源坏了"。它必须只在「全部新闻」出现。
 * 2. **隐藏后没有任何反馈**：列表静默变短，用户不知道原因 —— 必须有
 *    "已隐藏 N 个源"的可见提示。
 * 3. **切换源再切回来就丢了**：隐藏集合若存在组件 state 里、随视图重挂载而重置，
 *    用户先隐藏 A、点开 B、再回全量，A 又冒出来了。它必须持久化在 preferences。
 * 4. **没有恢复入口**：隐藏了多个源后只能一个个点回去。
 *
 * ★ 用真实 MemoryRouter + 真实 react-query（只 mock 网络边界 endpoints），
 *   因为"URL 怎么变 / 列表是否真的变短 / 偏好是否真的写回"都跨层。
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ArticleDTO, ArticlePage, SourceDTO, SourceList } from '@/api/types';
import type { ListArticlesParams } from '@/api/endpoints';
import { StorageKey } from '@/lib/storage';
import { DEFAULT_PREFERENCES, useGuestStore } from '@/stores/guest';
import { useAuthStore } from '@/stores/auth';
import { FeedPage } from '@/pages/FeedPage';

/* ── 依赖替身：只 mock 网络边界 ─────────────────────────────────────────── */

const { listArticlesMock, listSourcesMock, batchSetStateMock } = vi.hoisted(() => ({
  listArticlesMock: vi.fn(),
  listSourcesMock: vi.fn(),
  batchSetStateMock: vi.fn(),
}));

vi.mock('@/api/endpoints', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/endpoints')>();
  return {
    ...actual,
    listArticles: listArticlesMock,
    listSources: listSourcesMock,
    batchSetState: batchSetStateMock,
  };
});

/* ── 夹具 ──────────────────────────────────────────────────────────────── */

const NOW = 1_700_000_000_000;

function makeSource(id: number, name: string, over: Partial<SourceDTO> = {}): SourceDTO {
  return {
    id,
    key: `src-${id}`,
    name,
    url: `https://example.com/${id}.xml`,
    type: 'rss',
    category: 'other',
    isDefault: true,
    enabled: true,
    suggestInterval: 1800,
    iconUrl: null,
    language: null,
    remark: null,
    articleCount: 3,
    createdAt: '2023-11-14T22:13:20Z',
    updatedAt: '2023-11-14T22:13:20Z',
    ...over,
  };
}

const SOURCES: SourceList = {
  items: [makeSource(1, 'IT 之家'), makeSource(2, '少数派')],
};

function makeArticle(id: number, sourceId: number, sourceName: string): ArticleDTO {
  return {
    id,
    sourceId,
    sourceKey: `src-${sourceId}`,
    sourceName,
    category: 'tech',
    title: `文章 ${id}`,
    summary: '',
    imageUrl: null,
    author: null,
    url: `https://example.com/${id}`,
    publishedAt: '2023-11-14T22:13:20Z',
    updatedAt: '2023-11-14T22:13:20Z',
    tags: [],
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

function seedArticlesBySource(): void {
  listArticlesMock.mockImplementation((params: ListArticlesParams = {}) => {
    const all = [makeArticle(10, 1, 'IT 之家'), makeArticle(20, 2, '少数派'), makeArticle(11, 1, 'IT 之家')];
    const items = params.sourceId === undefined ? all : all.filter((a) => a.sourceId === params.sourceId);
    return Promise.resolve(makePage(items));
  });
}

/* ── 渲染外壳 ──────────────────────────────────────────────────────────── */

let queryClient: QueryClient;

function renderFeed(initialEntry = '/') {
  listSourcesMock.mockResolvedValue(SOURCES);
  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <Routes>
          <Route
            path="/"
            element={<FeedPage onOpenManage={() => undefined} drawerOpen={false} onDrawerChange={() => undefined} />}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

/** 高级筛选区的折叠触发器（文本按钮）。 */
function advancedToggle(): HTMLElement {
  return screen.getByRole('button', { name: /高级筛选|收起高级筛选/ });
}

/**
 * 在**高级筛选面板内**取出某源的勾选控件。
 *
 * ★ 必须限定在面板内：源名同时出现在侧栏、文章卡片的来源标签、以及本面板里，
 *   全文档查询会命中多个同名文本。
 */
function hiddenSourceCheckbox(name: string): HTMLElement {
  const panel = screen.getByTestId('hidden-sources-panel');
  const label = within(panel).getByText(name, { selector: 'span' });
  return within(label.closest('label') as HTMLElement).getByRole('checkbox');
}

beforeEach(() => {
  listArticlesMock.mockReset();
  listSourcesMock.mockReset();
  batchSetStateMock.mockReset();
  batchSetStateMock.mockResolvedValue({ readsChanged: 0, favoritesChanged: 0 });
  seedArticlesBySource();
  window.localStorage.clear();
  useAuthStore.setState({ status: 'guest', user: null, accessToken: null, refreshToken: null, expiresAtMs: 0 });
  useGuestStore.setState({ favorites: {}, reads: {}, preferences: { ...DEFAULT_PREFERENCES } });
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
});

afterEach(() => {
  cleanup();
  queryClient.clear();
  vi.restoreAllMocks();
});

/* ========================================================================== *
 * 1. 可见性：只在「全部新闻」视图出现
 * ========================================================================== */

describe('高级筛选区的可见性', () => {
  it('「全部新闻」视图：默认折叠，只有文本触发器，不展开源列表', async () => {
    renderFeed();
    // 触发器存在，但默认 aria-expanded=false，且没有勾选输入。
    const toggle = advancedToggle();
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(screen.queryAllByRole('checkbox')).toHaveLength(0);
    // 必须先等列表渲染，避免用例在后端请求未决时就结束。
    await screen.findByText('全部新闻', { selector: 'h1' });
  });

  it('点开某个具体来源后，高级筛选区消失（该视图下隐藏源无意义）', async () => {
    renderFeed('/?source=1');
    await screen.findByText('IT 之家', { selector: 'h1' });
    expect(screen.queryByRole('button', { name: /高级筛选|收起高级筛选/ })).toBeNull();
  });
});

/* ========================================================================== *
 * 2. 折叠 / 展开
 * ========================================================================== */

describe('折叠与展开', () => {
  it('展开后列出各源的可勾选项，可再次收起', async () => {
    renderFeed();
    fireEvent.click(advancedToggle());

    // 展开后出现两个源的复选框。
    const boxes = await screen.findAllByRole('checkbox');
    expect(boxes).toHaveLength(2);

    // 再点一次收起。
    fireEvent.click(screen.getByRole('button', { name: /收起高级筛选/ }));
    expect(screen.queryAllByRole('checkbox')).toHaveLength(0);
  });
});

/* ========================================================================== *
 * 3. 勾选 = 隐藏：列表变短 + 有可见提示 + 写入偏好
 * ========================================================================== */

describe('勾选隐藏源的效果', () => {
  it('勾选一个源 → 该源文章从列表消失，并出现「已隐藏 1 个源」提示', async () => {
    renderFeed();
    // 全量 3 篇。
    expect(await screen.findByText('3 篇')).toBeTruthy();

    fireEvent.click(advancedToggle());
    await screen.findByTestId('hidden-sources-panel');
    fireEvent.click(hiddenSourceCheckbox('IT 之家'));

    // 隐藏 IT 之家后只剩「少数派」的 1 篇。
    await waitFor(() => {
      expect(screen.getByText('1 篇')).toBeTruthy();
    });
    // ★ 反馈必须可见，否则是静默行为。
    expect(screen.getAllByText(/已隐藏 1 个源/).length).toBeGreaterThan(0);
  });

  it('勾选后把隐藏集合写入 preferences（游客落 localStorage）', async () => {
    renderFeed();
    // 等来源列表加载完再展开（否则面板里还没有可勾选的源）。
    await screen.findByText('3 篇');
    fireEvent.click(advancedToggle());
    await screen.findByTestId('hidden-sources-panel');
    fireEvent.click(hiddenSourceCheckbox('IT 之家'));

    await waitFor(() => {
      expect(useGuestStore.getState().preferences['feed.hiddenSources']).toBe('1');
    });
    const raw = window.localStorage.getItem(StorageKey.guestPreferences);
    expect(JSON.parse(raw as string)['feed.hiddenSources']).toBe('1');
  });

  it('隐藏集合已存在时，初始即生效并显示提示（持久化读回的闭环）', async () => {
    useGuestStore.setState({
      preferences: { ...DEFAULT_PREFERENCES, 'feed.hiddenSources': '2' },
    });
    renderFeed();

    // 隐藏少数派 → 只剩 IT 之家的 2 篇。
    expect(await screen.findByText('2 篇')).toBeTruthy();
    expect(screen.getAllByText(/已隐藏 1 个源/).length).toBeGreaterThan(0);
  });
});

/* ========================================================================== *
 * 4. 清除入口 & 跨视图连续性
 * ========================================================================== */

describe('清除入口与视图连续性', () => {
  it('「恢复全部来源」清空隐藏集合，列表恢复全量', async () => {
    useGuestStore.setState({
      preferences: { ...DEFAULT_PREFERENCES, 'feed.hiddenSources': '1,2' },
    });
    renderFeed();

    fireEvent.click(advancedToggle());
    const clear = await screen.findByRole('button', { name: '恢复全部来源' });
    fireEvent.click(clear);

    await waitFor(() => {
      expect(useGuestStore.getState().preferences['feed.hiddenSources']).toBe('');
      expect(screen.getByText('3 篇')).toBeTruthy();
    });
  });

  it('先隐藏 A、点开 B、再回「全部新闻」：隐藏状态不丢（不随视图重置）', async () => {
    renderFeed();
    // 等来源列表加载完再展开。
    await screen.findByText('3 篇');
    fireEvent.click(advancedToggle());
    await screen.findByTestId('hidden-sources-panel');
    fireEvent.click(hiddenSourceCheckbox('IT 之家'));
    await waitFor(() => {
      expect(useGuestStore.getState().preferences['feed.hiddenSources']).toBe('1');
    });

    // 点开「少数派」（源视图，高级筛选区不渲染）。
    const sidebarNav = screen.getAllByRole('navigation', { name: '新闻源筛选' })[0];
    if (!sidebarNav) throw new Error('侧栏 nav 未找到');
    const span = within(sidebarNav).getByText('少数派', { selector: 'span' });
    fireEvent.click(span.closest('button') as HTMLElement);

    // 再回「全部新闻」。
    fireEvent.click(screen.getByRole('button', { name: /全部新闻/ }));

    await waitFor(() => {
      // 隐藏的 IT 之家仍不出现：只剩少数派 1 篇。
      expect(screen.getByText('1 篇')).toBeTruthy();
    });
    expect(useGuestStore.getState().preferences['feed.hiddenSources']).toBe('1');
  });
});
