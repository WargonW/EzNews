/**
 * `Sidebar` 的来源选择回调契约（本次首页改造的依赖面）。
 *
 * ============================ 为什么单独测 Sidebar ============================
 * 首页改造把"点来源 = 独占切换"的**参数清理**放在 `FeedPage` 的 `onSelectSource`
 * 里，但触发这一切的语义契约在 Sidebar 侧：
 * - 点**未选中**的源 → 回调收到该源 id（=选中它）；
 * - 点**已选中**的源 → 回调收到 `null`（Sidebar 自己实现的"再点一下取消"）；
 * - 点「全部新闻」 → 回调收到 `null`（=回到全量流）。
 *
 * 这三条如果漂移（例如把 toggle 写成恒传 id），页面上表现为"点已选中的源
 * 没有任何反应"或"永远退不回全量"，而参数清理逻辑本身却是对的 —— 属于
 * "分层各自都对、组合起来错"的典型。这里用一个纯回调把契约钉死。
 *
 * 集成层面（URL 实际怎么变）由 FeedPage.source-switch.test.tsx 覆盖。
 */

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { SourceDTO } from '@/api/types';
import { Sidebar } from '@/components/Sidebar';

function makeSource(id: number, name: string, over: Partial<SourceDTO> = {}): SourceDTO {
  return {
    id,
    key: `src-${id}`,
    name,
    url: `https://example.com/${id}.xml`,
    type: 'rss',
    category: 'other',
    isDefault: true,
    enabled: true,
    suggestInterval: 1800,
    iconUrl: null,
    language: null,
    remark: null,
    articleCount: 2,
    createdAt: '2023-11-14T22:13:20Z',
    updatedAt: '2023-11-14T22:13:20Z',
    ...over,
  };
}

const SOURCES = [makeSource(1, 'IT 之家'), makeSource(2, '少数派')];

function renderSidebar(activeSourceId: number | null, onSelectSource = vi.fn()) {
  render(
    <Sidebar
      sources={SOURCES}
      loading={false}
      error={null}
      activeSourceId={activeSourceId}
      onSelectSource={onSelectSource}
      onOpenManage={() => undefined}
      counts={new Map<number, number>()}
    />,
  );
  return onSelectSource;
}

function nav(): HTMLElement {
  return screen.getByRole('navigation', { name: '新闻源筛选' });
}

/** 侧栏内某个源名的选择按钮（每行只有一个按钮 = 点它即看该源）。 */
function sourceSelectButton(name: string): HTMLElement {
  const span = within(nav()).getByText(name, { selector: 'span' });
  const btn = span.closest('button');
  if (!btn) throw new Error(`未找到源 ${name} 的选择按钮`);
  return btn as HTMLElement;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('Sidebar 来源选择回调', () => {
  it('点未选中的源 → 回调收到该源 id', () => {
    const cb = renderSidebar(null);
    fireEvent.click(sourceSelectButton('IT 之家'));
    expect(cb).toHaveBeenCalledWith(1);
  });

  it('点已选中的源（再点一下）→ 回调收到 null（取消选中）', () => {
    const cb = renderSidebar(1);
    fireEvent.click(sourceSelectButton('IT 之家'));
    expect(cb).toHaveBeenCalledWith(null);
  });

  it('「全部新闻」→ 回调收到 null（回到全量流）', () => {
    const cb = renderSidebar(2);
    fireEvent.click(within(nav()).getByRole('button', { name: /全部新闻/ }));
    expect(cb).toHaveBeenCalledWith(null);
  });

  it('选中态通过 aria-current 暴露给辅助技术（视觉高亮之外的等价信号）', () => {
    renderSidebar(1);
    expect(sourceSelectButton('IT 之家').getAttribute('aria-current')).toBe('true');
    expect(sourceSelectButton('少数派').getAttribute('aria-current')).toBeNull();
    // 未选中任何源时，「全部新闻」即当前项。
    cleanup();
    renderSidebar(null);
    expect(within(nav()).getByRole('button', { name: /全部新闻/ }).getAttribute('aria-current')).toBe('true');
  });

  it('每行只有「选择该源」一个按钮：不再有隐藏/勾选框（避免行内加减语义冲突）', () => {
    renderSidebar(null);
    // 每个源行内只应有一个 button（=选择按钮），不存在第二个勾选控件。
    expect(within(nav()).queryByRole('button', { name: /隐藏/ })).toBeNull();
    expect(within(nav()).queryByRole('button', { name: /显示/ })).toBeNull();
    // 源名所在行内确实只有一个 button。
    const span = within(nav()).getByText('IT 之家', { selector: 'span' });
    const row = span.closest('li');
    expect(row).not.toBeNull();
    expect(within(row as HTMLElement).getAllByRole('button')).toHaveLength(1);
  });

  it('源名删除线只依据服务端 enabled，不再受本地隐藏影响', () => {
    // 单独渲染一个 enabled=false 的源：应加删除线。
    cleanup();
    render(
      <Sidebar
        sources={[makeSource(1, '停用源', { enabled: false }), makeSource(2, '启用源')]}
        loading={false}
        error={null}
        activeSourceId={null}
        onSelectSource={vi.fn()}
        onOpenManage={() => undefined}
      />,
    );
    const disabled = within(nav()).getByText('停用源', { selector: 'span' });
    const enabled = within(nav()).getByText('启用源', { selector: 'span' });
    expect(disabled.className).toContain('line-through');
    expect(enabled.className).not.toContain('line-through');
  });
});
