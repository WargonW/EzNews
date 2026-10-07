/**
 * 分类中文标签与常量。
 *
 * openapi 的 Category 是英文枚举 key，服务端 `GET /api/v1/categories`
 * 会下发 `label`（中文）。本文件提供**兜底**标签：当分类接口未加载或
 * 服务端未返回 label 时（例如新增了枚举值但前端未更新），UI 不至于显示英文。
 */

import type { Category, SourceType } from '@/api/types';

/** 分类 key → 中文标签（兜底）。 */
export const CATEGORY_LABELS: Record<Category, string> = {
  tech: '科技',
  finance: '财经',
  sports: '体育',
  world: '国际',
  china: '国内',
  ent: '娱乐',
  life: '生活',
  auto: '汽车',
  military: '军事',
  science: '科学',
  health: '健康',
  other: '其他',
};

/** 分类枚举顺序（侧栏 Tab 展示顺序，与常见资讯产品一致）。 */
export const CATEGORY_ORDER: Category[] = [
  'tech',
  'finance',
  'world',
  'china',
  'sports',
  'ent',
  'life',
  'auto',
  'science',
  'health',
  'military',
  'other',
];

/** 分类对应的视觉色调（徽标底色），用 slate/blue 为基调的克制配色。 */
export const CATEGORY_TONE: Record<Category, string> = {
  tech: 'bg-blue-50 text-blue-700 dark:bg-blue-950 dark:text-blue-300',
  finance: 'bg-emerald-50 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-300',
  sports: 'bg-orange-50 text-orange-700 dark:bg-orange-950 dark:text-orange-300',
  world: 'bg-violet-50 text-violet-700 dark:bg-violet-950 dark:text-violet-300',
  china: 'bg-rose-50 text-rose-700 dark:bg-rose-950 dark:text-rose-300',
  ent: 'bg-pink-50 text-pink-700 dark:bg-pink-950 dark:text-pink-300',
  life: 'bg-teal-50 text-teal-700 dark:bg-teal-950 dark:text-teal-300',
  auto: 'bg-cyan-50 text-cyan-700 dark:bg-cyan-950 dark:text-cyan-300',
  military: 'bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300',
  science: 'bg-indigo-50 text-indigo-700 dark:bg-indigo-950 dark:text-indigo-300',
  health: 'bg-lime-50 text-lime-700 dark:bg-lime-950 dark:text-lime-300',
  other: 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-400',
};

/** 取分类标签，优先用服务端下发的 label。 */
export function categoryLabel(key: Category | string | undefined, serverLabel?: string): string {
  if (serverLabel) return serverLabel;
  if (key && key in CATEGORY_LABELS) return CATEGORY_LABELS[key as Category];
  return '其他';
}

/** 源类型中文标签。 */
export const SOURCE_TYPE_LABELS: Record<SourceType, string> = {
  rss: 'RSS',
  atom: 'Atom',
  api: 'API',
  manual: '手动',
};

/** openapi 中 sourceId 与 sourceKey "二选一"的说明文案。 */
export const SOURCE_FILTER_HINT = 'sourceId 与 sourceKey 二选一；同时提供以 sourceId 为准。';