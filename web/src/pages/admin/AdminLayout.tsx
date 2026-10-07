/**
 * 管理后台布局与权限守卫（路由 `/admin/*`）。
 *
 * ============================ ★ 无管理员 / 非管理员的处理 ============================
 * 这是本文件最重要的部分。后台不能靠"重定向走人"来处理无权访问 ——
 * 一旦无权时 `<Navigate to="/">` 而首页又某种原因跳回来，就是死循环重定向，
 * 表现为浏览器狂刷、CPU 拉满、页面永远白屏。
 *
 * 因此这里的规则是：**就地渲染明确提示，永不自动跳转**。三种情形：
 * 1. `status === 'initializing'`（会话正在静默恢复）→ 骨架屏。
 *    此时**不能**判定"未登录"—— 否则刷新页面会闪一下"请登录"再变后台。
 * 2. 未登录 → 就地给出登录入口（AuthDialog），不跳走。
 * 3. 已登录但角色不是 admin，或任意后台接口返回 403 → 明确的"无权访问"卡片，
 *    并给一个回首页的链接（用户主动点才离开）。
 *
 * ============================ 为什么以接口 403 为最终依据 ============================
 * JWT 里的 role 是登录那一刻的快照。管理员 A 把 B 降级后，B 手里的 token
 * 仍自称 admin —— 只看本地 role 会让 B 看到一个"点什么都是 403"的半成品后台。
 * 所以布局自己发一次概览请求：403 就整页切成无权提示，而不是让下面六个分区
 * 各自弹一个错误态。
 */

import { useState } from 'react';
import { Link, NavLink, Outlet } from 'react-router-dom';
import { ApiError } from '@/api/client';
import { AuthDialog } from '@/components/AuthDialog';
import { Skeleton } from '@/components/states';
import { AlertIcon, CloseIcon } from '@/components/icons';
import { useAdminOverview, useIsAdmin } from '@/hooks/useAdmin';
import { useAuthStore } from '@/stores/auth';

/** 侧边导航项。 */
const NAV_ITEMS: ReadonlyArray<{ to: string; label: string; hint: string; end?: boolean }> = [
  { to: '/admin', label: '概览', hint: '计数、存储、队列', end: true },
  { to: '/admin/users', label: '用户', hint: '角色、停用、删除' },
  { to: '/admin/sources', label: '新闻源', hint: 'RSS 源增删改' },
  { to: '/admin/audio', label: '语音任务', hint: '合成队列与重试' },
  { to: '/admin/content', label: '内容', hint: '各源文章数' },
  { to: '/admin/system', label: '系统', hint: '版本与开关' },
];

export function AdminLayout(): JSX.Element {
  const status = useAuthStore((s) => s.status);
  const isAdmin = useIsAdmin();
  const [authOpen, setAuthOpen] = useState(false);

  // 权限探测：只有"已登录且本地判定为管理员"时才发，
  // 避免每个误入 /admin 的游客都白打一次 401。
  const probe = useAdminOverview(status === 'authenticated' && isAdmin);
  const forbidden = probe.error instanceof ApiError && probe.error.status === 403;

  /* ---------- 1. 会话恢复中：骨架屏（不闪"请登录"） ---------- */
  if (status === 'initializing') {
    return (
      <div className="mx-auto max-w-[1400px] px-4 py-8 sm:px-6">
        <Skeleton className="h-8 w-40" />
        <div className="mt-6 grid grid-cols-2 gap-3 sm:grid-cols-4">
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-24 w-full rounded-xl" />
          ))}
        </div>
        <span className="sr-only">正在检查权限…</span>
      </div>
    );
  }

  /* ---------- 2. 未登录：就地给登录入口 ---------- */
  if (status !== 'authenticated') {
    return (
      <div className="mx-auto max-w-md px-4 py-16">
        <div className="ez-card p-6 text-center">
          <div className="mx-auto mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-brand-soft text-brand dark:text-blue-200">
            <AlertIcon className="text-xl" />
          </div>
          <h1 className="text-base font-semibold text-ink">管理后台需要登录</h1>
          <p className="mt-2 text-[13px] leading-relaxed text-ink-muted">
            后台仅供管理员使用。请使用管理员账号登录后重试。
          </p>
          <button
            type="button"
            onClick={() => setAuthOpen(true)}
            className="ez-btn ez-btn-md ez-btn-primary mt-5"
          >
            登录
          </button>
          <Link
            to="/"
            className="mt-3 block text-[12px] text-ink-faint underline underline-offset-2"
          >
            返回阅读界面
          </Link>
        </div>
        {authOpen ? (
          <AuthDialog
            open
            onClose={() => setAuthOpen(false)}
            // 登录成功不跳转：用户仍在 /admin，下面的守卫会重算（role 变了就自动进后台）。
            onSuccess={() => setAuthOpen(false)}
          />
        ) : null}
      </div>
    );
  }

  /* ---------- 3. 已登录但非管理员（含接口 403）：明确提示，不跳转 ---------- */
  if (!isAdmin || forbidden) {
    return <AdminForbidden reason={forbidden ? 'server' : 'role'} />;
  }

  /* ---------- 4. 管理员：侧栏 + 内容区 ---------- */
  return (
    <div className="mx-auto flex max-w-[1400px] px-4 py-6 sm:px-6">
      {/* 侧边导航：桌面常驻，窄屏收成横向 Tab */}
      <aside className="hidden w-52 shrink-0 md:block">
        <div className="sticky top-20">
          <Link to="/" className="flex items-center gap-2 text-[15px] font-semibold tracking-tight text-ink">
            <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-brand text-sm font-bold text-white">
              E
            </span>
            EZNews
          </Link>
          <p className="mt-1 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">管理后台</p>

          <nav className="mt-4 space-y-1" aria-label="管理后台导航">
            {NAV_ITEMS.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.end}
                className={({ isActive }) =>
                  `block rounded-lg px-3 py-2 transition-colors ${
                    isActive
                      ? 'bg-brand-soft/70 text-ink dark:bg-slate-800'
                      : 'text-ink-muted hover:bg-surface-sunken hover:text-ink'
                  }`
                }
              >
                {({ isActive }) => (
                  <>
                    <span className="block text-[13px] font-medium">{item.label}</span>
                    <span
                      className={`block text-[11px] ${isActive ? 'text-ink-faint' : 'text-ink-faint/80'}`}
                    >
                      {item.hint}
                    </span>
                  </>
                )}
              </NavLink>
            ))}
          </nav>

          <Link
            to="/"
            className="mt-5 inline-flex items-center gap-1.5 text-[12px] text-ink-muted hover:text-ink"
          >
            <CloseIcon className="text-sm" />
            退出后台
          </Link>
        </div>
      </aside>

      {/* 内容区 */}
      <main className="min-w-0 flex-1 md:pl-6">
        {/* 窄屏导航 */}
        <nav className="ez-scrollbar-none -mx-4 mb-4 flex gap-1.5 overflow-x-auto px-4 md:hidden" aria-label="管理后台导航">
          {NAV_ITEMS.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) =>
                `shrink-0 rounded-lg px-3 py-1.5 text-[13px] font-medium transition-colors ${
                  isActive
                    ? 'bg-brand text-white'
                    : 'bg-surface-raised text-ink-muted hover:bg-surface-sunken hover:text-ink'
                }`
              }
            >
              {item.label}
            </NavLink>
          ))}
        </nav>

        {/*
          ★ 概览请求失败（非 403）时**仍然渲染子路由**：六个分区各自有自己的
          错误态与重试按钮，整页拦在这里会让用户连"换个分区看看"都做不到。
        */}
        <Outlet />
      </main>
    </div>
  );
}

/** 无权访问提示（不跳转，给明确原因与出口）。 */
function AdminForbidden({ reason }: { reason: 'role' | 'server' }): JSX.Element {
  return (
    <div className="mx-auto max-w-md px-4 py-16">
      <div className="ez-card p-6 text-center">
        <div className="mx-auto mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-rose-50 text-rose-600 dark:bg-rose-950/50 dark:text-rose-400">
          <AlertIcon className="text-xl" />
        </div>
        <h1 className="text-base font-semibold text-ink">无权访问管理后台</h1>
        <p className="mt-2 text-[13px] leading-relaxed text-ink-muted">
          {reason === 'server'
            ? '服务端拒绝了后台请求（403）。你的账号在服务端已不是管理员 —— 可能刚被其他管理员降级，请重新登录后再试。'
            : '当前账号不是管理员。后台仅对角色为 admin 的账号开放。'}
        </p>
        <Link to="/" className="ez-btn ez-btn-md ez-btn-secondary mt-5">
          返回阅读界面
        </Link>
      </div>
    </div>
  );
}
