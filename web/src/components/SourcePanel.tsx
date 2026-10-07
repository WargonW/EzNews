/**
 * 「管理新闻来源」面板（抽屉）。
 *
 * PRD §8.2：桌面为右侧抽屉，<768px 为**全屏模态**。
 * 功能（F-SRC-07）：默认源开关、自定义源增删改、添加表单。
 *
 * ============================ 契约要点 ============================
 * - `PATCH /api/v1/sources/{id}/enabled` 是**唯一的启停通道**（默认源也只能走它）。
 * - `PUT /api/v1/sources/{id}` 编辑：openapi 明确"默认源仅允许修改 enabled"，
 *   所以默认源的编辑按钮会禁用并说明原因（而不是让用户提交后吃 400）。
 * - `DELETE /api/v1/sources/{id}` 删除：默认源不可删（服务端返回 409）。
 *   UI 层面直接不显示默认源的删除按钮 —— 提前阻止无意义的失败请求。
 */

import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '@/api/client';
import { createSource, deleteSource, listSources, setSourceEnabled, updateSource } from '@/api/endpoints';
import type { Category, SourceDTO, SourceInput, SourceType } from '@/api/types';
import { CATEGORY_LABELS, CATEGORY_ORDER, SOURCE_TYPE_LABELS } from '@/lib/constants';
import { sourceKeys } from '@/lib/queryKeys';
import { InlineError, InlineSpinner } from './states';
import { SourceTypeBadge } from './Sidebar';
import { CheckIcon, CloseIcon, EditIcon, PlusIcon, TrashIcon } from './icons';

export interface SourcePanelProps {
  open: boolean;
  onClose: () => void;
  /**
   * 内嵌模式：后台「新闻源」分区复用本面板时传 true。
   *
   * 差别只有**外框**，业务逻辑（增删改 / 启停 / 默认源不可删）完全共用：
   * - 不渲染遮罩、不 fixed 定位、不锁 body 滚动、不响应 Esc；
   * - 不渲染"管理新闻来源"标题栏（分区页已有自己的标题）。
   *
   * 为什么用开关而不是复制一份：源管理的三处服务端约束（默认源仅可改 enabled、
   * 默认源不可删、URL 形态校验）一旦复制就会出现"后台改了阅读端没改"的漂移。
   */
  embedded?: boolean;
}

/** 表单初始值。 */
const EMPTY_FORM: SourceInput = {
  name: '',
  url: '',
  type: 'rss',
  category: 'tech',
  suggestInterval: 1800,
  remark: null,
  enabled: true,
};

export function SourcePanel({ open, onClose, embedded = false }: SourcePanelProps): JSX.Element | null {
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<SourceDTO | null>(null);
  const [form, setForm] = useState<SourceInput>(EMPTY_FORM);
  const [formError, setFormError] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<number | null>(null);

  const sourcesQuery = useQuery({
    queryKey: sourceKeys.list(true),
    queryFn: () => listSources(true),
    staleTime: 60_000,
    enabled: open || embedded,
  });

  /** 所有与源相关的缓存都要失效：文章列表的"包含已停用源"结果也会变。 */
  const invalidateAll = (): void => {
    void queryClient.invalidateQueries({ queryKey: sourceKeys.all });
    void queryClient.invalidateQueries({ queryKey: ['articles'] });
    void queryClient.invalidateQueries({ queryKey: ['categories'] });
  };

  const enableMutation = useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) => setSourceEnabled(id, enabled),
    onSuccess: invalidateAll,
  });

  const saveMutation = useMutation({
    mutationFn: (input: SourceInput) =>
      editing ? updateSource(editing.id, input) : createSource(input),
    onSuccess: () => {
      invalidateAll();
      resetForm();
    },
  });

  const deleteMutation = useMutation({
    mutationFn: (id: number) => deleteSource(id),
    onSuccess: () => {
      invalidateAll();
      setConfirmDelete(null);
    },
  });

  const resetForm = (): void => {
    setEditing(null);
    setForm(EMPTY_FORM);
    setFormError(null);
  };

  // 面板关闭时清空编辑状态，避免下次打开残留上一条。
  useEffect(() => {
    if (!open) resetForm();
  }, [open]);

  // Esc 关闭 + 移动端锁滚动（内嵌模式下两者都不需要：它没有遮罩，也不独占屏幕）。
  useEffect(() => {
    if (!open || embedded) return;
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.removeEventListener('keydown', onKey);
      document.body.style.overflow = prev;
    };
  }, [open, onClose, embedded]);

  const { defaults, customs } = useMemo(() => {
    const items = sourcesQuery.data?.items ?? [];
    return {
      defaults: items.filter((s) => s.isDefault),
      customs: items.filter((s) => !s.isDefault),
    };
  }, [sourcesQuery.data]);

  if (!open && !embedded) return null;

  /** 表单提交校验。 */
  const submitForm = (e: React.FormEvent): void => {
    e.preventDefault();
    if (saveMutation.isPending) return;

    const name = form.name.trim();
    const url = form.url.trim();

    if (!name) return setFormError('请填写来源名称');
    if (!url) return setFormError('请填写源地址');
    // 轻量 URL 校验：只挡明显错误形态，不做过度严格（自托管场景 URL 形态很多）。
    if (!/^https?:\/\/.+/i.test(url)) return setFormError('地址需以 http:// 或 https:// 开头');

    if (form.suggestInterval !== undefined && form.suggestInterval < 60) {
      return setFormError('建议采集间隔不能小于 60 秒');
    }

    saveMutation.mutate({ ...form, name, url });
  };

  /** 渲染一个源行。 */
  const renderSource = (s: SourceDTO): JSX.Element => {
    const toggling = enableMutation.isPending && enableMutation.variables?.id === s.id;
    const deleting = deleteMutation.isPending && deleteMutation.variables === s.id;

    return (
      <li key={s.id} className="ez-card p-3">
        <div className="flex items-start gap-3">
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-1.5">
              <span className="truncate text-[13px] font-medium text-ink">{s.name}</span>
              <SourceTypeBadge type={s.type} />
              {s.isDefault ? (
                <span className="ez-badge bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300">
                  系统预置
                </span>
              ) : null}
            </div>
            <p className="mt-1 truncate font-mono text-[11px] text-ink-faint" title={s.url}>
              {s.url}
            </p>
            {s.remark ? <p className="mt-1 text-[11px] text-ink-muted">{s.remark}</p> : null}
            <p className="mt-1 text-[11px] text-ink-faint">
              {CATEGORY_LABELS[s.category] ?? s.category}
              {s.articleCount !== undefined && s.articleCount > 0 ? ` · ${s.articleCount} 篇文章` : ''}
              {s.suggestInterval > 0 ? ` · 建议每 ${formatInterval(s.suggestInterval)}` : ''}
            </p>
          </div>

          <div className="flex shrink-0 items-center gap-1">
            {/* 启停：默认源与自定义源共用同一端点 */}
            <button
              type="button"
              role="switch"
              aria-checked={s.enabled}
              disabled={toggling}
              onClick={() => enableMutation.mutate({ id: s.id, enabled: !s.enabled })}
              title={s.enabled ? '停用该源' : '启用该源'}
              className={`relative h-5 w-9 shrink-0 rounded-full transition-colors disabled:opacity-50 ${
                s.enabled ? 'bg-brand' : 'bg-slate-300 dark:bg-slate-700'
              }`}
            >
              <span
                className={`absolute top-0.5 h-4 w-4 rounded-full bg-white shadow transition-transform ${
                  s.enabled ? 'translate-x-[1.15rem]' : 'translate-x-0.5'
                }`}
              />
              <span className="sr-only">{s.enabled ? '已启用' : '已停用'}</span>
            </button>

            {/* 默认源不可编辑（openapi：仅允许改 enabled） */}
            {s.isDefault ? (
              <span
                className="ez-btn ez-btn-sm ez-btn-ghost h-8 w-8 cursor-not-allowed p-0 text-ink-faint opacity-40"
                title="系统预置源不可编辑，可停用"
              >
                <EditIcon className="text-sm" />
              </span>
            ) : (
              <button
                type="button"
                onClick={() => {
                  setEditing(s);
                  setForm({
                    name: s.name,
                    url: s.url,
                    type: s.type,
                    category: s.category,
                    suggestInterval: s.suggestInterval,
                    iconUrl: s.iconUrl,
                    language: s.language,
                    remark: s.remark,
                    enabled: s.enabled,
                  });
                  setFormError(null);
                }}
                className="ez-btn ez-btn-sm ez-btn-ghost h-8 w-8 p-0"
                title="编辑"
              >
                <EditIcon className="text-sm" />
              </button>
            )}

            {/* 默认源不可删除 → 直接不显示（避免无意义的 409） */}
            {!s.isDefault ? (
              confirmDelete === s.id ? (
                <span className="flex items-center gap-1">
                  <button
                    type="button"
                    onClick={() => deleteMutation.mutate(s.id)}
                    disabled={deleting}
                    className="ez-btn ez-btn-sm ez-btn-danger h-8 px-2"
                  >
                    {deleting ? <InlineSpinner /> : '确认'}
                  </button>
                  <button
                    type="button"
                    onClick={() => setConfirmDelete(null)}
                    className="ez-btn ez-btn-sm ez-btn-ghost h-8 px-2"
                  >
                    取消
                  </button>
                </span>
              ) : (
                <button
                  type="button"
                  onClick={() => setConfirmDelete(s.id)}
                  className="ez-btn ez-btn-sm ez-btn-ghost h-8 w-8 p-0 hover:text-rose-600"
                  title="删除"
                >
                  <TrashIcon className="text-sm" />
                </button>
              )
            ) : null}
          </div>
        </div>
      </li>
    );
  };

  return (
    <div
      className={
        embedded
          ? 'flex justify-end'
          : 'fixed inset-0 z-40 flex justify-end bg-slate-900/40 backdrop-blur-sm animate-fade-in'
      }
      onMouseDown={(e) => {
        if (!embedded && e.target === e.currentTarget) onClose();
      }}
    >
      <aside
        role={embedded ? 'region' : 'dialog'}
        aria-modal={embedded ? undefined : true}
        aria-labelledby={embedded ? undefined : 'source-panel-title'}
        className={
          embedded
            ? 'flex w-full flex-col overflow-hidden rounded-xl border border-line bg-surface-raised'
            : 'flex h-full w-full flex-col border-l border-line bg-surface shadow-pop sm:w-[26rem]'
        }
      >
        {embedded ? null : (
        <header className="flex h-14 shrink-0 items-center justify-between border-b border-line px-4">
          <h2 id="source-panel-title" className="text-[15px] font-semibold text-ink">
            管理新闻来源
          </h2>
          <button
            type="button"
            onClick={onClose}
            className="ez-btn ez-btn-sm ez-btn-ghost h-8 w-8 p-0"
            aria-label="关闭"
          >
            <CloseIcon className="text-base" />
          </button>
        </header>
        )}

        <div className="min-h-0 flex-1 overflow-y-auto p-4">
          {sourcesQuery.isError ? (
            <InlineError error={sourcesQuery.error} onRetry={() => void sourcesQuery.refetch()} />
          ) : null}

          {/* ---------- 添加 / 编辑表单 ---------- */}
          <form onSubmit={submitForm} className="ez-card mb-5 p-3.5">
            <div className="mb-2.5 flex items-center justify-between">
              <h3 className="text-[13px] font-semibold text-ink">
                {editing ? `编辑：${editing.name}` : '添加自定义来源'}
              </h3>
              {editing ? (
                <button type="button" onClick={resetForm} className="ez-btn ez-btn-sm ez-btn-ghost">
                  取消编辑
                </button>
              ) : null}
            </div>

            <div className="space-y-3">
              <div>
                <label className="ez-label" htmlFor="src-name">
                  来源名称
                </label>
                <input
                  id="src-name"
                  className="ez-input"
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  maxLength={128}
                  placeholder="例如：少数派"
                  required
                />
              </div>

              <div>
                <label className="ez-label" htmlFor="src-url">
                  源地址
                </label>
                <input
                  id="src-url"
                  type="url"
                  className="ez-input font-mono text-[12px]"
                  value={form.url}
                  onChange={(e) => setForm({ ...form, url: e.target.value })}
                  maxLength={2048}
                  placeholder="https://example.com/feed.xml"
                  required
                />
              </div>

              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="ez-label" htmlFor="src-type">
                    类型
                  </label>
                  <select
                    id="src-type"
                    className="ez-input"
                    value={form.type}
                    onChange={(e) => setForm({ ...form, type: e.target.value as SourceType })}
                  >
                    {(Object.keys(SOURCE_TYPE_LABELS) as SourceType[]).map((t) => (
                      <option key={t} value={t}>
                        {SOURCE_TYPE_LABELS[t]}
                      </option>
                    ))}
                  </select>
                </div>
                <div>
                  <label className="ez-label" htmlFor="src-cat">
                    分类
                  </label>
                  <select
                    id="src-cat"
                    className="ez-input"
                    value={form.category}
                    onChange={(e) => setForm({ ...form, category: e.target.value as Category })}
                  >
                    {CATEGORY_ORDER.map((c) => (
                      <option key={c} value={c}>
                        {CATEGORY_LABELS[c]}
                      </option>
                    ))}
                  </select>
                </div>
              </div>

              <div>
                <label className="ez-label" htmlFor="src-interval">
                  建议采集间隔（秒）
                </label>
                <input
                  id="src-interval"
                  type="number"
                  min={60}
                  step={60}
                  className="ez-input"
                  value={form.suggestInterval ?? 1800}
                  onChange={(e) => setForm({ ...form, suggestInterval: Number(e.target.value) || 1800 })}
                />
                <p className="mt-1 text-[10px] text-ink-faint">
                  仅供外部采集器参考，服务端自身不采集。
                </p>
              </div>

              <div>
                <label className="ez-label" htmlFor="src-remark">
                  备注<span className="ml-1 font-normal text-ink-faint">（选填）</span>
                </label>
                <input
                  id="src-remark"
                  className="ez-input"
                  value={form.remark ?? ''}
                  onChange={(e) => setForm({ ...form, remark: e.target.value || null })}
                  maxLength={500}
                />
              </div>
            </div>

            {formError ? (
              <p className="mt-2.5 text-[12px] text-rose-600 dark:text-rose-400" role="alert">
                {formError}
              </p>
            ) : null}
            {saveMutation.isError ? (
              <p className="mt-2.5 text-[12px] text-rose-600 dark:text-rose-400" role="alert">
                {saveMutation.error instanceof ApiError
                  ? saveMutation.error.message
                  : '保存失败，请检查网络'}
              </p>
            ) : null}

            <button
              type="submit"
              disabled={saveMutation.isPending}
              className="ez-btn ez-btn-md ez-btn-primary mt-3.5 w-full"
            >
              {saveMutation.isPending ? (
                <InlineSpinner label="保存中…" />
              ) : editing ? (
                <>
                  <CheckIcon />
                  保存修改
                </>
              ) : (
                <>
                  <PlusIcon />
                  添加来源
                </>
              )}
            </button>
          </form>

          {/* ---------- 默认来源 ---------- */}
          <section className="mb-5">
            <h3 className="mb-2 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
              默认来源（系统预置）
            </h3>
            {sourcesQuery.isLoading ? (
              <div className="space-y-2">
                {[0, 1, 2].map((i) => (
                  <div key={i} className="ez-skeleton h-16 w-full rounded-xl" />
                ))}
              </div>
            ) : defaults.length > 0 ? (
              <ul className="space-y-2">{defaults.map(renderSource)}</ul>
            ) : (
              <p className="text-[12px] text-ink-faint">服务端尚未预置任何默认源。</p>
            )}
          </section>

          {/* ---------- 自定义来源 ---------- */}
          <section>
            <h3 className="mb-2 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
              自定义来源
            </h3>
            {customs.length > 0 ? (
              <ul className="space-y-2">{customs.map(renderSource)}</ul>
            ) : (
              <p className="text-[12px] leading-relaxed text-ink-faint">
                还没有自定义源。用上方表单添加一个 RSS / Atom 地址。
              </p>
            )}
          </section>
        </div>
      </aside>
    </div>
  );
}

/** 秒 → 人类可读间隔。 */
function formatInterval(sec: number): string {
  if (sec < 60) return `${sec} 秒`;
  if (sec < 3600) return `${Math.round(sec / 60)} 分钟`;
  if (sec < 86400) return `${Math.round(sec / 3600)} 小时`;
  return `${Math.round(sec / 86400)} 天`;
}