/**
 * 后台「语音任务」分区（`/admin/audio`）。
 *
 * ============================ ★ 默认带 status=failed ============================
 * 服务端排序是 **id DESC**（最新在前），不是"失败优先"。后台的用途是**排障**，
 * 所以进入时默认过滤 `failed`，并用品服务端给的 `statusCounts` 做筛选 chip ——
 * 效果等同于失败优先，同时还能正茬（在原生游标分页下）翻页。
 *
 * ============================ 错误展示 ============================
 * 契约里 `error` 已拆成两个字段：
 * - `errorMsg` —— 给人看的中文说明，**优先展示**它；
 * - `errorCode` —— 机器码，用小字灰色显示（方便贴给开发者/查日志）。
 * 只给其中一个时不要硬去补另一个。
 *
 * ★ 只有 `failed` 状态才有「重试」按钮：对 ready / processing 的任务重试没有意义
 *   （服务端要么幂等忽略、要么重复合成一笔钱），UI 上直接不给入口。
 */

import { useState } from 'react';
import type { AdminAudioTask, AudioTaskStatus } from '@/api/types';
import { EmptyState, ErrorState, InlineError, InlineSpinner, Skeleton } from '@/components/states';
import { RefreshIcon } from '@/components/icons';
import { formatBytes, formatRelativeSec } from '@/lib/datetime';
import { useAdminAudioTasks, useRetryAdminAudioTask } from '@/hooks/useAdmin';
import { PageHeader } from './AdminOverviewPage';

/** chip 顺序固定：pending → processing → ready → failed（失败放最后，但显眼）。 */
const STATUS_ORDER: ReadonlyArray<AudioTaskStatus> = ['pending', 'processing', 'ready', 'failed'];

const STATUS_LABELS: Record<AudioTaskStatus, string> = {
  pending: '待处理',
  processing: '合成中',
  ready: '已完成',
  failed: '失败',
};

const STATUS_STYLES: Record<AudioTaskStatus, string> = {
  pending: 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300',
  processing: 'bg-blue-50 text-blue-700 dark:bg-blue-950 dark:text-blue-300',
  ready: 'bg-emerald-50 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-300',
  failed: 'bg-rose-50 text-rose-700 dark:bg-rose-950 dark:text-rose-300',
};

export function AdminAudioTasksPage(): JSX.Element {
  // ★ 默认 failed：见文件头说明。
  const [status, setStatus] = useState<AudioTaskStatus | ''>('failed');
  const query = useAdminAudioTasks(status);
  const retry = useRetryAdminAudioTask();

  const pages = query.data?.pages ?? [];
  const items = pages.flatMap((p) => p.items);
  const lastPage = pages[pages.length - 1];
  const statusCounts = lastPage?.statusCounts ?? {};

  const totalFiltered = lastPage?.total ?? 0;

  return (
    <div className="space-y-5">
      <PageHeader title="语音任务" subtitle="TTS 合成队列：查失败原因、重试" />

      {/* ---------- 状态筛选 chip（计数来自服务端 statusCounts，不受当前筛选影响） ---------- */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex flex-wrap gap-1.5">
          <StatusChip active={status === ''} label="全部" count={sumAll(statusCounts)} onClick={() => setStatus('')} />
          {STATUS_ORDER.map((s) => (
            <StatusChip
              key={s}
              active={status === s}
              label={STATUS_LABELS[s]}
              count={statusCounts[s] ?? 0}
              tone={s === 'failed' ? 'danger' : undefined}
              onClick={() => setStatus(s)}
            />
          ))}
        </div>
        <button
          type="button"
          onClick={() => void query.refetch()}
          disabled={query.isFetching}
          className="ez-btn ez-btn-sm ez-btn-secondary ml-auto"
        >
          {query.isFetching ? <InlineSpinner /> : <RefreshIcon />}
          刷新
        </button>
      </div>

      {retry.isError ? <InlineError error={retry.error} /> : null}

      {query.isLoading ? (
        <div className="space-y-2">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="h-20 w-full rounded-xl" />
          ))}
          <span className="sr-only">正在加载语音任务…</span>
        </div>
      ) : query.isError ? (
        <ErrorState error={query.error} onRetry={() => void query.refetch()} title="语音任务加载失败" />
      ) : items.length === 0 ? (
        <EmptyState
          title={status === 'failed' ? '没有失败的合成任务' : '还没有语音合成任务'}
          description={
            status === 'failed'
              ? '当前筛选下没有失败任务。切到「全部」可以看到队列里其它状态的任务。'
              : '用户在文章上点播放后，服务端才会创建合成任务。'
          }
        />
      ) : (
        <>
          <p className="text-[12px] text-ink-faint">
            {STATUS_LABELS[status as AudioTaskStatus] ?? '全部'}任务共 {totalFiltered} 条，已加载 {items.length} 条
            {lastPage ? ` · 音频 ${lastPage.audioCount} 个 / ${formatBytes(lastPage.audioBytes)}` : ''}
          </p>

          <ul className="space-y-2">
            {items.map((t) => (
              <TaskRow key={t.id} task={t} onRetry={() => retry.mutate(t.id)} retrying={retry.isPending && retry.variables === t.id} />
            ))}
          </ul>

          {query.hasNextPage ? (
            <button
              type="button"
              onClick={() => void query.fetchNextPage()}
              disabled={query.isFetchingNextPage}
              className="ez-btn ez-btn-md ez-btn-secondary"
            >
              {query.isFetchingNextPage ? <InlineSpinner label="加载中…" /> : '加载更多'}
            </button>
          ) : (
            <p className="text-[12px] text-ink-faint">已到最后一页</p>
          )}
        </>
      )}
    </div>
  );
}

/** 全部状态计数之和（"全部"chip 用；服务端没有给这个和，自己加即可）。 */
function sumAll(counts: Record<string, number>): number {
  return Object.values(counts).reduce((s, n) => s + n, 0);
}

/** 状态筛选 chip。 */
function StatusChip({
  active,
  label,
  count,
  tone,
  onClick,
}: {
  active: boolean;
  label: string;
  count: number;
  tone?: 'danger';
  onClick: () => void;
}): JSX.Element {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={`rounded-lg px-3 py-1.5 text-[13px] font-medium transition-colors ${
        active
          ? 'bg-brand text-white'
          : tone === 'danger' && count > 0
            ? 'bg-surface-raised text-rose-600 hover:bg-surface-sunken dark:text-rose-400'
            : 'bg-surface-raised text-ink-muted hover:bg-surface-sunken hover:text-ink'
      }`}
    >
      {label}
      <span className={`ml-1 text-[11px] tabular-nums ${active ? 'opacity-75' : 'text-ink-faint'}`}>{count}</span>
    </button>
  );
}

/** 单个任务行。 */
function TaskRow({
  task,
  retrying,
  onRetry,
}: {
  task: AdminAudioTask;
  retrying: boolean;
  onRetry: () => void;
}): JSX.Element {
  const view = STATUS_STYLES[task.status] ?? STATUS_STYLES.pending;
  const label = STATUS_LABELS[task.status] ?? task.status;

  return (
    <li className="ez-card p-3.5">
      <div className="flex flex-wrap items-start gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className={`ez-badge ${view}`}>{label}</span>
            <span className="truncate text-[13px] font-medium text-ink">{task.articleTitle}</span>
          </div>
          <p className="mt-1 text-[11px] text-ink-faint">
            任务 #{task.id} · 文章 #{task.articleId} · 音色 {task.voice} · {task.speed}× · 重试 {task.retryCount} 次
            {task.provider ? ` · ${task.provider}` : ''}
            {task.audioId > 0 ? ` · 音频 #${task.audioId}` : ''}
          </p>
          <p className="mt-0.5 text-[11px] text-ink-faint">
            正文 {task.textChars} 字 · 创建 {formatRelativeSec(task.createdAt)} · 更新{' '}
            {formatRelativeSec(task.updatedAt)}
          </p>

          {/* ★ 错误：优先展示 errorMsg，errorCode 用小字灰色 */}
          {task.errorMsg || task.errorCode ? (
            <div className="mt-1.5 rounded-md bg-rose-50 px-2 py-1.5 dark:bg-rose-950/50">
              {task.errorMsg ? (
                <p className="break-words text-[12px] text-rose-700 dark:text-rose-300">{task.errorMsg}</p>
              ) : null}
              {task.errorCode ? (
                <p className="mt-0.5 break-all font-mono text-[11px] text-rose-500/80 dark:text-rose-400/70">
                  {task.errorCode}
                </p>
              ) : null}
            </div>
          ) : null}
        </div>

        {task.status === 'failed' ? (
          <button
            type="button"
            onClick={onRetry}
            disabled={retrying}
            className="ez-btn ez-btn-sm ez-btn-secondary shrink-0"
            title="重新提交该任务（重置状态并重新入队）"
          >
            {retrying ? <InlineSpinner label="重试中…" /> : '重试'}
          </button>
        ) : null}
      </div>
    </li>
  );
}
