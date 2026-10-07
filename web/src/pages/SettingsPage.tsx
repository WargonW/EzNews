/**
 * 设置面板（路由 `/settings`）。
 *
 * ============================ 产品定位 ============================
 * PRD R2：这是**登录入口的第二处**（第一处是顶栏）。因此：
 * - 游客进来看到的是偏好设置 + 一个"登录/注册"按钮；
 * - 登录后同一页面多出账号区（资料、改密、会话管理、注销）。
 *
 * 偏好存储策略（F-ACC-03）：
 * - 游客 → 写本地 localStorage；
 * - 登录 → **同时**写本地（保证离线可用）与服务端（跨设备同步）。
 *   写入服务端用 `PUT /me/preferences` 整包覆盖（天然幂等）。
 */

import { useCallback, useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate } from 'react-router-dom';
import { ApiError } from '@/api/client';
import { changePassword, deleteAccount, listSessions, logout, putPreferences, revokeSession } from '@/api/endpoints';
import type { PreferenceKV } from '@/api/types';
import { formatAbsolute, formatRelative } from '@/lib/datetime';
import { meKeys } from '@/lib/queryKeys';
import { useAuthStore } from '@/stores/auth';
import { prefBool, prefNumber, useGuestStore } from '@/stores/guest';
import { useOnlineStatus } from '@/hooks/useOnlineStatus';
import { hasPendingMerge, useMergeGuestData } from '@/hooks/useMergeGuestData';
import { fullResyncUserState } from '@/hooks/useUserStateSync';
import { EmptyState, InlineError, InlineSpinner, OfflineBanner, Toast } from '@/components/states';
import { DevicesIcon, LogoutIcon, RefreshIcon, ShieldIcon, TrashIcon } from '@/components/icons';

/** 可选音色（腾讯云 TTS 常见音色 id）。服务端如有不同配置需相应调整。 */
const VOICE_OPTIONS = [
  { id: '101001', label: '智瑜（女声 · 亲和）' },
  { id: '101003', label: '智雲（女声 · 沉稳）' },
  { id: '101004', label: '智言（女声 · 清晰）' },
  { id: '101005', label: '智楚（男声 · 稳重）' },
  { id: '101006', label: '智彤（女声 · 活泼）' },
];

export function SettingsPage({ onOpenAuth }: { onOpenAuth: () => void }): JSX.Element {
  const status = useAuthStore((s) => s.status);
  const user = useAuthStore((s) => s.user);
  const clearLocalSession = useAuthStore((s) => s.clearLocalSession);
  const online = useOnlineStatus();
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const preferences = useGuestStore((s) => s.preferences);
  const setPreferences = useGuestStore((s) => s.setPreferences);
  const resetGuestState = useGuestStore((s) => s.resetGuestState);

  const loggedIn = status === 'authenticated';
  const { phase: mergePhase, merge: retryMerge } = useMergeGuestData(false);

  const [toastMsg, setToastMsg] = useState<string | null>(null);
  useEffect(() => {
    if (!toastMsg) return;
    const t = window.setTimeout(() => setToastMsg(null), 3000);
    return () => window.clearTimeout(t);
  }, [toastMsg]);

  /** 改偏好：本地立即生效；登录时异步整包同步到服务端。 */
  const updatePreference = useCallback(
    (key: string, value: string) => {
      setPreferences({ [key]: value });
      if (loggedIn) {
        // 乐观更新：不等服务端响应。反错再改回来即可。
        queryClient.setQueryData<{ preferences: PreferenceKV }>(meKeys.preferences, (old) => ({
          preferences: { ...(old?.preferences ?? {}), [key]: value },
          updatedAt: new Date().toISOString(),
        }));
      }
    },
    [setPreferences, loggedIn, queryClient],
  );

  // 偏好同步到服务端（debounce 1.5s，避免拖动滑块时狂发请求）。
  useEffect(() => {
    if (!loggedIn) return;
    const t = window.setTimeout(() => {
      putPreferences({ preferences: preferences as PreferenceKV }).catch(() => {
        /* 失败仅影响跨设备同步，本地已生效；下次修改会再试 */
      });
    }, 1500);
    return () => window.clearTimeout(t);
  }, [preferences, loggedIn]);

  return (
    <div className="mx-auto max-w-2xl px-4 py-8 sm:px-6">
      <h1 className="text-xl font-bold tracking-tight text-ink">设置</h1>
      <p className="mt-1 text-[13px] text-ink-muted">
        {loggedIn ? '偏好会同步到你的账号，换设备自动生效。' : '偏好保存在本机。登录后自动同步到云端。'}
      </p>

      {!online ? <OfflineBanner /> : null}

      <div className="mt-6 space-y-6">
        {/* ============ 账号区 ============ */}
        <section className="ez-card p-5">
          <h2 className="text-[15px] font-semibold text-ink">账号</h2>

          {!loggedIn ? (
            <div className="mt-3">
              <p className="text-[13px] leading-relaxed text-ink-muted">
                你正在以游客身份使用。浏览、筛选、搜索、播放、收藏、源管理
                <strong className="font-semibold text-ink">全部可用</strong>，
                收藏与已读保存在本机浏览器里。
              </p>
              <p className="mt-2 text-[13px] leading-relaxed text-ink-muted">
                登录后，收藏、已读与偏好会跨设备同步 —— 本地已有数据会自动合并进账号，不丢失。
              </p>
              <button type="button" onClick={onOpenAuth} className="ez-btn ez-btn-md ez-btn-primary mt-4">
                登录 / 注册
              </button>
            </div>
          ) : (
            <>
            <AccountSection
              username={user?.username ?? ''}
              email={user?.email ?? null}
              role={user?.role ?? 'user'}
              createdAt={user?.createdAt ?? ''}
              mergePhase={mergePhase}
              onRetryMerge={() => void retryMerge()}
              onLogout={async () => {
                try {
                  // 服务端按 Access Token 的 sid 销毁 session（DEC-14），
                  // 失败也继续清本地：本地必须无条件回到游客态。
                  await logout();
                } catch {
                  /* 忽略：本地清理更重要 */
                }
                clearLocalSession();
                // 登出后清空所有服务端数据缓存，避免下个账号看到本账号数据。
                queryClient.clear();
                resetGuestState();
                navigate('/');
              }}
            />
            {/* 管理员：进入后台的第二入口（第一处是顶栏） */}
            {user?.role === 'admin' ? (
              <Link to="/admin" className="ez-btn ez-btn-sm ez-btn-secondary mt-4">
                <ShieldIcon className="text-sm" />
                进入管理后台
              </Link>
            ) : null}
            </>
          )}

          {/* 合并待同步标记（US-ACC-05） */}
          {loggedIn && hasPendingMerge() && mergePhase !== 'running' ? (
            <div className="mt-4">
              <InlineError
                error={new Error('有本地数据尚未同步到账号')}
                onRetry={() => void retryMerge()}
              />
              <p className="mt-1.5 text-[11px] text-ink-faint">
                同步会在后台自动重试；也可以现在手动触发。功能使用不受影响。
              </p>
            </div>
          ) : null}

          {/* 自愈入口：本地与云端疑似不一致时，重新全量对齐 */}
          {loggedIn ? (
            <button
              type="button"
              onClick={() => {
                setToastMsg('正在从云端重新同步…');
                void fullResyncUserState()
                  .then(() => setToastMsg('已与云端对齐'))
                  .catch(() => setToastMsg('同步失败，请检查网络'));
              }}
              className="ez-btn ez-btn-sm ez-btn-ghost mt-3 text-ink-muted"
            >
              <RefreshIcon />
              重新从云端同步
            </button>
          ) : null}
        </section>

        {/* ============ 语音偏好 ============ */}
        <section className="ez-card p-5">
          <h2 className="text-[15px] font-semibold text-ink">语音播报</h2>
          <p className="mt-1 text-[12px] text-ink-muted">
            语音由服务端合成，首次播放需要等待，合成结果会缓存复用。
          </p>

          <div className="mt-4 space-y-4">
            <div>
              <label className="ez-label" htmlFor="pref-voice">
                音色
              </label>
              <select
                id="pref-voice"
                className="ez-input"
                value={preferences['tts.voice']}
                onChange={(e) => updatePreference('tts.voice', e.target.value)}
              >
                {VOICE_OPTIONS.map((v) => (
                  <option key={v.id} value={v.id}>
                    {v.label}
                  </option>
                ))}
              </select>
            </div>

            <div>
              <label className="ez-label" htmlFor="pref-speed">
                语速：<span className="tabular-nums">{prefNumber(preferences, 'tts.speed', 1).toFixed(2)}×</span>
              </label>
              <input
                id="pref-speed"
                type="range"
                min={0.5}
                max={2}
                step={0.05}
                value={prefNumber(preferences, 'tts.speed', 1)}
                onChange={(e) => updatePreference('tts.speed', e.target.value)}
                className="w-full accent-blue-600"
              />
              <div className="mt-1 flex justify-between text-[10px] text-ink-faint">
                <span>0.5× 慢</span>
                <span>1.0×</span>
                <span>2.0× 快</span>
              </div>
            </div>

            <label className="flex cursor-pointer items-center justify-between gap-3">
              <span>
                <span className="block text-[13px] font-medium text-ink">自动连播</span>
                <span className="block text-[11px] text-ink-muted">当前文章播完后自动播放下一篇</span>
              </span>
              <input
                type="checkbox"
                checked={prefBool(preferences, 'player.autoNext', false)}
                onChange={(e) => updatePreference('player.autoNext', String(e.target.checked))}
                className="h-4 w-4 shrink-0 accent-blue-600"
              />
            </label>
          </div>
        </section>

        {/* ============ 登录态：会话与安全 ============ */}
        {loggedIn ? (
          <>
            <SessionsSection />
            <DangerSection />
          </>
        ) : null}

        {/* ============ 关于 ============ */}
        <section className="ez-card p-5">
          <h2 className="text-[15px] font-semibold text-ink">关于</h2>
          <dl className="mt-3 space-y-2 text-[12px]">
            <div className="flex justify-between">
              <dt className="text-ink-muted">版本</dt>
              <dd className="text-ink">EZNews 1.0.0</dd>
            </div>
            <div className="flex justify-between">
              <dt className="text-ink-muted">API</dt>
              <dd className="font-mono text-[11px] text-ink-faint">
                {import.meta.env.VITE_API_BASE_URL || '同源（/api/v1）'}
              </dd>
            </div>
          </dl>
          <p className="mt-3 text-[11px] leading-relaxed text-ink-faint">
            游客态的收藏、已读与偏好保存在本机浏览器；登录后同步到你的账号。
            本项目不采集通讯录、位置、设备指纹或广告 ID。
          </p>
        </section>
      </div>

      {/* 轻提示 */}
      {toastMsg ? (
        <div className="pointer-events-none fixed bottom-6 left-1/2 z-50 w-full max-w-sm -translate-x-1/2 px-4">
          <Toast message={toastMsg} onClose={() => setToastMsg(null)} />
        </div>
      ) : null}
    </div>
  );
}

/* ========================================================================== *
 * 账号信息 + 登出
 * ========================================================================== */

function AccountSection({
  username,
  email,
  role,
  createdAt,
  mergePhase,
  onRetryMerge,
  onLogout,
}: {
  username: string;
  email: string | null;
  role: string;
  createdAt: string;
  mergePhase: string;
  onRetryMerge: () => void;
  onLogout: () => Promise<void>;
}): JSX.Element {
  const [confirmLogout, setConfirmLogout] = useState(false);

  return (
    <div className="mt-3">
      <div className="flex items-center gap-3">
        <span className="flex h-11 w-11 shrink-0 items-center justify-center rounded-full bg-brand text-base font-bold text-white">
          {username.slice(0, 1).toUpperCase()}
        </span>
        <div className="min-w-0">
          <p className="truncate text-[15px] font-medium text-ink">{username}</p>
          <p className="truncate text-[12px] text-ink-muted">
            {email ?? '未填写邮箱'}
            {role === 'admin' ? ' · 管理员' : ''}
            {createdAt ? ` · 注册于 ${formatAbsolute(createdAt)}` : ''}
          </p>
        </div>
      </div>

      {mergePhase === 'running' ? (
        <p className="mt-3 flex items-center gap-1.5 text-[12px] text-ink-muted">
          <InlineSpinner label="正在同步本地数据到账号…" />
        </p>
      ) : mergePhase === 'done' ? (
        <p className="mt-3 text-[12px] text-emerald-700 dark:text-emerald-400">本地数据已同步到账号。</p>
      ) : mergePhase === 'failed' ? (
        /*合并失败不阻断任何功能，只在这里给一个手动重试入口。 */
        <div className="mt-3">
          <p className="text-[12px] text-amber-700 dark:text-amber-400">
            本地数据尚未同步到账号（可能网络中断）。功能使用不受影响，稍后会自动重试。
          </p>
          <button
            type="button"
            onClick={onRetryMerge}
            className="ez-btn ez-btn-sm ez-btn-secondary mt-2"
          >
            <RefreshIcon />
            立即重试
          </button>
        </div>
      ) : null}

      {confirmLogout ? (
        <div className="mt-4 rounded-lg border border-amber-200 bg-amber-50 p-3 dark:border-amber-900 dark:bg-amber-950/40">
          <p className="text-[12px] text-amber-900 dark:text-amber-200">
            登出后将回到游客态：功能不受任何限制，仅失去跨设备同步能力。
          </p>
          <div className="mt-2.5 flex gap-2">
            <button
              type="button"
              onClick={() => void onLogout()}
              className="ez-btn ez-btn-sm ez-btn-danger"
            >
              确认登出
            </button>
            <button
              type="button"
              onClick={() => setConfirmLogout(false)}
              className="ez-btn ez-btn-sm ez-btn-secondary"
            >
              取消
            </button>
          </div>
        </div>
      ) : (
        <button
          type="button"
          onClick={() => setConfirmLogout(true)}
          className="ez-btn ez-btn-md ez-btn-secondary mt-4"
        >
          <LogoutIcon />
          退出登录
        </button>
      )}
    </div>
  );
}

/* ========================================================================== *
 * 会话管理（P2）
 * ========================================================================== */

function SessionsSection(): JSX.Element {
  const query = useQuery({
    queryKey: meKeys.sessions,
    queryFn: listSessions,
    staleTime: 30_000,
  });

  const revokeMutation = useMutation({
    mutationFn: (id: number) => revokeSession(id),
    onSuccess: () => void query.refetch(),
  });

  return (
    <section className="ez-card p-5">
      <h2 className="flex items-center gap-2 text-[15px] font-semibold text-ink">
        <DevicesIcon className="text-base" />
        登录设备
      </h2>
      <p className="mt-1 text-[12px] text-ink-muted">
        发现不认识的设备？可以直接把它踢下线。
      </p>

      {query.isLoading ? (
        <div className="mt-4 space-y-2">
          {[0, 1].map((i) => (
            <div key={i} className="ez-skeleton h-14 w-full rounded-lg" />
          ))}
        </div>
      ) : query.isError ? (
        <div className="mt-4">
          <InlineError error={query.error} onRetry={() => void query.refetch()} />
        </div>
      ) : (query.data?.items.length ?? 0) === 0 ? (
        <EmptyState compact title="没有活跃会话" />
      ) : (
        <ul className="mt-4 space-y-2">
          {query.data?.items.map((s) => (
            <li key={s.id} className="flex items-center gap-3 rounded-lg border border-line p-3">
              <div className="min-w-0 flex-1">
                <p className="truncate text-[13px] font-medium text-ink">
                  {s.deviceName ?? '未知设备'}
                  {s.current ? (
                    <span className="ez-badge ml-2 bg-brand-soft text-brand dark:text-blue-200">当前设备</span>
                  ) : null}
                </p>
                <p className="mt-0.5 text-[11px] text-ink-faint">
                  最近活动 {formatRelative(s.lastSeenAt)} · 到期 {formatAbsolute(s.expiresAt)}
                </p>
              </div>
              {!s.current ? (
                <button
                  type="button"
                  onClick={() => revokeMutation.mutate(s.id)}
                  disabled={revokeMutation.isPending}
                  className="ez-btn ez-btn-sm ez-btn-secondary shrink-0 text-rose-600 dark:text-rose-400"
                >
                  {revokeMutation.isPending && revokeMutation.variables === s.id ? (
                    <InlineSpinner />
                  ) : (
                    '踢下线'
                  )}
                </button>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/* ========================================================================== *
 * 危险操作：改密 + 注销
 * ========================================================================== */

function DangerSection(): JSX.Element {
  const queryClient = useQueryClient();
  const clearLocalSession = useAuthStore((s) => s.clearLocalSession);
  const navigate = useNavigate();

  const [oldPassword, setOldPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [pwMsg, setPwMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const [deletePassword, setDeletePassword] = useState('');
  const [confirmDelete, setConfirmDelete] = useState(false);

  const pwdMutation = useMutation({
    mutationFn: () => changePassword({ oldPassword, newPassword }),
    onSuccess: (token) => {
      // 服务端在改密成功后会下发新的 token 对（token_version 已递增，
      // 旧的 access token 全部失效），必须立即换新，否则下一步请求就会 401。
      useAuthStore.getState().applyTokens(token);
      setOldPassword('');
      setNewPassword('');
      setPwMsg({ ok: true, text: '密码已修改，其他设备需要重新登录。' });
    },
    onError: (err) => {
      setPwMsg({
        ok: false,
        text:
          err instanceof ApiError
            ? err.code === 'INVALID_CREDENTIALS'
              ? '原密码不正确'
              : err.message
            : '修改失败，请检查网络',
      });
    },
  });

  const deleteMutation = useMutation({
    mutationFn: () => deleteAccount({ password: deletePassword }),
    onSuccess: () => {
      clearLocalSession();
      queryClient.clear();
      navigate('/');
    },
  });

  return (
    <section className="ez-card border-rose-200 p-5 dark:border-rose-900/60">
      <h2 className="text-[15px] font-semibold text-ink">安全</h2>

      {/* 改密 */}
      <details className="group mt-3">
        <summary className="ez-btn ez-btn-sm ez-btn-secondary cursor-pointer list-none">
          修改密码
        </summary>
        <form
          className="mt-3 space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            setPwMsg(null);
            pwdMutation.mutate();
          }}
        >
          <div>
            <label className="ez-label" htmlFor="old-pw">
              当前密码
            </label>
            <input
              id="old-pw"
              type="password"
              className="ez-input"
              value={oldPassword}
              onChange={(e) => setOldPassword(e.target.value)}
              autoComplete="current-password"
              required
            />
          </div>
          <div>
            <label className="ez-label" htmlFor="new-pw">
              新密码
            </label>
            <input
              id="new-pw"
              type="password"
              className="ez-input"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              autoComplete="new-password"
              minLength={8}
              maxLength={128}
              required
            />
            <p className="mt-1 text-[11px] text-ink-faint">至少 8 个字符，不强制复杂度规则。</p>
          </div>
          {pwMsg ? (
            <p
              className={`text-[12px] ${pwMsg.ok ? 'text-emerald-700 dark:text-emerald-400' : 'text-rose-600 dark:text-rose-400'}`}
              role="alert"
            >
              {pwMsg.text}
            </p>
          ) : null}
          <button
            type="submit"
            disabled={pwdMutation.isPending || !oldPassword || !newPassword}
            className="ez-btn ez-btn-md ez-btn-primary"
          >
            {pwdMutation.isPending ? <InlineSpinner /> : '确认修改'}
          </button>
        </form>
      </details>

      {/* 注销 */}
      <details className="group mt-3">
        <summary className="ez-btn ez-btn-sm ez-btn-danger cursor-pointer list-none">
          <TrashIcon className="text-sm" />
          注销账号
        </summary>
        <div className="mt-3 rounded-lg bg-rose-50 p-3 dark:bg-rose-950/40">
          <p className="text-[12px] leading-relaxed text-rose-800 dark:text-rose-300">
            注销会<strong className="font-semibold">永久删除</strong>你的账号，以及收藏、已读、偏好与会话记录。
            文章与新闻源是全局共享数据，不受影响。
            <strong className="font-semibold">此操作不可撤销</strong>，服务端没有恢复路径。
          </p>
          {!confirmDelete ? (
            <button
              type="button"
              onClick={() => setConfirmDelete(true)}
              className="ez-btn ez-btn-sm ez-btn-secondary mt-3"
            >
              我已了解，继续
            </button>
          ) : (
            <form
              className="mt-3 space-y-2"
              onSubmit={(e) => {
                e.preventDefault();
                deleteMutation.mutate();
              }}
            >
              <label className="ez-label" htmlFor="del-pw">
                输入当前密码确认
              </label>
              <input
                id="del-pw"
                type="password"
                className="ez-input"
                value={deletePassword}
                onChange={(e) => setDeletePassword(e.target.value)}
                autoComplete="current-password"
                required
              />
              {deleteMutation.isError ? (
                <p className="text-[12px] text-rose-700 dark:text-rose-400">
                  {deleteMutation.error instanceof ApiError
                    ? deleteMutation.error.code === 'INVALID_CREDENTIALS'
                      ? '密码错误，注销已取消'
                      : deleteMutation.error.message
                    : '注销失败，请检查网络'}
                </p>
              ) : null}
              <div className="flex gap-2">
                <button
                  type="submit"
                  disabled={deleteMutation.isPending || !deletePassword}
                  className="ez-btn ez-btn-sm ez-btn-danger"
                >
                  {deleteMutation.isPending ? <InlineSpinner /> : '永久注销'}
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setConfirmDelete(false);
                    setDeletePassword('');
                  }}
                  className="ez-btn ez-btn-sm ez-btn-secondary"
                >
                  取消
                </button>
              </div>
            </form>
          )}
        </div>
      </details>
    </section>
  );
}