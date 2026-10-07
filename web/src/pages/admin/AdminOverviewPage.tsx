/**
 * 后台概览（`/admin`）。
 *
 * ★ 数据是**扁平**的（`AdminOverviewDTO`，没有 counts/storage/queue 嵌套），
 *   这里照抄扁平结构渲染，不做二次分组再取别名 —— 多一层名字就多一处漂移。
 *
 * 三句话定位：**有多少东西**（规模）、**队列健康吗**（失败/待处理）、**服务端是谁**（版本/运行时）。
 * 队列区是唯一带跳转的分组：`failedAudioTasks > 0` 时给一个去「语音任务」的入口，
 * 否则运维者看到"失败 3"却不知道下一步点哪。
 *
 * ★ `admins` 与用户列表里的 `admins` **不是一个口径**：
 *   这里是 `COUNT(*) WHERE role='admin'`（**含已停用**），
 *   想知道"还能用的管理员有几个"要看用户分区那个（只数未停用）。
 *   概览页因此把"已停用用户"和它并排放，并在标题里标注口径。
 */

import { Link } from 'react-router-dom';
import { EmptyState, ErrorState, Skeleton } from '@/components/states';
import { AlertIcon } from '@/components/icons';
import { formatRelativeSec, formatUptime } from '@/lib/datetime';
import { useAdminOverview } from '@/hooks/useAdmin';

export function AdminOverviewPage(): JSX.Element {
  const query = useAdminOverview();

  if (query.isLoading) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-7 w-32" />
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          {[0, 1, 2, 3, 4, 5, 6].map((i) => (
            <Skeleton key={i} className="h-24 w-full rounded-xl" />
          ))}
        </div>
        <span className="sr-only">正在加载概览…</span>
      </div>
    );
  }

  if (query.isError) {
    return <ErrorState error={query.error} onRetry={() => void query.refetch()} title="概览加载失败" />;
  }

  const data = query.data;
  if (!data) {
    return <EmptyState title="服务端未返回概览数据" description="请确认服务端已启用管理后台接口。" />;
  }

  const failed = data.failedAudioTasks;
  const statusEntries = Object.entries(data.audioTaskStatus ?? {});

  return (
    <div className="space-y-6">
      <PageHeader title="概览" subtitle="全站规模、语音队列与服务运行状态" />

      <section>
        <GroupTitle>规模</GroupTitle>
        <div className="mt-2 grid grid-cols-2 gap-3 sm:grid-cols-4">
          <StatCard label="用户" value={data.users} hint={`其中 ${data.disabledUsers} 个已停用`} />
          <StatCard
            label="管理员"
            value={data.admins}
            hint="按角色统计，含已停用"
          />
          <StatCard label="活跃会话" value={data.activeSessions} hint="未过期的登录会话" />
          <StatCard label="文章" value={data.articles} hint={`近 24 小时新增 ${data.articlesLast24h}`} />
          <StatCard label="新闻源" value={data.sources} />
          <StatCard label="音频" value={data.audios} hint="已合成的音频文件" />
        </div>
      </section>

      <section>
        <GroupTitle>语音合成队列</GroupTitle>
        <div className="mt-2 grid grid-cols-2 gap-3 sm:grid-cols-4">
          <StatCard label="待处理" value={data.pendingAudioTasks} />
          <StatCard label="失败" value={failed} tone={failed > 0 ? 'warn' : undefined} />
          {statusEntries.length > 0 ? (
            <div className="ez-card p-3.5 sm:col-span-2">
              <p className="text-[11px] text-ink-faint">按状态分布</p>
              <div className="mt-1.5 flex flex-wrap gap-1.5">
                {statusEntries.map(([status, n]) => (
                  <span key={status} className="ez-badge bg-surface-sunken text-ink-muted">
                    {TASK_STATUS_LABELS[status] ?? status}
                    <span className="tabular-nums">{n}</span>
                  </span>
                ))}
              </div>
            </div>
          ) : null}
        </div>

        {failed > 0 ? (
          <Link
            to="/admin/audio"
            className="mt-3 flex items-center gap-2 rounded-lg border border-amber-200 bg-amber-50 px-3.5 py-2.5 text-[12px] text-amber-900 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-200"
          >
            <AlertIcon className="shrink-0 text-base" />
            有 {failed} 个任务失败，去「语音任务」查看原因或重试
          </Link>
        ) : null}
      </section>

      <section>
        <GroupTitle>服务端</GroupTitle>
        <div className="ez-card mt-2 divide-y divide-line">
          <InfoRow label="服务版本" value={data.serverVersion || '未知'} mono />
          <InfoRow label="Go 版本" value={data.goVersion || '未知'} mono />
          <InfoRow label="运行时长" value={formatUptime(data.uptimeSeconds)} />
          <InfoRow label="数据库 schema" value={String(data.schemaVersion)} mono />
          <InfoRow label="数据生成时间" value={formatRelativeSec(data.generatedAt) || '未知'} />
        </div>
      </section>
    </div>
  );
}

/** contract `audioTaskStatus` 的键是英文状态枚举。 */
const TASK_STATUS_LABELS: Record<string, string> = {
  pending: '待处理',
  processing: '合成中',
  ready: '已完成',
  failed: '失败',
};

/** 键值行。 */
function InfoRow({ label, value, mono = false }: { label: string; value: string; mono?: boolean }): JSX.Element {
  return (
    <div className="flex items-center justify-between gap-4 px-3.5 py-2.5">
      <span className="text-[13px] text-ink-muted">{label}</span>
      <span className={`truncate text-[13px] text-ink ${mono ? 'font-mono text-[12px]' : ''}`}>{value}</span>
    </div>
  );
}

/* ========================================================================== *
 * 后台内通用的小组件（各分区共用，保证视觉一致）
 * ========================================================================== */

/** 分区页头。 */
export function PageHeader({ title, subtitle }: { title: string; subtitle?: string }): JSX.Element {
  return (
    <div>
      <h1 className="text-lg font-semibold tracking-tight text-ink">{title}</h1>
      {subtitle ? <p className="mt-0.5 text-[12px] text-ink-muted">{subtitle}</p> : null}
    </div>
  );
}

/** 分组小标题。 */
export function GroupTitle({ children }: { children: string }): JSX.Element {
  return <h2 className="text-[11px] font-semibold uppercase tracking-wide text-ink-faint">{children}</h2>;
}

/** 单个统计卡片。 */
export function StatCard({
  label,
  value,
  hint,
  raw = false,
  tone,
}: {
  label: string;
  value: number | string;
  hint?: string;
  /** true = value 已是格式化好的字符串（如 "1.2 MB"），不再做千分位处理。 */
  raw?: boolean;
  tone?: 'warn';
}): JSX.Element {
  return (
    <div className="ez-card p-3.5">
      <p className="text-[11px] text-ink-faint">{label}</p>
      <p
        className={`mt-1 text-xl font-semibold tabular-nums ${
          tone === 'warn' ? 'text-amber-600 dark:text-amber-400' : 'text-ink'
        }`}
      >
        {raw ? value : formatCount(value)}
      </p>
      {hint ? <p className="mt-0.5 text-[11px] text-ink-faint">{hint}</p> : null}
    </div>
  );
}

/** 数字千分位（后台数字大，不加分隔符很难一眼读出量级）。 */
function formatCount(value: number | string): string {
  const n = typeof value === 'number' ? value : Number(value);
  if (!Number.isFinite(n)) return String(value);
  return n.toLocaleString('zh-CN');
}
