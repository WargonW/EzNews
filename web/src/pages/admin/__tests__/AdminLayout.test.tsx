/**
 * `/admin` 权限守卫的回归网。
 *
 * ============================ 为什么必须测这三条 ============================
 * 后台守卫写错的**典型后果不是报错，而是白屏或死循环重定向**：
 * - 无权时 `<Navigate to="/">`，而首页的某个逻辑又把用户送回 /admin
 *   → 浏览器疯狂跳转、CPU 拉满、用户永远看不到任何东西；
 * - `status === 'initializing'` 时误判为"未登录" → 刷新页面先闪"请登录"再变后台。
 * 这两类问题在手工冒烟里偶发难复现（取决于会话恢复的时序），
 * 但用测试把状态钉死就一定是稳定的。
 *
 * 三条断言对应任务书的硬性要求：
 * 1. 游客 → 就地给登录入口，**不跳转**；
 * 2. 已登录非管理员 → 明确的"无权访问"文案，**不跳转**；
 * 3. 本地自称管理员但服务端 403（role 是旧快照）→ 同样落到"无权访问"，**不跳转**。
 *
 * ★ 用真实的 MemoryRouter + 真实的 react-query（只 mock 网络边界的 endpoints），
 *   因为"跳不跳转"这个行为本身就依赖路由与查询的真实协作。
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
// ★ 必须手动 cleanup：本项目 vitest 配的是 `globals: false`，
//   testing-library 的自动 cleanup 依赖全局 afterEach，此时不会注册 ——
//   不手动清会让多个用例的 DOM 叠在一起，"找到多个元素"这类假失败就会冒出来。
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '@/api/client';
import type { AdminOverview, UserDTO } from '@/api/types';
import { useAuthStore } from '@/stores/auth';
import { AdminLayout } from '../AdminLayout';

/* ── 依赖替身：只 mock 网络边界 ─────────────────────────────────────────── */

const { overviewMock } = vi.hoisted(() => ({ overviewMock: vi.fn() }));

vi.mock('@/api/endpoints', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/endpoints')>();
  return { ...actual, getAdminOverview: overviewMock };
});

/* ── 夹具 ──────────────────────────────────────────────────────────────── */

// ★ 形状必须跟 `AdminOverviewDTO` 完全对齐（扁平、时间为 epoch 秒）：
//   一旦漂移，这个夹具就不再是"服务端会返回的东西"，守卫测试的说服力随之失效。
const OVERVIEW: AdminOverview = {
  users: 3,
  admins: 1,
  disabledUsers: 0,
  activeSessions: 2,
  articles: 10,
  sources: 2,
  audios: 1,
  failedAudioTasks: 0,
  pendingAudioTasks: 0,
  articlesLast24h: 4,
  audioTaskStatus: { ready: 1 },
  schemaVersion: 3,
  uptimeSeconds: 3600,
  serverVersion: 'v1.2.0',
  goVersion: 'go1.22.0',
  generatedAt: 1_763_000_000,
};

function user(role: 'user' | 'admin'): UserDTO {
  return {
    id: role === 'admin' ? 1 : 2,
    username: role === 'admin' ? 'root' : 'alice',
    email: null,
    role,
    createdAt: '2026-01-01T00:00:00Z',
    lastLoginAt: null,
  };
}

function renderAdmin(): void {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/admin']}>
        <Routes>
          <Route path="/admin" element={<AdminLayout />}>
            <Route index element={<div>后台内容区</div>} />
          </Route>
          <Route path="/" element={<div>阅读首页</div>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  overviewMock.mockReset();
  overviewMock.mockResolvedValue(OVERVIEW);
  useAuthStore.setState({ status: 'guest', user: null, accessToken: null, refreshToken: null, expiresAtMs: 0 });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('AdminLayout 权限守卫', () => {
  it('会话恢复中：显示骨架，不显示任何"无权/请登录"文案', () => {
    useAuthStore.setState({ status: 'initializing', user: null });
    renderAdmin();

    expect(screen.getByText('正在检查权限…')).toBeTruthy();
    expect(screen.queryByText('管理后台需要登录')).toBeNull();
    expect(screen.queryByText('无权访问管理后台')).toBeNull();
  });

  it('游客：就地给登录入口，且绝不跳转到首页', () => {
    renderAdmin();

    expect(screen.getByText('管理后台需要登录')).toBeTruthy();
    // ★ 关键断言：这里出现"阅读首页"就说明发生了重定向。
    expect(screen.queryByText('阅读首页')).toBeNull();
    // 未登录时不应该去打后台接口（否则每个游客都白吃一次 401）。
    expect(overviewMock).not.toHaveBeenCalled();
  });

  it('已登录但非管理员：显示无权提示，不跳转', () => {
    useAuthStore.setState({ status: 'authenticated', user: user('user'), accessToken: 't' });
    renderAdmin();

    expect(screen.getByText('无权访问管理后台')).toBeTruthy();
    expect(screen.queryByText('阅读首页')).toBeNull();
    expect(screen.queryByText('后台内容区')).toBeNull();
  });

  it('本地自称管理员但服务端返回 403：按服务端判定为无权，不跳转', async () => {
    overviewMock.mockRejectedValue(new ApiError(403, 'FORBIDDEN', '需要管理员'));
    useAuthStore.setState({ status: 'authenticated', user: user('admin'), accessToken: 't' });
    renderAdmin();

    // 服务端 403 之前，本地 role 是 admin，此时不应立刻渲染后台内容。
    await waitFor(() => expect(screen.getByText('无权访问管理后台')).toBeTruthy());
    expect(screen.queryByText('后台内容区')).toBeNull();
    expect(screen.queryByText('阅读首页')).toBeNull();
  });

  it('管理员且服务端放行：渲染侧栏导航与内容区', async () => {
    useAuthStore.setState({ status: 'authenticated', user: user('admin'), accessToken: 't' });
    renderAdmin();

    await waitFor(() => expect(screen.getByText('后台内容区')).toBeTruthy());
    // 六个分区的导航项都要在，否则运维者只能靠手敲 URL。
    for (const label of ['概览', '用户', '新闻源', '语音任务', '内容', '系统']) {
      expect(screen.getAllByText(label).length).toBeGreaterThan(0);
    }
  });
});
