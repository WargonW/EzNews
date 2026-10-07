/**
 * 应用外壳与路由。
 *
 * ============================ 路由级懒加载 ============================
 * 首屏只加载 FeedPage 所需的 chunk，其余页面（详情/设置/收藏）按需加载，
 * 避免把表单、播放队列控制等代码打进首屏 bundle。
 *
 * 顶栏、面板这类**全局 UI 放在外壳**，不随路由切换而卸载 ——
 * 切换页面时搜索框内容得以保留。
 */

import { Component, Suspense, lazy, useCallback, useEffect, useState, type ReactNode } from 'react';
import { BrowserRouter, Navigate, Route, Routes, useNavigate, useLocation } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ApiError } from '@/api/client';
import { AuthDialog } from '@/components/AuthDialog';
import { SourcePanel } from '@/components/SourcePanel';
import { TopBar, type ThemeMode } from '@/components/TopBar';
import { ErrorState } from '@/components/states';
import { FeedPage } from '@/pages/FeedPage';
import { useAuthStore } from '@/stores/auth';
import { useUserStateSync } from '@/hooks/useUserStateSync';
import { useSessionBootstrap } from '@/hooks/useSessionBootstrap';
import { StorageKey, readString, writeString } from '@/lib/storage';

/* ---------- 路由级懒加载 ---------- */
const ArticleDetailPage = lazy(() =>
  import('@/pages/ArticleDetailPage').then((m) => ({ default: m.ArticleDetailPage })),
);
const SettingsPage = lazy(() => import('@/pages/SettingsPage').then((m) => ({ default: m.SettingsPage })));
const FavoritesPage = lazy(() => import('@/pages/FavoritesPage').then((m) => ({ default: m.FavoritesPage })));
const NotFoundPage = lazy(() => import('@/pages/NotFoundPage').then((m) => ({ default: m.NotFoundPage })));

/**
 * 管理后台（`/admin/*`）。
 *
 * 整个后台走懒加载：绝大多数访问者是读者，后台代码不该进入首屏 bundle。
 * 六个分区再各拆一个 chunk —— 后台本身就低频，没必要一次全下。
 */
const AdminLayout = lazy(() => import('@/pages/admin/AdminLayout').then((m) => ({ default: m.AdminLayout })));
const AdminOverviewPage = lazy(() =>
  import('@/pages/admin/AdminOverviewPage').then((m) => ({ default: m.AdminOverviewPage })),
);
const AdminUsersPage = lazy(() => import('@/pages/admin/AdminUsersPage').then((m) => ({ default: m.AdminUsersPage })));
const AdminSourcesPage = lazy(() =>
  import('@/pages/admin/AdminSourcesPage').then((m) => ({ default: m.AdminSourcesPage })),
);
const AdminAudioTasksPage = lazy(() =>
  import('@/pages/admin/AdminAudioTasksPage').then((m) => ({ default: m.AdminAudioTasksPage })),
);
const AdminContentPage = lazy(() =>
  import('@/pages/admin/AdminContentPage').then((m) => ({ default: m.AdminContentPage })),
);
const AdminSystemPage = lazy(() => import('@/pages/admin/AdminSystemPage').then((m) => ({ default: m.AdminSystemPage })));

/**
 * TanStack Query 客户端。
 *
 * 重试策略必须区分"网络错误"与"业务错误"：
 * - 401 由 client 层处理（单飞 refresh + 重放），此处不再重试；
 * - 404/403 重试无意义，只会让错误态持续更久、白白打服务端；
 * - 429 重试会加剧限流，交给 UI 让用户手动重试（服务端有 Retry-After）；
 * - 5xx 与网络错误可重试，但设上限，避免长时间转圈。
 */
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      gcTime: 5 * 60_000,
      // 窗口聚焦不自动重拉：增量同步由 lib/sync 单通道负责（省流量）。
      refetchOnWindowFocus: false,
      retry: (failureCount, error) => {
        if (error instanceof ApiError) {
          if (error.status === 401 || error.status === 403 || error.status === 404) return false;
          if (error.status === 429) return false;
          if (error.status >= 500) return failureCount < 2;
          return false;
        }
        return failureCount < 2;
      },
    },
  },
});

/** 应用外壳（位于 Router / QueryClientProvider 之内）。 */
function AppShell(): JSX.Element {
  const location = useLocation();
  const navigate = useNavigate();

  const [authOpen, setAuthOpen] = useState(false);
  const [sourcePanelOpen, setSourcePanelOpen] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [keyword, setKeyword] = useState('');
  const [theme, setTheme] = useState<ThemeMode>(() => readTheme());

  const status = useAuthStore((s) => s.status);
  const username = useAuthStore((s) => s.user?.username ?? null);
  // 后台入口只对管理员显示。这只是"显示与否"的预判，权限仍由服务端与
  // AdminLayout 的守卫把关（本地 role 可能是旧快照）。
  const isAdmin = useAuthStore((s) => s.user?.role === 'admin');

  // 会话引导（启动静默恢复 + 回到前台增量同步）。
  useSessionBootstrap();
  // 登录态下拉取云端收藏/已读/偏好（含墓碑，DEC-11）。
  useUserStateSync();

  // 主题应用。Tailwind 用 darkMode:'media' 跟随系统，这里额外支持显式覆盖。
  useEffect(() => {
    const root = document.documentElement;
    root.classList.remove('light', 'dark');
    if (theme === 'light') root.classList.add('light');
    else if (theme === 'dark') root.classList.add('dark');
    writeString(StorageKey.theme, theme);
  }, [theme]);

  // 路由切换时回到顶部（否则从长列表进详情再返回会停在中间位置）。
  useEffect(() => {
    window.scrollTo(0, 0);
  }, [location.pathname]);

  /** 搜索：首页时写入 URL 的 q 参数（可分享、可后退）。 */
  const onSearch = useCallback(
    (value: string) => {
      const trimmed = value.trim();
      if (location.pathname !== '/') {
        // 从其它页面发起搜索 → 回首页并带上 q。
        navigate(trimmed ? `/?q=${encodeURIComponent(trimmed)}` : '/');
        return;
      }
      setKeyword(trimmed);
      const params = new URLSearchParams(location.search);
      if (trimmed) params.set('q', trimmed);
      else params.delete('q');
      const qs = params.toString();
      navigate(qs ? `/?${qs}` : '/', { replace: true });
    },
    [location.pathname, location.search, navigate],
  );

  // URL 上的 q 变化时同步搜索框（支持分享链接与浏览器前进后退）。
  useEffect(() => {
    if (location.pathname === '/') {
      setKeyword(new URLSearchParams(location.search).get('q') ?? '');
    }
  }, [location.pathname, location.search]);

  return (
    <div className="min-h-screen bg-surface">
      <TopBar
        keyword={keyword}
        onSubmit={onSearch}
        onOpenDrawer={() => setDrawerOpen(true)}
        onOpenSettings={() => setSourcePanelOpen(true)}
        username={username}
        initializing={status === 'initializing'}
        onOpenAuth={() => setAuthOpen(true)}
        theme={theme}
        onThemeChange={setTheme}
        isAdmin={isAdmin}
      />

      <Suspense fallback={<RouteFallback />}>
        <Routes>
          <Route
            path="/"
            element={
              <FeedPage
                onOpenManage={() => setSourcePanelOpen(true)}
                drawerOpen={drawerOpen}
                onDrawerChange={setDrawerOpen}
              />
            }
          />
          <Route path="/article/:id" element={<ArticleDetailPage />} />
          <Route path="/favorites" element={<FavoritesPage />} />
          <Route path="/settings" element={<SettingsPage onOpenAuth={() => setAuthOpen(true)} />} />

          {/* ---------- 管理后台 ---------- */}
          <Route path="/admin" element={<AdminLayout />}>
            <Route index element={<AdminOverviewPage />} />
            <Route path="users" element={<AdminUsersPage />} />
            <Route path="sources" element={<AdminSourcesPage />} />
            <Route path="audio" element={<AdminAudioTasksPage />} />
            <Route path="content" element={<AdminContentPage />} />
            <Route path="system" element={<AdminSystemPage />} />
          </Route>

          <Route path="/index.html" element={<Navigate to="/" replace />} />
          <Route path="*" element={<NotFoundPage />} />
        </Routes>
      </Suspense>

      {/* 源管理面板 */}
      {sourcePanelOpen ? <SourcePanel open onClose={() => setSourcePanelOpen(false)} /> : null}

      {/*
        ★ 登录弹窗：只有两个触发点 —— 顶栏「登录」按钮、设置面板「登录/注册」按钮。
        浏览/筛选/播放/收藏路径上永远不会出现（PRD-ACCOUNT §1.1 R1/R2）。
      */}
      {authOpen ? (
        <AuthDialog
          open
          onClose={() => setAuthOpen(false)}
          // 登录成功后不做任何跳转与弹窗：用户当前在哪就在哪（零打断 R4）。
          onSuccess={() => setAuthOpen(false)}
        />
      ) : null}
    </div>
  );
}

/** 路由懒加载的兜底（chunk 下载/解析中）。 */
function RouteFallback(): JSX.Element {
  return (
    <div className="mx-auto max-w-[1600px] px-4 py-8 sm:px-6">
      <div className="ez-skeleton h-8 w-48" />
      <div className="mt-4 space-y-3">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="ez-skeleton h-24 w-full rounded-xl" />
        ))}
      </div>
      <span className="sr-only">加载中…</span>
    </div>
  );
}

/** 读取主题偏好（system = 跟随 prefers-color-scheme）。 */
function readTheme(): ThemeMode {
  const raw = readString(StorageKey.theme);
  return raw === 'light' || raw === 'dark' ? raw : 'system';
}

/**
 * 轻量错误边界（不引入第三方库，已在依赖白名单外）。
 * 作用：渲染期异常不至于白屏，给用户一个"重试"出口。
 */
interface ErrorBoundaryState {
  error: unknown;
}

class ErrorBoundary extends Component<{ children: ReactNode }, ErrorBoundaryState> {
  override state: ErrorBoundaryState = { error: null };

  static getDerivedStateFromError(error: unknown): ErrorBoundaryState {
    return { error };
  }

  override render(): ReactNode {
    if (this.state.error !== null) {
      return (
        <div className="flex min-h-screen items-center justify-center bg-surface p-6">
          <ErrorState error={this.state.error} onRetry={() => window.location.reload()} title="页面出错了" />
        </div>
      );
    }
    return this.props.children;
  }
}

export function App(): JSX.Element {
  return (
    <ErrorBoundary>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <AppShell />
        </BrowserRouter>
      </QueryClientProvider>
    </ErrorBoundary>
  );
}