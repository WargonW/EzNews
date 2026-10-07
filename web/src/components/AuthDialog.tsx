/**
 * 登录 / 注册 弹窗。
 *
 * ============================ 产品红线 ============================
 * PRD-ACCOUNT §1.1 R1/R2：
 * - **禁止**在浏览、筛选、播放路径上出现登录弹窗；
 * - 登录入口固定两处：顶栏「登录」按钮、设置面板「登录/注册」。
 *
 * 因此本组件**只**能被这两处调用，绝不在文章流、收藏按钮、播放按钮里触发。
 * 游客点收藏/已读一律立即写本地，不做任何"请先登录"的拦截。
 *
 * 登录成功后：**静默**触发合并（见 useMergeGuestData），无二次确认、无弹窗。
 */

import { useEffect, useRef, useState } from 'react';
import { ApiError } from '@/api/client';
import { login, register } from '@/api/endpoints';
import { getClientId, getDeviceName, useAuthStore } from '@/stores/auth';
import { useMergeGuestData } from '@/hooks/useMergeGuestData';
import { InlineSpinner } from './states';
import { CloseIcon } from './icons';

export interface AuthDialogProps {
  open: boolean;
  onClose: () => void;
  /** 登录成功后回调（用于关闭弹窗 + 切页）。 */
  onSuccess?: () => void;
}

type Mode = 'login' | 'register';

export function AuthDialog({ open, onClose, onSuccess }: AuthDialogProps): JSX.Element | null {
  const [mode, setMode] = useState<Mode>('login');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [email, setEmail] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  const applyTokens = useAuthStore((s) => s.applyTokens);
  const { merge } = useMergeGuestData(false);
  const dialogRef = useRef<HTMLDivElement>(null);
  const firstFieldRef = useRef<HTMLInputElement>(null);

  // 打开时重置表单与焦点。
  useEffect(() => {
    if (!open) return;
    setError(null);
    setFieldErrors({});
    // setTimeout 让焦点在渲染后生效，避免与入场动画抢焦点。
    const t = window.setTimeout(() => firstFieldRef.current?.focus(), 30);
    return () => window.clearTimeout(t);
  }, [open, mode]);

  // Esc 关闭 + 焦点陷阱（无障碍：弹窗内焦点不应跑到背景上）。
  useEffect(() => {
    if (!open) return;

    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        onClose();
        return;
      }
      if (e.key !== 'Tab') return;

      const focusables = dialogRef.current?.querySelectorAll<HTMLElement>(
        'button:not([disabled]), input:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])',
      );
      if (!focusables || focusables.length === 0) return;
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      if (!first || !last) return;

      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    };

    document.addEventListener('keydown', onKey);
    // 打开时锁背景滚动。
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.removeEventListener('keydown', onKey);
      document.body.style.overflow = prevOverflow;
    };
  }, [open, onClose]);

  if (!open) return null;

  /** 提交。校验在前端做一遍，服务端仍会校验（前端只为体验）。 */
  const submit = async (e: React.FormEvent): Promise<void> => {
    e.preventDefault();
    if (submitting) return;

    const errors: Record<string, string> = {};
    const u = username.trim();
    if (u.length < 3 || u.length > 32) errors.username = '用户名需为 3–32 个字符';
    if (password.length < 8 || password.length > 128) errors.password = '密码需为 8–128 个字符';
    if (mode === 'register' && email.trim() && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.trim())) {
      errors.email = '邮箱格式不正确';
    }
    setFieldErrors(errors);
    if (Object.keys(errors).length > 0) return;

    setSubmitting(true);
    setError(null);

    try {
      const clientId = getClientId();
      const deviceName = getDeviceName();

      const token =
        mode === 'login'
          ? await login({ username: u, password, deviceName, clientId })
          : await register({
              username: u,
              password,
              email: email.trim() || null,
              deviceName,
              clientId,
            });

      // Access Token 只进内存（applyTokens 内部已保证不落盘）。
      applyTokens(token);

      // ★ 静默合并：不弹窗、不确认。失败也不影响登录态。
      void merge();

      onSuccess?.();
      onClose();
    } catch (err) {
      if (err instanceof ApiError) {
        // 403 = 注册已关闭（auth.registrationEnabled=false），需明确告知而非笼统报错。
        if (err.status === 403) {
          setError('该实例已关闭注册。如需创建账号，请联系部署者。');
          setMode('login');
        } else if (err.code === 'INVALID_CREDENTIALS') {
          setError('用户名或密码错误');
        } else if (err.status === 429) {
          setError(
            err.retryAfterSec
              ? `尝试过于频繁，请 ${err.retryAfterSec} 秒后再试`
              : '尝试过于频繁，请稍后再试',
          );
        } else if (err.status === 503) {
          setError('服务繁忙，请稍后重试');
        } else if (err.code === 'CONFLICT') {
          setError('该用户名已被占用，换一个试试');
        } else {
          setError(err.message || '操作失败，请稍后重试');
        }
        // 字段级明细（VALIDATION_FAILED）
        if (err.details && err.details.length > 0) {
          const map: Record<string, string> = {};
          for (const d of err.details) map[d.field] = d.message;
          setFieldErrors(map);
        }
      } else {
        setError('网络异常，请检查连接后重试');
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-end justify-center bg-slate-900/40 p-0 backdrop-blur-sm animate-fade-in sm:items-center sm:p-4"
      onMouseDown={(e) => {
        // 点击遮罩关闭；点击内容不关。
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="auth-title"
        className="ez-enter w-full max-w-sm rounded-t-2xl border border-line bg-surface-raised p-5 shadow-pop sm:rounded-2xl sm:p-6"
      >
        <div className="mb-5 flex items-start justify-between">
          <div>
            <h2 id="auth-title" className="text-base font-semibold text-ink">
              {mode === 'login' ? '登录' : '注册'}
            </h2>
            <p className="mt-1 text-[12px] leading-relaxed text-ink-muted">
              {mode === 'login'
                ? '登录后收藏、已读与偏好可跨设备同步。不登录也能完整使用。'
                : '创建账号后，本地已有的收藏与已读会自动合并到账号。'}
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="ez-btn ez-btn-sm ez-btn-ghost -mr-1 -mt-1 h-8 w-8 p-0"
            aria-label="关闭"
          >
            <CloseIcon className="text-base" />
          </button>
        </div>

        <form onSubmit={(e) => void submit(e)} noValidate>
          <div className="space-y-3.5">
            <div>
              <label className="ez-label" htmlFor="auth-username">
                用户名
              </label>
              <input
                ref={firstFieldRef}
                id="auth-username"
                className={`ez-input ${fieldErrors.username ? 'border-rose-400 dark:border-rose-600' : ''}`}
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                autoComplete="username"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                maxLength={32}
                required
              />
              {fieldErrors.username ? (
                <p className="mt-1 text-[11px] text-rose-600 dark:text-rose-400">{fieldErrors.username}</p>
              ) : null}
            </div>

            <div>
              <label className="ez-label" htmlFor="auth-password">
                密码
              </label>
              <input
                id="auth-password"
                type="password"
                className={`ez-input ${fieldErrors.password ? 'border-rose-400 dark:border-rose-600' : ''}`}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete={mode === 'login' ? 'current-password' : 'new-password'}
                maxLength={128}
                required
              />
              {fieldErrors.password ? (
                <p className="mt-1 text-[11px] text-rose-600 dark:text-rose-400">{fieldErrors.password}</p>
              ) : null}
            </div>

            {mode === 'register' ? (
              <div>
                <label className="ez-label" htmlFor="auth-email">
                  邮箱<span className="ml-1 font-normal text-ink-faint">（选填，不验证）</span>
                </label>
                <input
                  id="auth-email"
                  type="email"
                  className={`ez-input ${fieldErrors.email ? 'border-rose-400 dark:border-rose-600' : ''}`}
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  autoComplete="email"
                  maxLength={128}
                />
                {fieldErrors.email ? (
                  <p className="mt-1 text-[11px] text-rose-600 dark:text-rose-400">{fieldErrors.email}</p>
                ) : null}
              </div>
            ) : null}
          </div>

          {error ? (
            <p
              className="mt-3.5 rounded-lg bg-rose-50 px-3 py-2 text-[12px] text-rose-700 dark:bg-rose-950/50 dark:text-rose-300"
              role="alert"
            >
              {error}
            </p>
          ) : null}

          <button
            type="submit"
            disabled={submitting}
            className="ez-btn ez-btn-lg ez-btn-primary mt-4 w-full"
          >
            {submitting ? <InlineSpinner label="处理中…" /> : mode === 'login' ? '登录' : '创建账号'}
          </button>

          <button
            type="button"
            onClick={() => setMode(mode === 'login' ? 'register' : 'login')}
            className="ez-btn ez-btn-sm ez-btn-ghost mt-2.5 w-full text-ink-muted"
          >
            {mode === 'login' ? '没有账号？去注册' : '已有账号？去登录'}
          </button>
        </form>

        {/* 密码找回的降级说明（PRD §2.4：无邮件通道） */}
        {mode === 'login' ? (
          <p className="mt-4 border-t border-line pt-3 text-[11px] leading-relaxed text-ink-faint">
            忘记密码？本实例不提供邮件找回，请联系部署者用命令行重置。
          </p>
        ) : null}
      </div>
    </div>
  );
}