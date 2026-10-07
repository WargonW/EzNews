/**
 * 文章详情页。
 *
 * ============================ 关键产品约束 ============================
 * 1. **游客完整可读**：不因未登录隐藏任何内容、不弹登录窗（PRD R1）。
 * 2. **自动标记已读**：进入详情即写本地已读（含墓碑语义），登录后同步云端。
 *    这是"已读"功能最自然的触发点，比让用户手动点更符合直觉。
 * 3. **语音播报**：服务端合成，客户端只播放（见 useArticleAudio）。
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ApiError } from '@/api/client';
import { getArticle } from '@/api/endpoints';
import type { ArticleDTO } from '@/api/types';
import { CATEGORY_TONE, categoryLabel } from '@/lib/constants';
import { formatAbsolute, formatRelative } from '@/lib/datetime';
import { articleKeys } from '@/lib/queryKeys';
import { useArticleAudio } from '@/hooks/useArticleAudio';
import { useFavoriteActions } from '@/hooks/useFavoriteActions';
import { DEFAULT_PREFERENCES, favoriteIdsOf, prefNumber, prefString, useGuestStore } from '@/stores/guest';
import { useAuthStore } from '@/stores/auth';
import { usePlayerStore } from '@/stores/player';
import { AudioPlayerBar } from '@/components/AudioPlayerBar';
import { OriginalLink } from '@/components/ArticleCard';
import { ArticleListSkeleton, ErrorState, Toast } from '@/components/states';
import { CheckIcon, PlayIcon, RefreshIcon, SpinnerIcon, StarIcon } from '@/components/icons';

/** 音频合成态 → 按钮文案。 */
const PLAY_LABEL: Record<string, string> = {
  idle: '转语音并播放',
  checking: '查询缓存…',
  submitting: '提交合成…',
  synthesizing: '合成中…',
  ready: '播放',
  failed: '重试合成',
};

export function ArticleDetailPage(): JSX.Element {
  const { id: idParam } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const articleId = Number(idParam);
  const valid = Number.isInteger(articleId) && articleId > 0;

  /* ---------------- 偏好（音色/语速） ---------------- */
  const preferences = useGuestStore((s) => s.preferences);
  const voice = prefString(preferences, 'tts.voice', DEFAULT_PREFERENCES['tts.voice']);
  const speed = prefNumber(preferences, 'tts.speed', 1);

  const [toast, setToast] = useState<string | null>(null);
  useEffect(() => {
    if (!toast) return;
    const t = window.setTimeout(() => setToast(null), 3000);
    return () => window.clearTimeout(t);
  }, [toast]);

  /* ---------------- 文章详情 ---------------- */
  const query = useQuery({
    queryKey: articleKeys.detail(articleId),
    queryFn: () => getArticle(articleId),
    enabled: valid,
    // 服务端给详情接口加了 ETag + max-age=60，浏览器与 TanStack 双重缓存。
    staleTime: 60_000,
  });

  const article: ArticleDTO | undefined = query.data;

  /* ---------------- 收藏 / 已读 ---------------- */
  /* 收藏态：订阅原始 map（引用稳定）后派生 id 集合，详见 stores/guest.ts 的说明。 */
  const favoritesMap = useGuestStore((s) => s.favorites);
  const favorites = useMemo(() => favoriteIdsOf(favoritesMap), [favoritesMap]);
  const loggedIn = useAuthStore((s) => s.status === 'authenticated');
  const { toggleFavorite, toggleRead } = useFavoriteActions(
    useCallback((message: string) => setToast(message), []),
  );

  /**
   * ★ 进入详情自动标记已读。
   *
   * 用"已标记过的 articleId"来保证只标一次：React 严格模式会双调用 effect，
   * 若不设防会连发两次写请求（多余的网络 + 多写一个墓碑）。
   * 换成 ref 而非 useMemo 标志位：ref 天然跨渲染存活且不会触发 exhaustive-deps 误报，
   * 而"换一篇文章要重新计一次"由比较 articleId 本身即可满足。
   *
   * 服务端已标记为已读（isRead=true）时不再重复标记。
   */
  const markedArticleId = useRef<number | null>(null);
  useEffect(() => {
    if (!article || !valid) return;
    if (markedArticleId.current === articleId) return;
    markedArticleId.current = articleId;
    if (article.isRead === true) return;
    // toggleRead 内部会写本地，并在登录态下同步云端。
    toggleRead(articleId);
  }, [article, articleId, valid, toggleRead]);

  const favorited = useMemo(() => {
    if (loggedIn && article?.isFavorited !== undefined) return article.isFavorited;
    return favorites.has(articleId);
  }, [loggedIn, article, favorites, articleId]);

  /* ---------------- 语音 ---------------- */
  const speech = useArticleAudio(articleId, voice, speed);
  const isSynthesizing =
    speech.phase === 'checking' || speech.phase === 'submitting' || speech.phase === 'synthesizing';

  const playQueue = usePlayerStore((s) => s.play);

  /** 单条播放（不建队列）。 */
  const playSingle = useCallback(
    (url: string) => {
      playQueue(
        [
          {
            articleId,
            title: article?.title ?? '',
            url,
            voice,
            speed,
            ...(speech.audio ? { durationMsHint: speech.audio.durationMs } : {}),
          },
        ],
        0,
      );
    },
    [articleId, article, voice, speed, speech.audio, playQueue],
  );

  /** 播放：未就绪则先合成，就绪则交给播放器。 */
  const handlePlay = useCallback(async (): Promise<void> => {
    if (!valid) return;
    if (speech.phase === 'ready' && speech.audioUrl) {
      playSingle(speech.audioUrl);
      return;
    }
    if (speech.phase === 'failed') {
      await speech.retry();
      return;
    }
    const url = await speech.play();
    if (url) playSingle(url);
  }, [speech, valid, playSingle]);

  /* ---------------- 渲染 ---------------- */
  if (!valid) {
    return (
      <div className="mx-auto max-w-2xl px-4 py-16">
        <ErrorState title="文章 ID 不合法" />
      </div>
    );
  }

  if (query.isLoading) {
    return (
      <div className="mx-auto max-w-2xl px-4 py-8 sm:px-6">
        <div className="space-y-3">
          <ArticleListSkeleton count={1} />
        </div>
      </div>
    );
  }

  if (query.isError) {
    const err = query.error;
    // 404 是最常见的正常路径（文章被删除），给专门的文案与返回入口。
    if (err instanceof ApiError && err.status === 404) {
      return (
        <div className="mx-auto max-w-2xl px-4 py-16">
          <ErrorState
            title="这篇文章不存在或已被删除"
            onRetry={undefined}
          />
          <div className="text-center">
            <Link to="/" className="ez-btn ez-btn-md ez-btn-secondary">
              返回首页
            </Link>
          </div>
        </div>
      );
    }
    return (
      <div className="mx-auto max-w-2xl px-4 py-16">
        <ErrorState error={err} onRetry={() => void query.refetch()} />
      </div>
    );
  }

  if (!article) {
    return (
      <div className="mx-auto max-w-2xl px-4 py-16">
        <ErrorState title="未找到文章" />
      </div>
    );
  }

  const tone = CATEGORY_TONE[article.category] ?? CATEGORY_TONE.other;

  return (
    <div className="mx-auto max-w-2xl px-4 pb-28 pt-6 sm:px-6">
      {/* 返回 */}
      <button
        type="button"
        onClick={() => navigate(-1)}
        className="ez-btn ez-btn-sm ez-btn-ghost -ml-1.5 mb-4"
      >
        ← 返回
      </button>

      {/* 封面图 */}
      {article.imageUrl ? (
        <img
          src={article.imageUrl}
          alt=""
          className="mb-5 aspect-[2/1] w-full rounded-xl object-cover"
          onError={(e) => {
            e.currentTarget.style.display = 'none';
          }}
        />
      ) : null}

      {/* 分类 */}
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className={`ez-badge ${tone}`}>{categoryLabel(article.category)}</span>
        <span className="ez-badge bg-brand-soft text-brand dark:text-blue-200">{article.sourceName}</span>
      </div>

      {/* 标题 */}
      <h1 className="text-[22px] font-bold leading-snug tracking-tight text-ink sm:text-2xl">
        {article.title}
      </h1>

      {/* 元信息 */}
      <div className="mt-3 flex flex-wrap items-center gap-x-2.5 gap-y-1 text-[12px] text-ink-muted">
        {article.author ? <span>{article.author}</span> : null}
        <time dateTime={article.publishedAt} title={formatAbsolute(article.publishedAt)}>
          {formatRelative(article.publishedAt)}
        </time>
        {article.tags.length > 0 ? (
          <span className="flex flex-wrap gap-1">
            {article.tags.map((t) => (
              <span key={t} className="ez-badge bg-surface-sunken text-ink-muted">
                #{t}
              </span>
            ))}
          </span>
        ) : null}
      </div>

      {/* 操作条 */}
      <div className="sticky top-14 z-10 -mx-4 mt-5 border-y border-line bg-surface/95 px-4 py-2.5 backdrop-blur sm:-mx-6 sm:px-6">
        <div className="flex flex-wrap items-center gap-2">
          {/* 播放 / 合成 */}
          <button
            type="button"
            onClick={() => void handlePlay()}
            disabled={isSynthesizing}
            className="ez-btn ez-btn-md ez-btn-primary px-4"
          >
            {isSynthesizing ? <SpinnerIcon /> : <PlayIcon className="text-xs" />}
            {PLAY_LABEL[speech.phase] ?? '播放'}
          </button>

          {speech.phase === 'failed' ? (
            <button
              type="button"
              onClick={() => void speech.retry()}
              className="ez-btn ez-btn-md ez-btn-secondary"
            >
              <RefreshIcon />
              重试
            </button>
          ) : null}

          {/* 收藏 */}
          <button
            type="button"
            onClick={() => toggleFavorite(article.id)}
            className={`ez-btn ez-btn-md ${favorited ? 'ez-btn-secondary text-amber-500' : 'ez-btn-secondary'}`}
            aria-pressed={favorited}
          >
            <StarIcon filled={favorited} />
            {favorited ? '已收藏' : '收藏'}
          </button>

          <OriginalLink url={article.url} />
        </div>

        {/* 合成进度 / 失败原因 */}
        {speech.phase === 'synthesizing' && speech.progressText ? (
          <p className="mt-2 flex items-center gap-1.5 text-[12px] text-ink-muted" role="status">
            <SpinnerIcon />
            {speech.progressText}
          </p>
        ) : null}
        {speech.phase === 'failed' && speech.error ? (
          <p className="mt-2 text-[12px] text-rose-600 dark:text-rose-400" role="alert">
            {speech.error}
          </p>
        ) : null}
      </div>

      {/* 摘要 */}
      {article.summary ? (
        <p className="ez-prose mt-5 border-l-2 border-brand-soft pl-4 text-[15px] text-ink-muted">
          {article.summary}
        </p>
      ) : null}

      {/* 正文 */}
      {article.content ? (
        <div className="ez-prose mt-5">
          {article.content.split(/\n{2,}/).map((para, i) => (
            <p key={i}>{para}</p>
          ))}
        </div>
      ) : (
        <div className="mt-6 rounded-xl border border-dashed border-line bg-surface-sunken p-5 text-center">
          <p className="text-[13px] text-ink-muted">本篇未收录正文</p>
          <p className="mt-1 text-[12px] text-ink-faint">
            服务端默认只存标题与摘要（省存储、降低版权风险）。点击「阅读原文」查看完整内容。
          </p>
          <OriginalLink url={article.url} />
        </div>
      )}

      {/* 底部提示：登录入口只在设置面板，不在这里打断 */}
      <div className="mt-10 flex items-center justify-between border-t border-line pt-5 text-[12px] text-ink-faint">
        <span className="flex items-center gap-1">
          <CheckIcon className="text-xs" />
          已自动标记为已读
        </span>
        <Link to="/" className="underline underline-offset-2 hover:text-ink-muted">
          返回全部新闻
        </Link>
      </div>

      {/* 底部播放器 */}
      <AudioPlayerBar src={speech.phase === 'ready' ? speech.audioUrl : null} />

      {/* 轻提示 */}
      {toast ? (
        <div className="pointer-events-none fixed bottom-20 left-1/2 z-50 w-full max-w-sm -translate-x-1/2 px-4">
          <Toast message={toast} onClose={() => setToast(null)} />
        </div>
      ) : null}
    </div>
  );
}