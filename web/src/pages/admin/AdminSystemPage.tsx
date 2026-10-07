/**
 * 后台「系统」分区（`/admin/system`）。
 *
 * 展示 `GET /api/v1/admin/system`（`AdminSystemDTO`）：进程与运行时、
 * SQLite 存储状况、各表行数。
 *
 * ★ 这里**只读，没有任何写操作**：契约里没有提供改配置的端点，
 *   早期稿中的 `ftsEnabled` / `searchEnabled` / `registrationEnabled` 三个开关
 *   在 final 契约里已不存在，因此本页不再渲染"开关"这种会诱导点击的形态，
 *   一律渲染成只读键值。
 *
 * ★ `dbBytes` 是**逻辑占用** = `pageSize × pageCount`，不是磁盘文件大小：
 *   SQLite 删数据后不会把空闲页归还文件系统，文件大小几乎不降，
 *   但逻辑占用会立刻下降。页面上必须写明这一点，否则用户 stat 一下磁盘
 *   就会认为接口报错。配套的 `freeRatio` 也因此才有意义。
 */

import { EmptyState, ErrorState, Skeleton } from '@/components/states';
import { AlertIcon } from '@/components/icons';
import { formatAbsoluteSec, formatBytes, formatUptime } from '@/lib/datetime';
import { useAdminSystem } from '@/hooks/useAdmin';
import { GroupTitle, PageHeader } from './AdminOverviewPage';

/** 空闲页占比超过这个值就提示执行 VACUUM。 */
const VACUUM_HINT_RATIO = 0.3;

export function AdminSystemPage(): JSX.Element {
  const query = useAdminSystem();

  if (query.isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-7 w-32" />
        <Skeleton className="h-40 w-full rounded-xl" />
        <Skeleton className="h-40 w-full rounded-xl" />
        <Skeleton className="h-56 w-full rounded-xl" />
        <span className="sr-only">正在加载系统信息…</span>
      </div>
    );
  }

  if (query.isError) {
    return <ErrorState error={query.error} onRetry={() => void query.refetch()} title="系统信息加载失败" />;
  }

  const data = query.data;
  if (!data) {
    return <EmptyState title="服务端未返回系统信息" description="请确认服务端版本支持 /api/v1/admin/system。" />;
  }

  const startedAt = formatAbsoluteSec(data.startedAt);
  const tableRows = Object.entries(data.tableCounts ?? {}).sort((a, b) => b[1] - a[1]);
  const freePct = Math.round((Number.isFinite(data.freeRatio) ? data.freeRatio : 0) * 100);
  const needVacuum = data.freeRatio > VACUUM_HINT_RATIO;

  return (
    <div className="space-y-6">
      <PageHeader title="系统" subtitle="进程、运行时与数据库存储状况（只读）" />

      <section>
        <GroupTitle>进程</GroupTitle>
        <div className="ez-card mt-2 divide-y divide-line">
          <Row label="服务版本" value={data.serverVersion || '未知'} mono />
          <Row label="Go 版本" value={data.goVersion || '未知'} mono />
          <Row label="运行平台" value={[data.goos, data.goarch].filter(Boolean).join(' / ') || '未知'} mono />
          <Row label="启动时间" value={startedAt || '未知'} />
          <Row label="运行时长" value={formatUptime(data.uptimeSeconds) || '未知'} />
          <Row label="Goroutine 数" value={data.numGoroutine.toLocaleString('zh-CN')} />
          <Row label="堆内存占用" value={`${formatNumber(data.heapAllocMb)} MB`} />
          <Row label="数据库 schema" value={String(data.schemaVersion)} mono />
        </div>
      </section>

      <section>
        <GroupTitle>数据库</GroupTitle>
        <div className="ez-card mt-2 divide-y divide-line">
          <Row label="逻辑占用" value={formatBytes(data.dbBytes)} />
          <Row label="journal 模式" value={data.journalMode || '未知'} mono />
          <Row label="页大小" value={`${data.pageSize.toLocaleString('zh-CN')} B`} mono />
          <Row label="页数" value={data.pageCount.toLocaleString('zh-CN')} mono />
          <Row label="空闲页数" value={data.freePages.toLocaleString('zh-CN')} />
          <Row label="空闲页占比" value={`${freePct}%`} />
        </div>

        {needVacuum ? (
          <div className="mt-3 flex items-start gap-2 rounded-lg border border-amber-200 bg-amber-50 px-3.5 py-2.5 text-[12px] leading-relaxed text-amber-900 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-200">
            <AlertIcon className="mt-px shrink-0 text-base" />
            <span>
              空闲页占比 {freePct}%，建议对数据库执行一次 <code className="font-mono">VACUUM</code>
              回收空闲页。这不是故障：SQLite 删除数据后不会自动把空间还给文件系统，
              需要停服执行或在业务低峰期执行。
            </span>
          </div>
        ) : null}

        <p className="mt-2 text-[11px] leading-relaxed text-ink-faint">
          「逻辑占用」= 页大小 × 页数，反映的是数据库实际用到多少空间；
          SQLite 并不会把删除数据后的空闲页归还磁盘，所以<b className="font-medium">磁盘上的文件大小通常明显大于这个值</b>
          ，两者不一致属于正常现象。
        </p>
      </section>

      <section>
        <GroupTitle>表行数</GroupTitle>
        {tableRows.length === 0 ? (
          <EmptyState title="服务端未返回表行数" description="tableCounts 为空。" />
        ) : (
          <div className="ez-card mt-2 divide-y divide-line">
            {tableRows.map(([table, count]) => (
              <Row key={table} label={TABLE_LABELS[table] ?? table} value={count.toLocaleString('zh-CN')} mono />
            ))}
          </div>
        )}
      </section>
    </div>
  );
}

/** 键值行（`mono` 用于表名/版本这类不该被中文字体重排的内容）。 */
function Row({ label, value, mono = false }: { label: string; value: string; mono?: boolean }): JSX.Element {
  return (
    <div className="flex items-center justify-between gap-4 px-3.5 py-2.5">
      <span className="text-[13px] text-ink-muted">{label}</span>
      <span className={`truncate text-[13px] tabular-nums text-ink ${mono ? 'font-mono text-[12px]' : ''}`}>
        {value}
      </span>
    </div>
  );
}

/** 带小数的数字统一保留一位，避免出现「堆内存 12.0000001 MB」。 */
function formatNumber(n: number): string {
  if (!Number.isFinite(n)) return '—';
  return Number.isInteger(n) ? String(n) : n.toFixed(1);
}

/**
 * `tableCounts` 的键 → 中文标签。
 *
 * ★ 只列能从服务端表名确定的；**没有列出的键直接显示原始表名**（等宽字体），
 *   绝不猜一个中文名上去 —— 猜错的标签比裸表名有害得多。
 */
const TABLE_LABELS: Record<string, string> = {
  article: '文章',
  user: '用户',
  session: '会话',
  source: '新闻源',
  audio: '音频',
  audio_task: '语音任务',
  user_favorite: '收藏',
  user_read: '已读状态',
  user_state: '用户状态',
  user_preference: '偏好设置',
};
