/**
 * 后台「新闻源」分区（`/admin/sources`）。
 *
 * ★ **不新造端点**：直接复用阅读端已有的 `/api/v1/sources`（CRUD 已存在），
 *   UI 也直接复用 `SourcePanel` 的 `embedded` 模式 —— 源管理的三处服务端约束
 *   （系统预置源只能改 enabled、预置源不可删、URL 形态校验）在两个入口必须一致，
 *   复制一份必然漂移。
 *
 * 顶部额外写一句"新闻源是全局共享数据"：后台里"删除"这个动作 elsewhere 指账号，
 * 在这里指源，运维者容易把两者的影响范围搞混（删源会连带删文章，删账号不会）。
 */

import { SourcePanel } from '@/components/SourcePanel';
import { PageHeader } from './AdminOverviewPage';

export function AdminSourcesPage(): JSX.Element {
  return (
    <div className="space-y-5">
      <PageHeader title="新闻源" subtitle="RSS / Atom 源的增删改与启停" />

      <div className="rounded-lg border border-line bg-surface-sunken px-3.5 py-2.5 text-[12px] leading-relaxed text-ink-muted">
        新闻源是<strong className="font-semibold text-ink">全局共享数据</strong>：
        停用只影响是否继续进入默认列表，删除自定义源会连带删除该源下的文章。
        删除<strong className="font-semibold text-ink">用户账号</strong>不会影响新闻源。
      </div>

      {/*
        open 恒为 true：内嵌模式下 `open` 只用于 `!open && !embedded` 的早退判断，
        onClose 不会被触发（遮罩点击与 Esc 都已按 embedded 关闭），这里传一个空函数。
      */}
      <SourcePanel open embedded onClose={() => undefined} />
    </div>
  );
}
