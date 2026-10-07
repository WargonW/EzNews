/**
 * 通用状态组件：骨架屏 / 空态 / 错误态 / 离线态。
 *
 * 产品要求（F-WEB-03 + 任务书）：必须覆盖
 * **加载骨架屏、空态（无文章/无搜索结果/未登录）、错误态（可重试）、离线态**。
 * 把它们收敛到一处，保证视觉语言一致，也避免每个页面各写一套。
 */

import type { ReactNode } from 'react';
import { ApiError } from '@/api/client';
import { AlertIcon, CheckCircleIcon, InboxIcon, RefreshIcon, SearchIcon, SpinnerIcon, WifiOffIcon } from './icons';

/* ========================================================================== *
 * 骨架屏
 * ========================================================================== */

/** 单个骨架块。 */
export function Skeleton({ className = '' }: { className?: string }): JSX.Element {
  return <div className={`ez-skeleton ${className}`} aria-hidden="true" />;
}

/** 文章卡片骨架。 */
export function ArticleCardSkeleton({ compact = false }: { compact?: boolean }): JSX.Element {
  if (compact) {
    return (
      <div className="ez-card flex gap-3 p-3">
        <Skeleton className="h-16 w-16 shrink-0 rounded-lg" />
        <div className="min-w-0 flex-1 space-y-2 py-0.5">
          <Skeleton className="h-4 w-4/5" />
          <Skeleton className="h-3 w-2/5" />
        </div>
      </div>
    );
  }
  return (
    <div className="ez-card flex gap-4 p-4">
      <div className="min-w-0 flex-1 space-y-2.5">
        <Skeleton className="h-5 w-4/5" />
        <Skeleton className="h-3.5 w-full" />
        <Skeleton className="h-3.5 w-2/3" />
        <div className="flex items-center gap-2 pt-1">
          <Skeleton className="h-3 w-16" />
          <Skeleton className="h-3 w-12" />
        </div>
      </div>
      <Skeleton className="h-20 w-28 shrink-0 rounded-lg sm:h-24 sm:w-36" />
    </div>
  );
}

/** 列表骨架：首屏展示 6 张卡片。 */
export function ArticleListSkeleton({ count = 6 }: { count?: number }): JSX.Element {
  return (
    <div className="space-y-3" role="status" aria-label="正在加载文章">
      {Array.from({ length: count }, (_, i) => (
        <ArticleCardSkeleton key={i} />
      ))}
      <span className="sr-only">正在加载…</span>
    </div>
  );
}

/** 侧栏骨架。 */
export function SidebarSkeleton(): JSX.Element {
  return (
    <div className="space-y-4 p-4" role="status" aria-label="正在加载新闻源">
      {[4, 6].map((n) => (
        <div key={n} className="space-y-2">
          <Skeleton className="h-3 w-20" />
          <div className="space-y-1.5">
            {Array.from({ length: n }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full rounded-lg" />
            ))}
          </div>
        </div>
      ))}
      <span className="sr-only">正在加载…</span>
    </div>
  );
}

/* ========================================================================== *
 * 空态 / 错误态
 * ========================================================================== */

interface EmptyStateProps {
  icon?: ReactNode;
  title: string;
  description?: ReactNode;
  action?: ReactNode;
  /** 紧凑模式用于侧栏/卡片内。 */
  compact?: boolean;
}

/** 空态。 */
export function EmptyState({ icon, title, description, action, compact = false }: EmptyStateProps): JSX.Element {
  return (
    <div
      className={`flex flex-col items-center justify-center text-center ${
        compact ? 'px-4 py-8' : 'px-6 py-16'
      }`}
    >
      <div
        className={`mb-4 flex items-center justify-center rounded-full bg-surface-sunken text-ink-faint ${
          compact ? 'h-11 w-11' : 'h-16 w-16'
        }`}
      >
        {icon ?? <InboxIcon className={compact ? 'text-xl' : 'text-2xl'} />}
      </div>
      <h3 className={`font-semibold text-ink ${compact ? 'text-sm' : 'text-base'}`}>{title}</h3>
      {description ? (
        <p className="mt-1.5 max-w-sm text-[13px] leading-relaxed text-ink-muted">{description}</p>
      ) : null}
      {action ? <div className="mt-5">{action}</div> : null}
    </div>
  );
}

/** 无搜索结果（与"无文章"区分，给出可操作的下一步）。 */
export function NoSearchResults({ keyword }: { keyword: string }): JSX.Element {
  return (
    <EmptyState
      icon={<SearchIcon className="text-2xl" />}
      title={`没有找到与「${keyword}」相关的文章`}
      description="换个关键词试试，或清除筛选条件浏览全部新闻。搜索范围覆盖标题与摘要。"
    />
  );
}

/** 无文章（首次使用 / 筛选过窄）。 */
export function NoArticles({ hasFilters }: { hasFilters: boolean }): JSX.Element {
  return (
    <EmptyState
      icon={<InboxIcon className="text-2xl" />}
      title={hasFilters ? '当前筛选条件下没有文章' : '还没有任何新闻'}
      description={
        hasFilters
          ? '试试放宽筛选条件，或到「管理来源」里启用更多新闻源。'
          : '新闻由外部采集器写入服务端。请确认采集器已配置并运行，或先到「管理来源」添加一个 RSS 源。'
      }
    />
  );
}

interface ErrorStateProps {
  /** 可选：不传则用默认文案。 */
  error?: unknown;
  onRetry?: () => void;
  title?: string;
  compact?: boolean;
}

/**
 * 把 ApiError 的 code 翻译成人话。
 *
 * 直接把服务端 message 展示给用户是常见错误 —— 它面向开发者（含内部术语）。
 * 这里按 code 分流；无法识别时才回落到 message。
 */
function describeError(error: unknown): { title: string; hint: string | null } {
  if (error instanceof ApiError) {
    if (error.code === 'NETWORK_ERROR') {
      return { title: '网络不可用', hint: '请检查网络连接后重试。已加载的内容仍可继续浏览。' };
    }
    if (error.status === 429) {
      const wait = error.retryAfterSec;
      return {
        title: '请求过于频繁',
        hint: wait ? `服务端建议 ${wait} 秒后重试。` : '服务端触发了限流，请稍后重试。',
      };
    }
    if (error.status === 503) {
      return { title: '服务暂时繁忙', hint: '数据库繁忙或服务过载，通常几秒后自动恢复。' };
    }
    if (error.status === 401) {
      return { title: '登录状态已失效', hint: '已回到游客态，功能不受影响。' };
    }
    if (error.status === 403) {
      return { title: '操作不被允许', hint: error.message };
    }
    if (error.status === 404) {
      return { title: '内容不存在', hint: '它可能已被删除。' };
    }
    if (error.status === 409) {
      return { title: '操作冲突', hint: error.message };
    }
    if (error.status === 413) {
      return { title: '数据量超出限制', hint: '请分批处理，或减少一次提交的数据量。' };
    }
    if (error.status >= 500) {
      return { title: '服务端出错', hint: '这是服务端的问题，稍后重试即可。' };
    }
    return { title: error.message || '请求失败', hint: null };
  }
  if (error instanceof Error) {
    return { title: error.message || '出了点问题', hint: null };
  }
  return { title: '出了点问题', hint: null };
}

/** 错误态（可重试）。 */
export function ErrorState({ error, onRetry, title, compact = false }: ErrorStateProps): JSX.Element {
  const described = describeError(error);
  return (
    <div
      className={`flex flex-col items-center justify-center text-center ${compact ? 'px-4 py-8' : 'px-6 py-14'}`}
      role="alert"
    >
      <div className="mb-4 flex h-14 w-14 items-center justify-center rounded-full bg-rose-50 text-rose-600 dark:bg-rose-950/50 dark:text-rose-400">
        <AlertIcon className="text-2xl" />
      </div>
      <h3 className="text-base font-semibold text-ink">{title ?? described.title}</h3>
      {described.hint ? (
        <p className="mt-1.5 max-w-sm text-[13px] leading-relaxed text-ink-muted">{described.hint}</p>
      ) : null}
      {error instanceof ApiError && error.requestId ? (
        <p className="mt-2 font-mono text-[11px] text-ink-faint">requestId: {error.requestId}</p>
      ) : null}
      {onRetry ? (
        <button type="button" onClick={onRetry} className="ez-btn ez-btn-md ez-btn-secondary mt-5">
          <RefreshIcon />
          重试
        </button>
      ) : null}
    </div>
  );
}

/** 轻量错误条（用于侧栏、面板等非整页区域）。 */
export function InlineError({ error, onRetry }: { error: unknown; onRetry?: () => void }): JSX.Element {
  const described = describeError(error);
  return (
    <div
      className="flex items-start gap-2.5 rounded-lg border border-rose-200 bg-rose-50 p-3 text-[13px] dark:border-rose-900 dark:bg-rose-950/40"
      role="alert"
    >
      <AlertIcon className="mt-0.5 shrink-0 text-base text-rose-600 dark:text-rose-400" />
      <div className="min-w-0 flex-1">
        <p className="font-medium text-ink">{described.title}</p>
        {described.hint ? <p className="mt-0.5 text-ink-muted">{described.hint}</p> : null}
      </div>
      {onRetry ? (
        <button
          type="button"
          onClick={onRetry}
          className="ez-btn ez-btn-sm ez-btn-ghost shrink-0 text-rose-700 dark:text-rose-400"
        >
          <RefreshIcon />
          重试
        </button>
      ) : null}
    </div>
  );
}

/* ========================================================================== *
 * 离线态
 * ========================================================================== */

/**
 * 离线横幅。
 *
 * 设计取向：**不阻断**。离线时已缓存的列表/详情照常可读，
 * 只用一条横幅告知"当前看到的可能是旧内容"。这符合"游客永不被打断"。
 */
export function OfflineBanner({ pending }: { pending?: number }): JSX.Element | null {
  return (
    <div
      className="flex items-center justify-center gap-2 bg-amber-50 px-4 py-1.5 text-[12px] text-amber-800 dark:bg-amber-950/60 dark:text-amber-300"
      role="status"
    >
      <WifiOffIcon className="shrink-0" />
      <span>
        离线模式 —— 显示的是已缓存的内容
        {pending !== undefined && pending > 0 ? `，${pending} 项更新将在恢复网络后拉取` : ''}
      </span>
    </div>
  );
}

/** 顶栏内的紧凑离线指示（离线横幅已经出现时不重复展示）。 */
export function OfflineDot(): JSX.Element {
  return (
    <span
      className="inline-flex items-center gap-1 rounded-md bg-amber-100 px-1.5 py-0.5 text-[11px] font-medium text-amber-800 dark:bg-amber-950 dark:text-amber-300"
      title="离线：正在使用缓存数据"
    >
      <WifiOffIcon />
      离线
    </span>
  );
}

/* ========================================================================== *
 * 轻提示（Toast）
 * ========================================================================== */

/**
 * 轻提示。
 *
 * PRD 要求："已同步 20 条收藏"这类反馈必须是**可关闭的非阻断轻提示**，
 * 不能做成需要用户点"好"的弹窗。所以：自动消失、不抢焦点、不遮挡内容。
 */
export function Toast({
  message,
  tone = 'info',
  onClose,
}: {
  message: string;
  tone?: 'info' | 'success' | 'warn';
  onClose?: () => void;
}): JSX.Element {
  const toneClass =
    tone === 'success'
      ? 'border-emerald-200 bg-emerald-50 text-emerald-900 dark:border-emerald-900 dark:bg-emerald-950 dark:text-emerald-100'
      : tone === 'warn'
        ? 'border-amber-200 bg-amber-50 text-amber-900 dark:border-amber-900 dark:bg-amber-950 dark:text-amber-100'
        : 'border-line bg-surface-raised text-ink';

  return (
    <div
      className={`ez-enter pointer-events-auto flex items-center gap-2 rounded-lg border px-3.5 py-2.5 text-[13px] shadow-pop ${toneClass}`}
      role="status"
      aria-live="polite"
    >
      {tone === 'success' ? <CheckCircleIcon className="shrink-0 text-base" /> : null}
      <span className="min-w-0">{message}</span>
      {onClose ? (
        <button type="button" onClick={onClose} className="ml-1 shrink-0 opacity-60 hover:opacity-100" aria-label="关闭">
          ×
        </button>
      ) : null}
    </div>
  );
}

/** 加载中的行内指示器（按钮内联用）。 */
export function InlineSpinner({ label }: { label?: string }): JSX.Element {
  return (
    <span className="inline-flex items-center gap-1.5">
      <SpinnerIcon />
      {label ? <span>{label}</span> : null}
    </span>
  );
}