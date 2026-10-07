/**
 * 首页「左侧点选来源 → 右侧整体切换」改造的回归网。
 *
 * ============================ 为什么这几条必须测 ============================
 * 本次改造把首页从「全量聚合流 + 分类 Tab」改成「点来源 = 独占该源」，
 * 筛选态依然全部走 URL query string。这类改造的坑全是**无声**的：
 *
 * 1. **点来源没有清掉 category**：URL 里 source 与 category 叠加，
 *    服务端按两者 AND 过滤 —— 用户点了一个源却看到空列表，
 *    且从界面上再也看不出 category 还在生效（Tab 已经删了）。
 * 2. **「全部新闻」只清 source 不清 category**：用户以为回到了全量流，
 *    实际仍被上次的分类（或链接里带的分类）继续过滤。
 * 3. **页头标题不随源变化 / 源名查不到时显示空白**：来源列表还在加载时
 *    标题闪成空白，是最容易被忽略的边界。
 * 4. **「全部已读」范围不随选中源收窄**：`visibleIds` 若仍来自全量结果，
 *    一次点击会误伤几百篇其它源的文章 —— 正是本次改造要避免的。
 *
 * ★ 用真实的 MemoryRouter + 真实 react-query（只 mock 网络边界的 endpoints），
 *   因为"URL 参数怎么变"与"筛选变化后重取什么"本身就是这两层的协作产物，
 *   mock 掉任何一层，测出来的就只是"我自己的 mock"。
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
// ★ 手动 cleanup：本项目 vitest 配 `globals: false`，testing-library 的自动
//   cleanup 依赖全局 afterEach，此时不会注册 —— 不手动清会让多个用例的 DOM 叠一起。
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type {
  ArticleDTO,
  ArticlePage,
  SourceDTO,
  SourceList,
} from '@/api/types';
import type { ListArticlesParams } from '@/api/endpoints';
import { articleKeys } from '@/lib/queryKeys';
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

/**
 * 列表按 sourceId 分流返回：点某个源后，只有该源的文章会回来。
 * 这样"可见范围随源收窄"才是真实数据层的效果，而不是断言里的假象。
 */
function seedArticlesBySource(): void {
  listArticlesMock.mockImplementation((params: ListArticlesParams = {}) => {
    const all = [makeArticle(10, 1, 'IT 之家'), makeArticle(20, 2, '少数派'), makeArticle(11, 1, 'IT 之家')];
    const items = params.sourceId === undefined ? all : all.filter((a) => a.sourceId === params.sourceId);
    return Promise.resolve(makePage(items));
  });
}

/* ── 渲染外壳 ──────────────────────────────────────────────────────────── */

let queryClient: QueryClient;
/** DashboardLayout 的 memory router 里没法直接读 URL，用探针把 query string 暴露成文本。 */
let lastSearch = '';

function LocationProbe(): JSX.Element {
  const location = useLocation();
  lastSearch = location.search;
  return <span data-testid="search">{location.search}</span>;
}

function renderFeed(initialEntry = '/', options: { sources?: SourceList | 'pending' } = {}) {
  const { sources = SOURCES } = options;
  if (sources === 'pending') {
    listSourcesMock.mockImplementation(() => new Promise<SourceList>(() => undefined));
  } else {
    listSourcesMock.mockResolvedValue(sources);
  }

  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <Routes>
          <Route
            path="/"
            element={
              <>
                <LocationProbe />
                <FeedPage onOpenManage={() => undefined} drawerOpen={false} onDrawerChange={() => undefined} />
              </>
            }
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

/**
 * 从侧栏里取出选中某源的按钮。
 *
 * ★ 必须把查询**限定在侧栏 nav 内**：源名同时会出现在页头标题（选中后）与
 *   文章卡片的来源标签里，全文档范围查询会命中多个同名文本。
 */
function sidebarNav(): HTMLElement {
  const nav = screen.getAllByRole('navigation', { name: '新闻源筛选' })[0];
  if (!nav) throw new Error('侧栏 nav 未找到');
  return nav;
}

/**
 * 等待来源列表渲染出某个源名，并返回**选中该源的那个按钮**。
 *
 * ★ 也不能直接用 `getByRole('button', { name })` —— 同一行里还有一个
 *   `aria-label="隐藏 X"` 的勾选框按钮，它的可访问名同样包含源名。
 *   这里在 nav 内锁定源名文本，再上溯到其最近的 button（=选择按钮）。
 */
async function waitForSource(name: string): Promise<HTMLElement> {
  const nav = await waitFor(() => sidebarNav());
  const span = await within(nav).findByText(name, { selector: 'span' });
  const btn = span.closest('button');
  if (!btn) throw new Error(`waitForSource: 源名 ${name} 未找到其选择按钮`);
  return btn as HTMLElement;
}

beforeEach(() => {
  lastSearch = '';
  listArticlesMock.mockReset();
  listSourcesMock.mockReset();
  batchSetStateMock.mockReset();
  batchSetStateMock.mockResolvedValue({ readsChanged: 0, favoritesChanged: 0 });
  seedArticlesBySource();
  // 游客态即可覆盖筛选改造；登录态只会多走一步服务端已读，不影响 URL 断言。
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
 * 1. 点来源 = 独占切换：写 source、清 category
 * ========================================================================== */

describe('点来源 = 独占切换', () => {
  it('初始无筛选时，点某个源 → URL 只有 source、无 category', async () => {
    renderFeed();
    fireEvent.click(await waitForSource('IT 之家'));

    await waitFor(() => {
      expect(lastSearch).toBe('?source=1');
    });
    expect(lastSearch).not.toContain('category');
  });

  it('URL 里已带 category 时，点来源要把 category 清掉（否则两参数叠加成空列表）', async () => {
    // 用户带着一个旧的 category 链接进来（例如旧书签）。
    renderFeed('/?category=finance');
    fireEvent.click(await waitForSource('少数派'));

    await waitFor(() => {
      expect(lastSearch).toBe('?source=2');
    });
    expect(lastSearch).not.toContain('category');
  });

  it('点来源保留 q（在某个源里继续搜索是自然预期）', async () => {
    renderFeed('/?q=ai');
    fireEvent.click(await waitForSource('IT 之家'));

    await waitFor(() => {
      const p = new URLSearchParams(lastSearch);
      expect(p.get('source')).toBe('1');
      expect(p.get('q')).toBe('ai');
    });
    expect(lastSearch).not.toContain('category');
  });

  it('再次点同一个已选中的源 → 取消选中，回到无 source', async () => {
    renderFeed('/?source=1');
    const btn = await waitForSource('IT 之家');
    // 第一次点击：active → null（Sidebar 的 onSelectSource(active ? null : s.id)）。
    fireEvent.click(btn);

    await waitFor(() => {
      expect(lastSearch).not.toContain('source=');
    });
  });
});

/* ========================================================================== *
 * 2. 「全部新闻」= 回到全量聚合流：source 与 category 都要清
 * ========================================================================== */

describe('「全部新闻」入口', () => {
  it('从「源内 + 残留 category」状态点全部新闻：两个参数都清掉', async () => {
    renderFeed('/?source=1&category=tech');
    fireEvent.click(await screen.findByRole('button', { name: /全部新闻/ }));

    await waitFor(() => {
      expect(lastSearch).toBe('');
    });
  });

  it('从源内状态点全部新闻：清掉 source', async () => {
    renderFeed('/?source=2');
    fireEvent.click(await screen.findByRole('button', { name: /全部新闻/ }));

    await waitFor(() => {
      expect(lastSearch).not.toContain('source=');
    });
  });
});

/* ========================================================================== *
 * 3. 页头标题随选中源变化
 * ========================================================================== */

describe('页头标题', () => {
  it('无选中源时显示默认文案「全部新闻」', async () => {
    renderFeed();
    // 侧栏「全部新闻」入口与页头标题同名，都会命中；这里锁定页头那个 h1。
    await waitForSource('IT 之家');
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('全部新闻');
  });

  it('选中某个源后，h1 变成该来源名', async () => {
    renderFeed('/?source=1');
    await waitForSource('IT 之家');
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('IT 之家');
    });
  });

  it('来源列表尚未加载完时，标题回落默认文案而不是空白', async () => {
    renderFeed('/?source=1', { sources: 'pending' });
    // 来源列表 pending，源名查不到 —— 标题必须回落，绝不能是空串。
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('全部新闻');
    });
  });
});

/* ========================================================================== *
 * 4. 「全部已读」范围随选中源收窄（易漏的耦合点）
 * ========================================================================== */

describe('「全部已读」的可见范围', () => {
  it('选中源后，只有该源的文章进入提交范围（不误伤其它源 20）', async () => {
    // 登录态才走 batchSetState 网络路径，从而能观测到提交的 id 集合。
    useAuthStore.setState({
      status: 'authenticated',
      accessToken: 't',
      refreshToken: null,
      expiresAtMs: NOW + 3_600_000,
      user: null,
    });

    renderFeed('/?source=1');
    await waitForSource('IT 之家');

    // 该源下应有 2 篇（id 10、11），计数出现在「全部已读」按钮上。
    const markBtn = await screen.findByTitle(/把当前列表的 2 篇未读标记为已读/);
    fireEvent.click(markBtn);

    // ★ 核心断言：提交范围恰好是该源的 10、11，绝不含其它源的 20。
    await waitFor(() => {
      expect(batchSetStateMock).toHaveBeenCalledWith({ articleIds: [10, 11], read: true });
    });
    const submitted = batchSetStateMock.mock.calls[0]?.[0] as { articleIds: number[] };
    expect(submitted.articleIds).not.toContain(20);
  });

  it('切回「全部新闻」后，提交范围恢复为全部源', async () => {
    useAuthStore.setState({
      status: 'authenticated',
      accessToken: 't',
      refreshToken: null,
      expiresAtMs: NOW + 3_600_000,
      user: null,
    });

    renderFeed('/?source=1');
    await waitForSource('IT 之家');
    fireEvent.click(await screen.findByRole('button', { name: /全部新闻/ }));

    // 全量下应有 3 篇（10、11、20）。
    const markBtn = await screen.findByTitle(/把当前列表的 3 篇未读标记为已读/);
    fireEvent.click(markBtn);

    await waitFor(() => {
      expect(batchSetStateMock).toHaveBeenCalledWith({ articleIds: [10, 20, 11], read: true });
    });
  });
});

/* ========================================================================== *
 * 5. 列表整体切换的请求侧断言（URL 决定重取哪个 sourceId）
 * ========================================================================== */

describe('列表请求随选中源切换', () => {
  it('点源后，列表请求带上该 sourceId（queryKey/params 由 URL 驱动，无需手动 refetch）', async () => {
    renderFeed();
    fireEvent.click(await waitForSource('少数派'));

    await waitFor(() => {
      const calls = listArticlesMock.mock.calls.map((c) => (c[0] as ListArticlesParams | undefined)?.sourceId);
      expect(calls).toContain(2);
    });
  });

  it('列表渲染与 queryKey 一致：选中源后 key 里带 sourceId（缓存不串数据的前提）', async () => {
    renderFeed('/?source=1');
    await waitForSource('IT 之家');
    // 至少要有一个 ['articles','list',{sourceId:1,...}] 的缓存条目。
    await waitFor(() => {
      const key = articleKeys.list({ sourceId: 1, withAudio: true });
      expect(queryClient.getQueryData(key)).toBeTruthy();
    });
  });
});
