/**
 * 二次确认弹窗（破坏性操作专用）。
 *
 * ============================ 为什么单独抽一个 ============================
 * 后台有三类不可撤销的操作（删除用户 / 踢全部会话 / 停用账号），
 * 各自内联一套确认 UI 会很快漂移成三种不同的措辞与按钮顺序。
 * 收敛到一处，保证：
 * - 确认按钮**永远在左**、取消永远在右（顺序固定，肌肉记忆不失效）；
 * - 危险操作一律 "确认 / 取消" 双按钮，不做单点确认；
 * - Esc 与点遮罩都等于"取消"，绝不等同于"确认"。
 *
 * ★ 设计取向：**不自动聚焦确认按钮**。让焦点落在弹窗容器上，
 *   用户必须主动点一下"确认"——回车误触在这个场景里代价太大。
 */

import { useEffect, useRef, type ReactNode } from 'react';
import { AlertIcon } from './icons';

export interface ConfirmDialogProps {
  open: boolean;
  title: string;
  /** 后果说明。必须写清楚"会失去什么"，不要只写"确定吗"。 */
  description: ReactNode;
  confirmLabel: string;
  /** 危险操作（红色确认按钮）。默认 true —— 后台用它就是为了拦破坏性操作。 */
  danger?: boolean;
  /** 提交中：禁用两个按钮，避免重复提交。 */
  pending?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel,
  danger = true,
  pending = false,
  onConfirm,
  onCancel,
}: ConfirmDialogProps): JSX.Element | null {
  const dialogRef = useRef<HTMLDivElement>(null);

  // Esc = 取消；同时把焦点移入弹窗（键盘用户的起点）。
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape' && !pending) onCancel();
    };
    document.addEventListener('keydown', onKey);
    const t = window.setTimeout(() => dialogRef.current?.focus(), 30);
    return () => {
      document.removeEventListener('keydown', onKey);
      window.clearTimeout(t);
    };
  }, [open, onCancel, pending]);

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-slate-900/40 p-4 backdrop-blur-sm animate-fade-in"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget && !pending) onCancel();
      }}
    >
      <div
        ref={dialogRef}
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="confirm-dialog-title"
        tabIndex={-1}
        className="w-full max-w-md rounded-xl border border-line bg-surface-raised p-5 shadow-pop"
      >
        <div className="flex gap-3">
          <div
            className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-full ${
              danger
                ? 'bg-rose-50 text-rose-600 dark:bg-rose-950/50 dark:text-rose-400'
                : 'bg-brand-soft text-brand dark:text-blue-200'
            }`}
          >
            <AlertIcon className="text-lg" />
          </div>
          <div className="min-w-0 flex-1">
            <h2 id="confirm-dialog-title" className="text-[15px] font-semibold text-ink">
              {title}
            </h2>
            <div className="mt-1.5 text-[13px] leading-relaxed text-ink-muted">{description}</div>
          </div>
        </div>

        <div className="mt-5 flex justify-end gap-2">
          <button
            type="button"
            onClick={onConfirm}
            disabled={pending}
            className={`ez-btn ez-btn-md ${danger ? 'ez-btn-danger' : 'ez-btn-primary'}`}
          >
            {confirmLabel}
          </button>
          <button
            type="button"
            onClick={onCancel}
            disabled={pending}
            className="ez-btn ez-btn-md ez-btn-secondary"
          >
            取消
          </button>
        </div>
      </div>
    </div>
  );
}
