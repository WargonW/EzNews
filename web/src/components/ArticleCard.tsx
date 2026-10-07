/**
 * 文章卡片。
 *
 * 视觉与交互约定（对齐 PRD §8.1）：
 * - 缩略图 + 标题 + 摘要 + 来源徽标/时间/分类
 * - 悬停浮起（shadow 变化），点击进详情
 * - 已读文章降低标题对比度（但仍可读，不隐藏内容）
 *
 * 键盘可达：整卡是一个 `<article>`，内部用链接/按钮，
 * 避免"整卡包在 `<a>` 里再套按钮"的非法嵌套。
 */

import { memo } from 'react';
import { Link } from 'react-router-dom';
import type { ArticleDTO } from '@/api/types';
import { formatRelative } from '@/lib/datetime';
import { CATEGORY_TONE, categoryLabel } from '@/lib/constants';
import { CheckIcon, ExternalLinkIcon, PlayIcon, StarIcon } from './icons';

export interface ArticleCardProps {
  article: ArticleDTO;
  /** 本地（含游客态）收藏态。 */
  favorited: boolean;
  /** 本地（含游客态）已读态。 */
  read: boolean;
  /** 有音频可播（列表页 withAudio=true 时服务端会给出状态）。 */
  hasAudio: boolean;
  onToggleFavorite: (articleId: number) => void;
  onPlay: (article: ArticleDTO) => void;
  /** 紧凑模式（用于侧栏"收藏"页等空间受限处）。 */
  compact?: boolean;
}

function ArticleCardImpl({
  article,
  favorited,
  read,
  hasAudio,
  onToggleFavorite,
  onPlay,
  compact = false,
}: ArticleCardProps): JSX.Element {
  const tone = CATEGORY_TONE[article.category] ?? CATEGORY_TONE.other;

  if (compact) {
    return (
      <article className="ez-card ez-card-interactive group relative flex gap-3 p-3">
        <Link
          to={`/article/${article.id}`}
          className="flex min-w-0 flex-1 gap-3"
          aria-label={article.title}
        >
          <div className="min-w-0 flex-1">
            <h3
              className={`ez-line-clamp-2 text-sm font-medium leading-snug ${
                read ? 'text-ink-muted' : 'text-ink'
              }`}
            >
              {article.title}
            </h3>
            <p className="mt-1.5 flex items-center gap-1.5 text-[11px] text-ink-faint">
              <span className="truncate">{article.sourceName}</span>
              <span aria-hidden="true">·</span>
              <time dateTime={article.publishedAt}>{formatRelative(article.publishedAt)}</time>
            </p>
          </div>
          {article.imageUrl ? (
            <img
              src={article.imageUrl}
              alt=""
              loading="lazy"
              decoding="async"
              className="h-14 w-14 shrink-0 rounded-lg object-cover"
            />
          ) : null}
        </Link>

        <div className="absolute right-2 top-2 flex gap-1 opacity-0 transition-opacity focus-within:opacity-100 group-hover:opacity-100">
          <FavoriteButton active={favorited} onClick={() => onToggleFavorite(article.id)} />
        </div>
      </article>
    );
  }

  return (
    <article className="ez-card ez-card-interactive group relative overflow-hidden">
      <div className="flex gap-4 p-4">
        <Link to={`/article/${article.id}`} className="flex min-w-0 flex-1 gap-4">
          <div className="min-w-0 flex-1">
            {/* 已读用左侧竖条 + 标题降对比度表示，不隐藏内容 */}
            <h3
              className={`ez-line-clamp-2 text-[15px] font-semibold leading-snug ${
                read ? 'text-ink-muted' : 'text-ink'
              }`}
            >
              {article.title}
            </h3>

            {article.summary ? (
              <p className="ez-line-clamp-2 mt-1.5 text-[13px] leading-relaxed text-ink-muted">
                {article.summary}
              </p>
            ) : null}

            <div className="mt-2.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-[11px] text-ink-faint">
              <span className="ez-badge bg-brand-soft text-brand dark:text-blue-200">{article.sourceName}</span>
              <span className={`ez-badge ${tone}`}>{categoryLabel(article.category)}</span>
              <time dateTime={article.publishedAt}>{formatRelative(article.publishedAt)}</time>
              {article.author ? (
                <>
                  <span aria-hidden="true">·</span>
                  <span className="max-w-[12rem] truncate">{article.author}</span>
                </>
              ) : null}
            </div>
          </div>

          {article.imageUrl ? (
            <img
              src={article.imageUrl}
              alt=""
              loading="lazy"
              decoding="async"
              className="h-20 w-28 shrink-0 rounded-lg object-cover sm:h-24 sm:w-36"
              onError={(e) => {
                // 缩略图 404/加载失败时隐藏，避免出现破图占位。
                e.currentTarget.style.display = 'none';
              }}
            />
          ) : null}
        </Link>

        {/* 悬浮操作：始终对键盘可见（focus-within），鼠标用户悬停显示 */}
        <div className="absolute right-3 top-3 flex items-center gap-1 opacity-0 transition-opacity focus-within:opacity-100 group-hover:opacity-100">
          {hasAudio ? (
            <button
              type="button"
              onClick={() => onPlay(article)}
              className="ez-btn ez-btn-sm h-8 w-8 rounded-lg bg-surface-raised/90 p-0 text-ink-muted shadow-card backdrop-blur hover:text-brand"
              title="播放语音"
              aria-label={`播放《${article.title}》的语音`}
            >
              <PlayIcon className="text-xs" />
            </button>
          ) : null}
          <FavoriteButton active={favorited} onClick={() => onToggleFavorite(article.id)} />
        </div>
      </div>

      {/* 已读进度条：底部 2px，视觉上不抢注意力但可感知 */}
      {read ? <div className="h-0.5 w-full bg-brand-soft dark:bg-slate-700" aria-hidden="true" /> : null}
    </article>
  );
}

/**
 * 收藏按钮。
 *
 * PRD R3：游客点击收藏**必须立即生效于本地**，不得先要求登录。
 * 所以这个按钮在任何状态下都可点，不做"未登录禁用"处理。
 */
function FavoriteButton({ active, onClick }: { active: boolean; onClick: () => void }): JSX.Element {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`ez-btn ez-btn-sm h-8 w-8 rounded-lg bg-surface-raised/90 p-0 shadow-card backdrop-blur ${
        active ? 'text-amber-500' : 'text-ink-faint hover:text-amber-500'
      }`}
      title={active ? '取消收藏' : '收藏'}
      aria-label={active ? '取消收藏' : '收藏'}
      aria-pressed={active}
    >
      <StarIcon filled={active} className="text-sm" />
    </button>
  );
}

/**
 * memo 化：列表翻页时 React 会重渲染所有卡片，但只有变化的卡片需要重绘。
 * 收藏/已读态由外部传入，因此 props 浅比较即可命中。
 */
export const ArticleCard = memo(ArticleCardImpl);

/** 列表底部的"加载更多"区（骨架 / 加载中 / 没有更多了）。 */
export function ListFooter({
  loading,
  hasMore,
  onLoadMore,
  total,
}: {
  loading: boolean;
  hasMore: boolean;
  onLoadMore: () => void;
  total: number;
}): JSX.Element | null {
  if (!hasMore && total > 0) {
    return (
      <p className="py-8 text-center text-[12px] text-ink-faint">
        已经到底了 · 共 {total} 篇
      </p>
    );
  }
  if (!hasMore) return null;

  return (
    <div className="py-6 text-center">
      {loading ? (
        <span className="inline-flex items-center gap-2 text-[13px] text-ink-muted">
          <span className="h-3.5 w-3.5 animate-spin rounded-full border-2 border-slate-300 border-t-brand" />
          加载中…
        </span>
      ) : (
        <button type="button" onClick={onLoadMore} className="ez-btn ez-btn-md ez-btn-secondary">
          加载更多
        </button>
      )}
    </div>
  );
}

/** 原文链接（详情页用）。 */
export function OriginalLink({ url }: { url: string }): JSX.Element {
  return (
    <a
      href={url}
      target="_blank"
      rel="noopener noreferrer"
      className="ez-btn ez-btn-sm ez-btn-ghost"
      title="在新标签页打开原文"
    >
      <ExternalLinkIcon />
      阅读原文
    </a>
  );
}

/** 已读标记（图例说明用）。 */
export function ReadMark(): JSX.Element {
  return (
    <span className="inline-flex items-center gap-1 text-[11px] text-brand">
      <CheckIcon className="text-xs" />
      已读
    </span>
  );
}