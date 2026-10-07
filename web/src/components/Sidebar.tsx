/**
 * 侧栏：新闻源筛选（+ 分类入口）。
 *
 * PRD §8.1 布局：桌面常驻，768px 以下收折为抽屉。
 * 分组：全部新闻 → 默认来源 → 我的来源（自定义）→ 添加自定义源入口。
 *
 * **每行只有一个语义：点 = 看这个源**（点击即把右侧列表整体切换为该源，
 * 再点同一地址则退回「全部新闻」）。侧栏不再承担"在本列表里隐藏某源"的职责 ——
 * 那与"点 = 看"恰好相反（一个加法、一个减法），同一行两个方向相反的控件
 * 必然让用户混淆。**「隐藏源」现已移到首页「全部新闻」视图下的高级筛选区**，
 * 那里才是它唯一合理的场景（全量流里屏蔽不想看的源），
 * 且与服务端 `PATCH /sources/{id}/enabled` 的真·启停彻底分开。
 */

import { useMemo } from 'react';
import type { SourceDTO } from '@/api/types';
import { SOURCE_TYPE_LABELS } from '@/lib/constants';
import { EmptyState, SidebarSkeleton } from './states';
import { PlusIcon } from './icons';

export interface SidebarProps {
  sources: SourceDTO[] | undefined;
  loading: boolean;
  error: unknown;
  /** 当前按源筛选的 id；null = 全部。 */
  activeSourceId: number | null;
  onSelectSource: (id: number | null) => void;
  /** 打开管理来源面板。 */
  onOpenManage: () => void;
  /** 各源文章数（来自 categories 或源列表 articleCount）。 */
  counts?: ReadonlyMap<number, number>;
}

export function Sidebar({
  sources,
  loading,
  error,
  activeSourceId,
  onSelectSource,
  onOpenManage,
  counts,
}: SidebarProps): JSX.Element {
  const { defaults, customs, enabledCount, totalCount } = useMemo(() => {
    const all = sources ?? [];
    const d = all.filter((s) => s.isDefault);
    const c = all.filter((s) => !s.isDefault);
    return {
      defaults: d,
      customs: c,
      enabledCount: all.filter((s) => s.enabled).length,
      totalCount: all.length,
    };
  }, [sources]);

  if (loading && !sources) return <SidebarSkeleton />;

  if (error && !sources) {
    return (
      <div className="p-4">
        <EmptyState
          compact
          title="源列表加载失败"
          description="请检查服务端是否已启动。"
        />
      </div>
    );
  }

  const renderItem = (s: SourceDTO): JSX.Element => {
    // 删除线只表示**服务端真实停用**（enabled=false，来自「管理来源」面板），
    // 不再掺入本地的"隐藏源"筛选 —— 隐藏影响的是"这篇要不要显示"，
    // 而不是"这个源是不是停用了"，把两者混进同一个视觉信号会让用户
    // 在隐藏一个源后误以为它被停用。
    const enabled = s.enabled;
    const count = counts?.get(s.id);
    const active = activeSourceId === s.id;

    return (
      <li key={s.id}>
        <button
          type="button"
          onClick={() => onSelectSource(active ? null : s.id)}
          className={`flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left transition-colors ${
            active ? 'bg-brand-soft/60 dark:bg-slate-800' : 'hover:bg-surface-sunken'
          }`}
          aria-current={active ? 'true' : undefined}
        >
          <span className={`min-w-0 flex-1 truncate text-[13px] ${enabled ? 'text-ink' : 'text-ink-faint line-through'}`}>
            {s.name}
          </span>
          {count !== undefined && count > 0 ? (
            <span className="shrink-0 text-[11px] tabular-nums text-ink-faint">{count}</span>
          ) : null}
          {active ? <span className="h-1.5 w-1.5 shrink-0 rounded-full bg-brand" aria-hidden="true" /> : null}
        </button>
      </li>
    );
  };

  return (
    <nav className="flex h-full flex-col overflow-y-auto" aria-label="新闻源筛选">
      <div className="flex-1 space-y-5 p-3">
        {/* 全部新闻 */}
        <div>
          <button
            type="button"
            onClick={() => onSelectSource(null)}
            className={`flex w-full items-center gap-2 rounded-lg px-2 py-2 text-left transition-colors ${
              activeSourceId === null
                ? 'bg-brand-soft/60 font-medium text-brand dark:bg-slate-800 dark:text-blue-200'
                : 'text-ink hover:bg-surface-sunken'
            }`}
            aria-current={activeSourceId === null ? 'true' : undefined}
          >
            <span className="flex-1 text-[13px]">全部新闻</span>
            {totalCount > 0 ? (
              <span className="text-[11px] tabular-nums text-ink-faint">{enabledCount}</span>
            ) : null}
          </button>
        </div>

        {defaults.length > 0 ? (
          <section>
            <h2 className="mb-1 px-2 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
              默认来源
            </h2>
            <ul className="space-y-0.5">{defaults.map(renderItem)}</ul>
          </section>
        ) : null}

        <section>
          <h2 className="mb-1 px-2 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
            我的来源
          </h2>
          {customs.length > 0 ? (
            <ul className="space-y-0.5">{customs.map(renderItem)}</ul>
          ) : (
            <p className="px-2 py-1.5 text-[12px] leading-relaxed text-ink-faint">
              还没有自定义源。添加 RSS / Atom 地址即可。
            </p>
          )}
        </section>
      </div>

      {/* 添加自定义源：<768px 改为全屏模态入口，≥1024px 常驻 */}
      <div className="sticky bottom-0 border-t border-line bg-surface-raised p-3">
        <button type="button" onClick={onOpenManage} className="ez-btn ez-btn-md ez-btn-secondary w-full">
          <PlusIcon />
          添加自定义源
        </button>
        <p className="mt-2 px-1 text-[10px] leading-relaxed text-ink-faint">
          停用源请到管理面板；临时不想看某个源，用「全部新闻」页的高级筛选。
        </p>
      </div>
    </nav>
  );
}

/** 源类型小徽标（管理面板用）。 */
export function SourceTypeBadge({ type }: { type: SourceDTO['type'] }): JSX.Element {
  return (
    <span className="ez-badge bg-surface-sunken text-ink-muted">
      {SOURCE_TYPE_LABELS[type] ?? type}
    </span>
  );
}