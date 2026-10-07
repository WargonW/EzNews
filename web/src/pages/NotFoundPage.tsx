/**
 * 404 页。
 *
 * 不做成系统级的"错误页"风格 —— 与空态保持同一套视觉语言，
 * 给出去处明确的下一步（回首页 / 去设置）。
 */

import { Link } from 'react-router-dom';
import { EmptyState } from '@/components/states';
import { InboxIcon } from '@/components/icons';

export function NotFoundPage(): JSX.Element {
  return (
    <div className="mx-auto max-w-2xl px-4 py-20">
      <EmptyState
        icon={<InboxIcon className="text-2xl" />}
        title="页面不存在"
        description="链接可能已失效，或文章已被删除。"
        action={
          <Link to="/" className="ez-btn ez-btn-md ez-btn-primary">
            返回首页
          </Link>
        }
      />
    </div>
  );
}