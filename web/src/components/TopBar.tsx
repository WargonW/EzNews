/**
 * 顶栏：品牌 + 全局搜索 + 主题切换 + 源管理入口 + 登录入口。
 *
 * ★ PRD R2 硬性规则：**登录入口固定在顶栏与设置面板两处**，
 *   不随内容穿插出现。因此这里只有一处"登录/我的"按钮，不做浮动气泡、不做遮罩。
 */

import { useEffect, useRef, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { useOnlineStatus } from '@/hooks/useOnlineStatus';
import { OfflineDot } from './states';
import { CloseIcon, MenuIcon, SearchIcon, SettingsIcon, ShieldIcon } from './icons';

export interface TopBarProps {
  /** 搜索框内容（由外壳从 URL 同步而来，作为 draft 的初值与外部校正源）。 */
  keyword: string;
  onSubmit: (value: string) => void;
  /** 移动端抽屉开关。 */
  onOpenDrawer: () => void;
  /** 打开设置面板。 */
  onOpenSettings: () => void;
  /** 登录态用户名；null = 游客。 */
  username: string | null;
  /** 正在恢复会话（此时登录入口显示骨架，避免误显示"游客"再跳变）。 */
  initializing: boolean;
  /** 登录/注册弹窗。 */
  onOpenAuth: () => void;
  /** 主题：system | light | dark。 */
  theme: ThemeMode;
  onThemeChange: (t: ThemeMode) => void;
  /**
   * 当前账号是否为管理员（决定要不要显示后台入口）。
   *
   * ★ 只是"显不显示"的预判，不是权限依据：JWT 里的 role 可能已过期，
   *   真正的判定在服务端（后台接口对非 admin 一律 403），
   *   `/admin` 内部还有一层守卫负责处理这种情况。
   */
  isAdmin?: boolean;
}

export type ThemeMode = 'system' | 'light' | 'dark';

export function TopBar({
  keyword,
  onSubmit,
  onOpenDrawer,
  onOpenSettings,
  username,
  initializing,
  onOpenAuth,
  theme,
  onThemeChange,
  isAdmin = false,
}: TopBarProps): JSX.Element {
  const online = useOnlineStatus();
  const navigate = useNavigate();
  const [draft, setDraft] = useState(keyword);
  const inputRef = useRef<HTMLInputElement>(null);

  // 外部筛选变化（如点了侧栏的分类）时同步搜索框内容。
  useEffect(() => setDraft(keyword), [keyword]);

  // "/" 聚焦搜索框：键盘用户的肌肉记忆。
  useEffect(() => {
    const onKey = (e: KeyboardEvent): void => {
      const target = e.target as HTMLElement | null;
      const typing =
        target instanceof HTMLInputElement ||
        target instanceof HTMLTextAreaElement ||
        target?.isContentEditable === true;
      if (e.key === '/' && !typing) {
        e.preventDefault();
        inputRef.current?.focus();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  return (
    <header className="sticky top-0 z-30 border-b border-line bg-surface-raised/85 backdrop-blur-md">
      <div className="mx-auto flex h-14 max-w-[1600px] items-center gap-2 px-3 sm:gap-3 sm:px-5">
        {/* 移动端抽屉开关 */}
        <button
          type="button"
          onClick={onOpenDrawer}
          className="ez-btn ez-btn-sm ez-btn-ghost h-9 w-9 shrink-0 p-0 lg:hidden"
          aria-label="打开筛选菜单"
        >
          <MenuIcon className="text-lg" />
        </button>

        <Link to="/" className="flex shrink-0 items-center gap-2" aria-label="EZNews 首页">
          <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-brand text-sm font-bold text-white">
            E
          </span>
          <span className="hidden text-[15px] font-semibold tracking-tight text-ink sm:inline">EZNews</span>
        </Link>

        {/* 搜索框 */}
        <form
          className="min-w-0 flex-1"
          role="search"
          onSubmit={(e) => {
            e.preventDefault();
            onSubmit(draft.trim());
          }}
        >
          <div className="relative mx-auto max-w-xl">
            <SearchIcon className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-base text-ink-faint" />
            <input
              ref={inputRef}
              type="search"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder="搜索新闻、来源、关键词…"
              aria-label="搜索新闻"
              maxLength={64}
              className="ez-input h-9 pl-9 pr-8"
            />
            {draft ? (
              <button
                type="button"
                onClick={() => {
                  setDraft('');
                  onSubmit('');
                  inputRef.current?.focus();
                }}
                className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-1 text-ink-faint hover:text-ink"
                aria-label="清除搜索"
              >
                <CloseIcon className="text-sm" />
              </button>
            ) : null}
            {/* "/ 快捷键"提示：桌面端才有意义 */}
            <kbd className="pointer-events-none absolute right-2.5 top-1/2 hidden -translate-y-1/2 rounded border border-line px-1.5 py-0.5 font-mono text-[10px] text-ink-faint md:block">
              /
            </kbd>
          </div>
        </form>

        {!online ? <OfflineDot /> : null}

        {/* 主题切换：system 跟随 prefers-color-scheme（任务书要求） */}
        <ThemeToggle theme={theme} onChange={onThemeChange} />

        {/* 源管理入口 */}
        <button
          type="button"
          onClick={onOpenSettings}
          className="ez-btn ez-btn-sm ez-btn-ghost hidden h-9 shrink-0 sm:inline-flex"
          title="管理新闻来源与设置"
        >
          <SettingsIcon className="text-base" />
          <span className="hidden lg:inline">管理来源</span>
        </button>

        {/* 管理后台入口：只对管理员显示，避免让运维者靠手敲 /admin */}
        {isAdmin ? (
          <Link
            to="/admin"
            className="ez-btn ez-btn-sm ez-btn-ghost hidden h-9 shrink-0 sm:inline-flex"
            title="进入管理后台"
          >
            <ShieldIcon className="text-base" />
            <span className="hidden lg:inline">后台</span>
          </Link>
        ) : null}

        {/* ★ 登录入口（R2：仅此一处 + 设置面板一处） */}
        {username ? (
          <button
            type="button"
            onClick={() => navigate('/settings')}
            className="ez-btn ez-btn-sm h-9 shrink-0 rounded-lg border border-line px-2.5 text-[13px] font-medium text-ink hover:bg-surface-sunken"
            title="账号与偏好设置"
          >
            <span className="flex h-5 w-5 items-center justify-center rounded-full bg-brand-soft text-[11px] font-semibold text-brand dark:text-blue-200">
              {username.slice(0, 1).toUpperCase()}
            </span>
            <span className="hidden max-w-[6rem] truncate sm:inline">{username}</span>
          </button>
        ) : initializing ? (
          <div className="h-9 w-20 shrink-0 animate-pulse rounded-lg bg-surface-sunken" aria-hidden="true" />
        ) : (
          <button type="button" onClick={onOpenAuth} className="ez-btn ez-btn-sm ez-btn-primary h-9 shrink-0 px-3.5">
            登录
          </button>
        )}
      </div>
    </header>
  );
}

/** 主题切换按钮（system → light → dark 循环）。 */
function ThemeToggle({ theme, onChange }: { theme: ThemeMode; onChange: (t: ThemeMode) => void }): JSX.Element {
  const [isDark, setIsDark] = useState(() =>
    typeof matchMedia !== 'undefined' ? matchMedia('(prefers-color-scheme: dark)').matches : false,
  );

  // system 模式下需跟随系统实时变化。
  useEffect(() => {
    if (theme !== 'system') return;
    const mq = matchMedia('(prefers-color-scheme: dark)');
    const onChangeMq = (e: MediaQueryListEvent): void => setIsDark(e.matches);
    mq.addEventListener('change', onChangeMq);
    return () => mq.removeEventListener('change', onChangeMq);
  }, [theme]);

  useEffect(() => {
    if (theme === 'dark') setIsDark(true);
    else if (theme === 'light') setIsDark(false);
  }, [theme]);

  const effectiveDark = theme === 'dark' || (theme === 'system' && isDark);

  const label: Record<ThemeMode, string> = {
    system: '跟随系统',
    light: '浅色',
    dark: '深色',
  };

  return (
    <button
      type="button"
      onClick={() => {
        const next: ThemeMode = theme === 'system' ? 'light' : theme === 'light' ? 'dark' : 'system';
        onChange(next);
      }}
      className="ez-btn ez-btn-sm ez-btn-ghost h-9 w-9 shrink-0 p-0"
      title={`主题：${label[theme]}${effectiveDark ? '（当前深色）' : ''}`}
      aria-label={`主题：${label[theme]}`}
    >
      {effectiveDark ? <MoonIcon /> : <SunIcon />}
    </button>
  );
}

function SunIcon(): JSX.Element {
  return (
    <svg viewBox="0 0 24 24" className="text-base" fill="none" stroke="currentColor" strokeWidth={1.75} strokeLinecap="round" aria-hidden="true">
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
    </svg>
  );
}

function MoonIcon(): JSX.Element {
  return (
    <svg viewBox="0 0 24 24" className="text-base" fill="none" stroke="currentColor" strokeWidth={1.75} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z" />
    </svg>
  );
}