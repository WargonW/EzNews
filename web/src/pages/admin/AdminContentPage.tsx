/**
 * 后台「内容」分区（`/admin/content`）。
 *
 * ============================ ★ 三个口径必须同时说清楚 ============================
 * 这是本页唯一容易"看起来像 bug"的地方：
 * - `total` = **本响应 topSources 各源文章数之和**（Top N，不是全库）；
 * - `orphanArticles` = 不属于任何现存源的文章（源被删后的残留），
 *   它**不计入** `total`，也不会出现在表格任何一行；
 * - 全库文章总数要看**概览**的 `articles`。
 * 三者都显式展示并写明口径，否则用户看到"表格合计 < 概览总数"会以为接口算错了。
 *
 * ============================ 时间口径 ============================
 * `last24h` / `last7d` 按文章的 **published_at** 算，不是服务端采集写入时间 ——
 * 一条昨天发布、今天补抓的新闻，在内容视角下就该算进 24 小时。UI 上必须标注，
 * 否则运维会拿它去反推"采集器是不是挂了"，结论正好相反。
 */

import { useMemo } from 'react';
import { EmptyState, ErrorState, Skeleton } from '@/components/states';
import { formatRelativeSec } from '@/lib/datetime';
import { useAdminContentStats, useAdminOverview } from '@/hooks/useAdmin';
import { CATEGORY_LABELS } from '@/lib/constants';
import { PageHeader } from './AdminOverviewPage';

export function AdminContentPage(): JSX.Element {
  const query = useAdminContentStats();
  // 全库文章总数只有概览里有；内容接口那个 total 是 Top 源的合计（非全库）。
  const overview = useAdminOverview();

  const byCategory = useMemo(() => {
    return [...(query.data?.byCategory ?? [])].sort((a, b) => b.count - a.count);
  }, [query.data]);

  if (query.isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-7 w-32" />
        <div className="space-y-2">
          {[0, 1, 2, 3, 4].map((i) => (
            <Skeleton key={i} className="h-14 w-full rounded-xl" />
          ))}
        </div>
        <span className="sr-only">正在加载内容统计…</span>
      </div>
    );
  }

  if (query.isError) {
    return <ErrorState error={query.error} onRetry={() => void query.refetch()} title="内容统计加载失败" />;
  }

  const data = query.data;
  if (!data) {
    return <EmptyState title="服务端未返回内容统计" description="请确认服务端已启用管理后台接口。" />;
  }

  if (data.topSources.length === 0 && byCategory.length === 0) {
    return (
      <div className="space-y-5">
        <PageHeader title="内容" subtitle="文章分布：分类、来源与增量" />
        <EmptyState
          title="还没有内容统计"
          description="服务端尚未采集到任何文章，或还没有配置新闻源。可先到「新闻源」分区添加一个 RSS 源。"
        />
      </div>
    );
  }

  const allArticles = overview.data?.articles;

  return (
    <div className="space-y-5">
      <PageHeader
        title="内容"
        subtitle={`Top ${data.topSources.length} 个来源合计 ${data.total.toLocaleString('zh-CN')} 篇文章`}
      />

      {/* ---------- 顶部计数 ---------- */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
        <MiniCard label="Top 源合计" value={data.total.toLocaleString('zh-CN')} />
        <MiniCard
          label="全库文章"
          value={allArticles !== undefined ? allArticles.toLocaleString('zh-CN') : '—'}
          hint={allArticles === undefined ? '概览接口未加载' : undefined}
        />
        <MiniCard label="近 24 小时" value={data.last24h.toLocaleString('zh-CN')} />
        <MiniCard label="近 7 天" value={data.last7d.toLocaleString('zh-CN')} />
        <MiniCard
          label="已停用源"
          value={data.disabledSources.toLocaleString('zh-CN')}
          hint={data.disabledSources > 0 ? '停用的源不再进入默认列表' : undefined}
        />
      </div>
      <p className="-mt-2 text-[11px] text-ink-faint">
        「近 24 小时 / 近 7 天」按文章的 <strong className="font-medium text-ink-muted">published_at</strong> 统计，
        不是采集写入时间。
      </p>

      {/* ---------- 按源 Top N ---------- */}
      <section>
        <h2 className="text-[11px] font-semibold uppercase tracking-wide text-ink-faint">按来源（Top {data.topSources.length}）</h2>
        <div className="ez-card mt-2 overflow-hidden">
          <table className="w-full text-left text-[13px]">
            <thead className="border-b border-line bg-surface-sunken text-[11px] uppercase tracking-wide text-ink-faint">
              <tr>
                <th scope="col" className="px-3.5 py-2 font-semibold">
                  来源
                </th>
                <th scope="col" className="w-20 px-3.5 py-2 text-right font-semibold">
                  文章数
                </th>
                <th scope="col" className="hidden w-28 px-3.5 py-2 font-semibold sm:table-cell">
                  最新发布
                </th>
              </tr>
            </thead>
            <tbody>
              {data.topSources.map((s) => (
                <tr key={s.sourceId} className="border-b border-line last:border-0">
                  <td className="px-3.5 py-2.5">
                    <span className="flex flex-wrap items-center gap-1.5">
                      <span className="truncate text-ink">{s.sourceName}</span>
                      {!s.enabled ? (
                        <span className="ez-badge bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300">
                          已停用
                        </span>
                      ) : null}
                    </span>
                    <span className="block text-[11px] text-ink-faint">#{s.sourceId}</span>
                  </td>
                  <td className="px-3.5 py-2.5 text-right tabular-nums text-ink">
                    {s.articleCount.toLocaleString('zh-CN')}
                  </td>
                  <td className="hidden px-3.5 py-2.5 text-ink-muted sm:table-cell">
                    {s.lastSeenAt > 0 ? formatRelativeSec(s.lastSeenAt) : '暂无文章'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {/*
          ★ 脚注：orphanArticles 必须显式写出来。
          表格是 Top N，合计天然小于全库总数，差额 = 孤儿文章 + 未进 Top 的源。
        */}
        <p className="mt-2 text-[11px] leading-relaxed text-ink-faint">
          另有 <strong className="font-medium text-ink-muted">{data.orphanArticles.toLocaleString('zh-CN')}</strong>{' '}
          篇「无归属」文章 —— 它们所属的新闻源已被删除，因此既不计入上表任何一行，
          也不计入上面的「Top 源合计」。全库文章数请以概览为准。
        </p>
      </section>

      {/* ---------- 按分类 ---------- */}
      {byCategory.length > 0 ? (
        <section>
          <h2 className="text-[11px] font-semibold uppercase tracking-wide text-ink-faint">按分类</h2>
          <div className="mt-2 grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-4">
            {byCategory.map((c) => (
              <div key={c.category} className="ez-card flex items-baseline justify-between gap-2 p-3">
                <span className="truncate text-[13px] text-ink-muted">
                  {CATEGORY_LABELS[c.category as keyof typeof CATEGORY_LABELS] ?? c.category}
                </span>
                <span className="shrink-0 text-[15px] font-semibold tabular-nums text-ink">
                  {c.count.toLocaleString('zh-CN')}
                </span>
              </div>
            ))}
          </div>
        </section>
      ) : null}
    </div>
  );
}

/** 顶部小计数卡。 */
function MiniCard({ label, value, hint }: { label: string; value: string; hint?: string }): JSX.Element {
  return (
    <div className="ez-card p-3">
      <p className="text-[11px] text-ink-faint">{label}</p>
      <p className="mt-0.5 text-lg font-semibold tabular-nums text-ink">{value}</p>
      {hint ? <p className="mt-0.5 text-[11px] text-ink-faint">{hint}</p> : null}
    </div>
  );
}
