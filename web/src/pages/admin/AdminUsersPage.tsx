/**
 * 后台「用户」分区（`/admin/users`）。
 *
 * ============================ ★ 按钮能不能点，读服务端下发的能力位 ============================
 * `AdminUserDTO` 每行自带 `self / canChangeRole / canDisable / canDelete / canRevokeAll`，
 * 判据是服务端写的（目标不是本人、且不是"最后一个可用管理员"）。
 * **前端不再自己数 admins 去推** —— 那条规则在两端各写一遍必然漂移。
 *
 * ============================ ★ 主保护是 409 错误气泡 ============================
 * 能力位只服务 UX。服务端对每个写操作独立校验：
 * - 改自己角色 / 停用自己 / 删自己        → 409 CONFLICT
 * - 动最后一个可用管理员（降级/停用/删除）→ 409 CONFLICT
 * - 删除用户名重复（建号时）              → 409 CONFLICT
 * - `role` 传非法值、`disabled` 漏传      → 400 VALIDATION_FAILED
 * 所以每个 mutation 的失败都必须**显示出来**：按钮置灰只能防手滑，
 * 防不了"两个管理员同时操作"这种竞态（A 看到的位，在 B 降级之后就过时了）。
 *
 * ============================ 时间与轮次 ============================
 * 时间字段是 **epoch 秒**（不是 ISO 串），用 `formatAbsoluteSec` / `formatRelativeSec` 渲染；
 * `lastLoginAt === 0` 表示从未登录。
 */

import { useEffect, useState } from 'react';
import type { AdminUser, UserRole } from '@/api/types';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { EmptyState, ErrorState, InlineError, InlineSpinner, Skeleton, Toast } from '@/components/states';
import { PlusIcon, SearchIcon, TrashIcon } from '@/components/icons';
import { formatAbsoluteSec, formatRelativeSec } from '@/lib/datetime';
import {
  useAdminUsers,
  useCreateAdminUser,
  useDeleteAdminUser,
  useRevokeAllAdminUserSessions,
  useSetAdminUserDisabled,
  useSetAdminUserRole,
} from '@/hooks/useAdmin';
import { PageHeader } from './AdminOverviewPage';

/** 确认框的待执行动作。 */
type PendingAction =
  | { kind: 'delete'; user: AdminUser }
  | { kind: 'revoke'; user: AdminUser }
  | { kind: 'disable'; user: AdminUser };

export function AdminUsersPage(): JSX.Element {
  const [keyword, setKeyword] = useState('');
  // 搜索条件只在提交时生效 —— 每敲一个字符都换 queryKey 会让列表不停重建，
  // 且中间态（输到一半的 "a"）也会各发一次请求。
  const [q, setQ] = useState('');
  const [role, setRole] = useState<UserRole | ''>('');
  const [pending, setPending] = useState<PendingAction | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [notice, setNotice] = useState<{ message: string; tone: 'info' | 'success' | 'warn' } | null>(null);

  const list = useAdminUsers({ q, role: role === '' ? undefined : role });

  const roleMutation = useSetAdminUserRole();
  const disabledMutation = useSetAdminUserDisabled();
  const deleteMutation = useDeleteAdminUser();
  const revokeMutation = useRevokeAllAdminUserSessions();
  const createMutation = useCreateAdminUser();

  const pages = list.data?.pages ?? [];
  const users = pages.flatMap((p) => p.items);
  // 页级元信息取最新一页：total 与 admins 翻页时恒定，hasMore 只有最后一页准。
  const lastPage = pages[pages.length - 1];
  const busy =
    roleMutation.isPending ||
    disabledMutation.isPending ||
    deleteMutation.isPending ||
    revokeMutation.isPending;

  // 轻提示自动消失（非阻断）。
  useEffect(() => {
    if (!notice) return;
    const t = window.setTimeout(() => setNotice(null), 3200);
    return () => window.clearTimeout(t);
  }, [notice]);

  const submitSearch = (): void => setQ(keyword.trim());

  /**
   * 取当前 mutation 的错误优先展示。
   *
   * 409 是这里的**主保护**：能力位可能因为别人并发操作而过时，
   * 只有服务端的一句话能解释"为什么点了没反应"。
   */
  const mutationError =
    roleMutation.error ??
    disabledMutation.error ??
    deleteMutation.error ??
    revokeMutation.error ??
    createMutation.error;

  const clearMutationErrors = (): void => {
    roleMutation.reset();
    disabledMutation.reset();
    deleteMutation.reset();
    revokeMutation.reset();
    createMutation.reset();
  };

  const runAction = (): void => {
    if (!pending) return;
    const { kind, user } = pending;
    if (kind === 'delete') {
      deleteMutation.mutate(user.id, {
        onSuccess: () => {
          setPending(null);
          setNotice({ message: `已删除账号「${user.username}」`, tone: 'success' });
        },
        onSettled: () => setPending(null),
      });
      return;
    }
    if (kind === 'revoke') {
      revokeMutation.mutate(user.id, {
        onSuccess: (res) => {
          setPending(null);
          setNotice({
            message:
              res.revoked > 0
                ? `已吊销「${user.username}」的 ${res.revoked} 个会话`
                : `「${user.username}」当前没有活跃会话`,
            tone: res.revoked > 0 ? 'success' : 'info',
          });
        },
        onSettled: () => setPending(null),
      });
      return;
    }
    disabledMutation.mutate(
      { id: user.id, disabled: true },
      {
        onSuccess: () => {
          setPending(null);
          setNotice({ message: `已停用「${user.username}」`, tone: 'success' });
        },
        onSettled: () => setPending(null),
      },
    );
  };

  return (
    <div className="space-y-5">
      <PageHeader title="用户" subtitle="管理账号角色、停用状态与登录会话" />

      {/* 服务端口径的两个总数（列表可能正被筛选/翻页截断，不能自己数行） */}
      {lastPage ? (
        <p className="text-[12px] text-ink-faint">
          共 {lastPage.total} 个匹配用户 · {lastPage.admins} 个可用管理员（未停用）
        </p>
      ) : null}

      {/* ---------- 筛选 ---------- */}
      <div className="ez-card flex flex-wrap items-end gap-3 p-3.5">
        <form
          className="min-w-[12rem] flex-1"
          onSubmit={(e) => {
            e.preventDefault();
            submitSearch();
          }}
        >
          <label className="ez-label" htmlFor="admin-user-q">
            搜索用户名或邮箱
          </label>
          <div className="relative">
            <SearchIcon className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-base text-ink-faint" />
            <input
              id="admin-user-q"
              className="ez-input pl-9"
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
              placeholder="输入关键词后回车"
              maxLength={64}
            />
          </div>
        </form>

        <div>
          <label className="ez-label" htmlFor="admin-user-role">
            角色
          </label>
          <select
            id="admin-user-role"
            className="ez-input w-32"
            value={role}
            onChange={(e) => setRole(e.target.value as UserRole | '')}
          >
            <option value="">全部</option>
            <option value="admin">管理员</option>
            <option value="user">普通用户</option>
          </select>
        </div>

        <button type="button" onClick={submitSearch} className="ez-btn ez-btn-md ez-btn-secondary">
          搜索
        </button>
        <button
          type="button"
          onClick={() => {
            setKeyword('');
            setQ('');
            setRole('');
          }}
          className="ez-btn ez-btn-md ez-btn-ghost"
          disabled={!q && role === '' && !keyword}
        >
          重置
        </button>
        <button
          type="button"
          onClick={() => {
            clearMutationErrors();
            setCreateOpen((v) => !v);
          }}
          className="ez-btn ez-btn-md ez-btn-primary ml-auto"
        >
          <PlusIcon className="text-sm" />
          新建账号
        </button>
      </div>

      {/* ---------- 新建账号 ---------- */}
      {createOpen ? (
        <CreateUserForm
          pending={createMutation.isPending}
          onCancel={() => setCreateOpen(false)}
          onSubmit={(input) =>
            createMutation.mutate(input, {
              onSuccess: (created) => {
                setCreateOpen(false);
                setNotice({
                  message: `已创建账号「${created.username}」，该账号需自行登录`,
                  tone: 'success',
                });
              },
            })
          }
        />
      ) : null}

      {/* ---------- 错误气泡：409 的主保护出口 ---------- */}
      {mutationError ? (
        <InlineError error={mutationError} onRetry={clearMutationErrors} />
      ) : null}

      {/* ---------- 列表 ---------- */}
      {list.isLoading ? (
        <div className="space-y-2">
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-20 w-full rounded-xl" />
          ))}
          <span className="sr-only">正在加载用户…</span>
        </div>
      ) : list.isError ? (
        <ErrorState error={list.error} onRetry={() => void list.refetch()} title="用户列表加载失败" />
      ) : users.length === 0 ? (
        <EmptyState
          title={q || role ? '没有匹配的用户' : '还没有用户'}
          description={
            q || role
              ? '换个关键词或清空角色筛选试试。'
              : '可以用右上角的「新建账号」直接开一个账号 —— 即使注册功能已关闭。'
          }
        />
      ) : (
        <>
          <ul className="space-y-2">
            {users.map((u) => (
              <UserRow
                key={u.id}
                user={u}
                busy={busy}
                onToggleRole={() =>
                  roleMutation.mutate(
                    { id: u.id, role: u.role === 'admin' ? 'user' : 'admin' },
                    {
                      onSuccess: (updated) =>
                        setNotice({
                          message: `「${updated.username}」已改为${updated.role === 'admin' ? '管理员' : '普通用户'}`,
                          tone: 'success',
                        }),
                    },
                  )
                }
                onToggleDisabled={() => {
                  if (u.disabled) {
                    disabledMutation.mutate(
                      { id: u.id, disabled: false },
                      {
                        onSuccess: () => setNotice({ message: `已恢复「${u.username}」`, tone: 'success' }),
                      },
                    );
                    return;
                  }
                  clearMutationErrors();
                  setPending({ kind: 'disable', user: u });
                }}
                onRevoke={() => {
                  clearMutationErrors();
                  setPending({ kind: 'revoke', user: u });
                }}
                onDelete={() => {
                  clearMutationErrors();
                  setPending({ kind: 'delete', user: u });
                }}
              />
            ))}
          </ul>

          <div className="flex items-center justify-between gap-3">
            <p className="text-[12px] text-ink-faint">
              已加载 {users.length} / {lastPage?.total ?? users.length} 个
            </p>
            {list.hasNextPage ? (
              <button
                type="button"
                onClick={() => void list.fetchNextPage()}
                disabled={list.isFetchingNextPage}
                className="ez-btn ez-btn-md ez-btn-secondary"
              >
                {list.isFetchingNextPage ? <InlineSpinner label="加载中…" /> : '加载更多'}
              </button>
            ) : (
              <span className="text-[12px] text-ink-faint">已到最后一页</span>
            )}
          </div>
        </>
      )}

      {/* ---------- 二次确认 ---------- */}
      <ConfirmDialog
        open={pending !== null}
        title={
          pending?.kind === 'delete'
            ? `删除账号「${pending.user.username}」`
            : pending?.kind === 'revoke'
              ? `吊销「${pending.user.username}」的全部会话`
              : `停用账号「${pending?.user.username ?? ''}」`
        }
        description={
          pending?.kind === 'delete' ? (
            <>
              <p>
                将<strong className="font-semibold text-ink">永久删除</strong>该账号，并级联清除其
                <strong className="font-semibold text-ink">会话、收藏、已读与偏好设置</strong>。
              </p>
              <p className="mt-1.5">
                文章与新闻源是全局共享数据，<strong className="font-semibold text-ink">不会被删除</strong>。
              </p>
              <p className="mt-1.5">此操作不可撤销。</p>
            </>
          ) : pending?.kind === 'revoke' ? (
            <>
              <p>该用户在所有设备上的登录状态会立即失效，需要重新登录。</p>
              <p className="mt-1.5">其收藏、已读与偏好数据保存在服务端，不受影响。</p>
            </>
          ) : (
            <>
              <p>停用后该用户无法登录，已登录的会话也会被吊销。</p>
              <p className="mt-1.5">账号与其收藏/已读数据仍然保留，可随时恢复。</p>
            </>
          )
        }
        confirmLabel={
          pending?.kind === 'delete' ? '永久删除' : pending?.kind === 'revoke' ? '全部吊销' : '停用账号'
        }
        pending={busy}
        onConfirm={runAction}
        onCancel={() => setPending(null)}
      />

      {/* ---------- 轻提示 ---------- */}
      {notice ? (
        <div className="pointer-events-none fixed bottom-6 left-1/2 z-50 w-full max-w-sm -translate-x-1/2 px-4">
          <div className="pointer-events-auto">
            <Toast message={notice.message} tone={notice.tone} onClose={() => setNotice(null)} />
          </div>
        </div>
      ) : null}
    </div>
  );
}

/* ========================================================================== *
 * 新建账号表单
 * ========================================================================== */

function CreateUserForm({
  pending,
  onSubmit,
  onCancel,
}: {
  pending: boolean;
  onSubmit: (input: { username: string; password: string; email: string; role: UserRole }) => void;
  onCancel: () => void;
}): JSX.Element {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [email, setEmail] = useState('');
  const [role, setRole] = useState<UserRole>('user');
  const [error, setError] = useState<string | null>(null);

  const submit = (e: React.FormEvent): void => {
    e.preventDefault();
    if (pending) return;
    setError(null);

    const name = username.trim();
    if (!name) return setError('请填写用户名');
    // 服务端走的是与自助注册完全相同的 Argon2id 与复杂度校验，
    // 这里只做最表层的长度卡口 —— 复制一份密码规则到前端必然漂移。
    if (password.length < 8) return setError('密码至少 8 个字符');

    onSubmit({
      username: name,
      password,
      email: email.trim(),
      role,
    });
  };

  return (
    <form onSubmit={submit} className="ez-card p-4">
      <h2 className="text-[13px] font-semibold text-ink">新建账号</h2>
      <p className="mt-1 text-[11px] leading-relaxed text-ink-muted">
        这里开的账号<strong className="font-semibold text-ink">不会自动登录</strong>
        —— 服务端不返回 token，新账号需要自己登录一次。
        即使注册功能已关闭，管理员也可以从这里进人。
      </p>

      <div className="mt-3 grid gap-3 sm:grid-cols-2">
        <div>
          <label className="ez-label" htmlFor="new-username">
            用户名
          </label>
          <input
            id="new-username"
            className="ez-input"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            maxLength={64}
            required
          />
        </div>
        <div>
          <label className="ez-label" htmlFor="new-email">
            邮箱<span className="ml-1 font-normal text-ink-faint">（选填）</span>
          </label>
          <input
            id="new-email"
            type="email"
            className="ez-input"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            maxLength={128}
          />
        </div>
        <div>
          <label className="ez-label" htmlFor="new-password">
            初始密码
          </label>
          <input
            id="new-password"
            type="password"
            className="ez-input"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            minLength={8}
            maxLength={128}
            autoComplete="new-password"
            required
          />
          <p className="mt-1 text-[11px] text-ink-faint">至少 8 个字符；复杂度规则与自助注册一致。</p>
        </div>
        <div>
          <label className="ez-label" htmlFor="new-role">
            角色
          </label>
          <select
            id="new-role"
            className="ez-input"
            value={role}
            onChange={(e) => setRole(e.target.value as UserRole)}
          >
            <option value="user">普通用户</option>
            <option value="admin">管理员</option>
          </select>
        </div>
      </div>

      {error ? (
        <p className="mt-2.5 text-[12px] text-rose-600 dark:text-rose-400" role="alert">
          {error}
        </p>
      ) : null}

      <div className="mt-3.5 flex gap-2">
        <button type="submit" disabled={pending} className="ez-btn ez-btn-md ez-btn-primary">
          {pending ? <InlineSpinner label="创建中…" /> : '创建账号'}
        </button>
        <button type="button" onClick={onCancel} disabled={pending} className="ez-btn ez-btn-md ez-btn-secondary">
          取消
        </button>
      </div>
    </form>
  );
}

/* ========================================================================== *
 * 单个用户行
 * ========================================================================== */

interface UserRowProps {
  user: AdminUser;
  busy: boolean;
  onToggleRole: () => void;
  onToggleDisabled: () => void;
  onRevoke: () => void;
  onDelete: () => void;
}

function UserRow({ user, busy, onToggleRole, onToggleDisabled, onRevoke, onDelete }: UserRowProps): JSX.Element {
  /**
   * 三个"不能点"的理由统一取一段文案。
   *
   * ★ 理由只能来自服务端的能力位（它是"最后一个可用管理员"规则的唯一所有者）；
   *   这里只做**展示措辞**上的区分：本人 vs 最后管理员，
   *   让用户明白"为什么这个按钮灰了"。
   */
  const selfReason = '不能对自己做这个操作';
  const lastAdminReason = '这是最后一个可用管理员，不能被降级 / 停用 / 删除';
  const reasonFor = (allowed: boolean): string | null => {
    if (allowed) return null;
    return user.self ? selfReason : lastAdminReason;
  };

  const roleDisabledReason = reasonFor(user.canChangeRole);
  const disableDisabledReason = reasonFor(user.canDisable);
  const deleteDisabledReason = reasonFor(user.canDelete);
  const revokeDisabledReason = user.canRevokeAll ? null : '不能吊销自己的会话';

  return (
    <li className="ez-card p-3.5">
      <div className="flex flex-wrap items-start gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="truncate text-[14px] font-medium text-ink">{user.username}</span>
            <span
              className={`ez-badge ${
                user.role === 'admin'
                  ? 'bg-brand-soft text-brand dark:text-blue-200'
                  : 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300'
              }`}
            >
              {user.role === 'admin' ? '管理员' : '普通用户'}
            </span>
            {user.disabled ? (
              <span className="ez-badge bg-rose-50 text-rose-700 dark:bg-rose-950/60 dark:text-rose-300">
                已停用
              </span>
            ) : null}
            {user.self ? <span className="ez-badge bg-surface-sunken text-ink-muted">你自己</span> : null}
          </div>
          <p className="mt-1 truncate text-[12px] text-ink-muted">{user.email || '未填写邮箱'}</p>
          <p className="mt-1 text-[11px] text-ink-faint">
            注册于 {formatAbsoluteSec(user.createdAt)}
            {user.lastLoginAt > 0 ? ` · 最近登录 ${formatRelativeSec(user.lastLoginAt)}` : ' · 从未登录'}
          </p>
          {/* 计数由服务端在列表里一并算出，不需要再打详情接口 */}
          <p className="mt-0.5 text-[11px] text-ink-faint">
            会话 {user.sessionCount} · 收藏 {user.favoriteCount} · 已读 {user.readCount}
          </p>
        </div>

        {/* ---------- 操作区 ---------- */}
        <div className="flex flex-wrap items-center gap-1.5">
          <button
            type="button"
            onClick={onToggleRole}
            disabled={busy || roleDisabledReason !== null}
            title={roleDisabledReason ?? (user.role === 'admin' ? '降级为普通用户' : '设为管理员')}
            className="ez-btn ez-btn-sm ez-btn-secondary"
          >
            {user.role === 'admin' ? '取消管理员' : '设为管理员'}
          </button>

          <button
            type="button"
            onClick={onToggleDisabled}
            disabled={busy || disableDisabledReason !== null}
            title={disableDisabledReason ?? (user.disabled ? '恢复该账号' : '停用该账号')}
            className="ez-btn ez-btn-sm ez-btn-secondary"
          >
            {user.disabled ? '恢复' : '停用'}
          </button>

          <button
            type="button"
            onClick={onRevoke}
            disabled={busy || revokeDisabledReason !== null}
            title={revokeDisabledReason ?? '吊销其全部登录会话'}
            className="ez-btn ez-btn-sm ez-btn-secondary"
          >
            吊销全部会话
          </button>

          <button
            type="button"
            onClick={onDelete}
            disabled={busy || deleteDisabledReason !== null}
            title={deleteDisabledReason ?? '删除该账号'}
            className="ez-btn ez-btn-sm ez-btn-secondary text-rose-600 dark:text-rose-400"
          >
            <TrashIcon className="text-sm" />
            删除
          </button>
        </div>
      </div>
    </li>
  );
}
