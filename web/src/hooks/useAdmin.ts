/**
 * 管理后台的 React Query hooks（`/api/v1/admin/**`）。
 *
 * ============================ 为什么集中在一个文件 ============================
 * 后台的查询彼此高度相关：改一个用户要同时失效"用户列表 + 概览计数"，
 * 重试一个任务要同时失效"任务列表 + 概览里的失败数"。把这些失效关系写在
 * 同一处，才能保证"改了 A 却忘了刷 B"这类遗漏能被一眼看出来。
 *
 * ============================ 失效策略 ============================
 * 后台是低频运维界面，数据正确性优先于请求数：写操作后一律**失效重拉**，
 * 不做乐观更新。理由：服务端还要过"最后一个可用管理员""不许动自己"等守卫，
 * 乐观更新后被 409 打回会留下"界面显示成功、实际没生效"的坏状态。
 *
 * ============================ 关于服务端下发的能力位 ============================
 * `AdminUserDTO` 每行自带 `self / canChangeRole / canDisable / canDelete / canRevokeAll`，
 * 它们是**服务端算的**（判据：目标不是本人、且不是最后一个可用管理员）。
 * UI 只读这些位来置灰按钮，**不再**本地推导 —— 规则一旦两端各写一遍必然漂移，
 * 而这里的漂移方向最坏：把本可操作的按钮置灰还算小事，
 * 放行了服务端会拒绝的操作，用户就会看到一次莫名其妙的 409。
 */

import { useInfiniteQuery, useMutation, useQuery, useQueryClient, type InfiniteData } from '@tanstack/react-query';
import {
  createAdminUser,
  deleteAdminUser,
  getAdminContentStats,
  getAdminOverview,
  getAdminSystem,
  listAdminAudioTasks,
  listAdminUsers,
  retryAdminAudioTask,
  revokeAllAdminUserSessions,
  setAdminUserDisabled,
  setAdminUserRole,
} from '@/api/endpoints';
import type {
  AdminAudioTask,
  AdminAudioTaskPage,
  AdminContentStats,
  AdminOverview,
  AdminRevokeResult,
  AdminSystemInfo,
  AdminUser,
  AdminUserPage,
  AudioTaskStatus,
  CreateAdminUserInput,
  UserRole,
} from '@/api/types';
import { adminKeys } from '@/lib/queryKeys';
import { useAuthStore } from '@/stores/auth';

/** 用户列表页大小。服务端允许 1–100（默认 20），超限会被夹到 100。 */
export const ADMIN_USER_PAGE_SIZE = 50;

/** 语音任务列表页大小。服务端允许 1–200（默认 20）。 */
export const ADMIN_AUDIO_PAGE_SIZE = 20;

/**
 * 当前登录用户是否为管理员（用于"显不显示后台入口"）。
 *
 * ★ 这只是**前端预判**，绝不能当权限依据：
 *   JWT 里的 role 是登录那一刻的快照，被降级后仍然自称 admin；
 *   账号被停用也是同样的情形。真正的判定在服务端，
 *   后台布局以接口是否返回 403 为准（见 AdminLayout）。
 */
export function useIsAdmin(): boolean {
  return useAuthStore((s) => s.user?.role === 'admin');
}

/**
 * 后台概览（扁平结构）。
 *
 * @param enabled 未登录时传 false —— 后台布局把它当权限探测用，
 *   不给误入 /admin 的游客白打一次 401。
 */
export function useAdminOverview(enabled = true) {
  return useQuery<AdminOverview>({
    queryKey: adminKeys.overview,
    queryFn: getAdminOverview,
    staleTime: 30_000,
    enabled,
  });
}

/**
 * 用户列表（游标翻页 + 搜索 + 角色筛选）。
 *
 * 泛型第 5 个参数 `string | null` 必须显式写：让 TS 自己推会把 `pageParam`
 * 推成 `{}`，报一个看不懂根因的类型错误。
 */
export function useAdminUsers(params: { q?: string; role?: UserRole } = {}) {
  const q = params.q?.trim() || undefined;
  const role = params.role;

  return useInfiniteQuery<
    AdminUserPage,
    Error,
    InfiniteData<AdminUserPage, string | null>,
    ReturnType<typeof adminKeys.users>,
    string | null
  >({
    queryKey: adminKeys.users({ q, role }),
    queryFn: ({ pageParam }) =>
      listAdminUsers({
        limit: ADMIN_USER_PAGE_SIZE,
        cursor: pageParam ?? undefined,
        q,
        role,
      }),
    initialPageParam: null,
    // ★ 没有下一页必须返回 undefined；nextCursor 是空串也算到底了。
    getNextPageParam: (last) => (last.hasMore && last.nextCursor ? last.nextCursor : undefined),
    staleTime: 15_000,
  });
}

/** 内容统计（分类 + Top 源 + 孤儿文章）。 */
export function useAdminContentStats() {
  return useQuery<AdminContentStats>({
    queryKey: adminKeys.contentStats,
    queryFn: getAdminContentStats,
    staleTime: 60_000,
  });
}

/**
 * 语音合成任务（游标翻页）。
 *
 * ★ **默认 `status='failed'`**：服务端按 id DESC 排（最新的在前），
 *   不加过滤时最新任务未必是用户想看的；后端反过来建议"进入语音页先过滤失败"，
 *   配合 `statusCounts` 做筛选 chip，效果等同于"失败优先"且还能正常翻页。
 *   想看全部时传 `''`（空串 = 不加过滤）。
 */
export function useAdminAudioTasks(status: AudioTaskStatus | '' = 'failed') {
  return useInfiniteQuery<
    AdminAudioTaskPage,
    Error,
    InfiniteData<AdminAudioTaskPage, string | null>,
    ReturnType<typeof adminKeys.audioTasks>,
    string | null
  >({
    queryKey: adminKeys.audioTasks(status),
    queryFn: ({ pageParam }) =>
      listAdminAudioTasks({
        status: status === '' ? undefined : status,
        limit: ADMIN_AUDIO_PAGE_SIZE,
        cursor: pageParam ?? undefined,
      }),
    initialPageParam: null,
    getNextPageParam: (last) => (last.hasMore && last.nextCursor ? last.nextCursor : undefined),
    staleTime: 10_000,
  });
}

/** 系统信息与数据库状态。 */
export function useAdminSystem() {
  return useQuery<AdminSystemInfo>({
    queryKey: adminKeys.system,
    queryFn: getAdminSystem,
    staleTime: 60_000,
  });
}

/** 写操作后的统一失效：用户维度（列表 / 概览计数）一起刷。 */
function useInvalidateAdmin() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: adminKeys.all });
  };
}

/** 改角色（200 返回更新后的用户）。对自己操作 / 降级最后一个管理员 → 409。 */
export function useSetAdminUserRole() {
  const invalidate = useInvalidateAdmin();
  return useMutation<AdminUser, unknown, { id: number; role: UserRole }>({
    mutationFn: ({ id, role }) => setAdminUserRole(id, role),
    onSuccess: invalidate,
  });
}

/** 停用 / 启用账号（200 返回更新后的用户）。 */
export function useSetAdminUserDisabled() {
  const invalidate = useInvalidateAdmin();
  return useMutation<AdminUser, unknown, { id: number; disabled: boolean }>({
    mutationFn: ({ id, disabled }) => setAdminUserDisabled(id, disabled),
    onSuccess: invalidate,
  });
}

/**
 * 删除账号（204）。
 *
 * 级联清除该用户的会话 / 收藏 / 已读 / 偏好 / merge_log；
 * 文章与新闻源是全局共享数据，不受影响 —— 所以**不失效**它们。
 */
export function useDeleteAdminUser() {
  const invalidate = useInvalidateAdmin();
  return useMutation<void, unknown, number>({
    mutationFn: (id) => deleteAdminUser(id),
    onSuccess: invalidate,
  });
}

/**
 * 吊销该用户全部会话（200 + `{ revoked }`）。
 *
 * 返回被吊销的会话数，UI 拿它做提示比"操作成功"有用得多 ——
 * "踢下线 0 个会话"是该用户本来就没登录，值得让用户知道。
 */
export function useRevokeAllAdminUserSessions() {
  const invalidate = useInvalidateAdmin();
  return useMutation<AdminRevokeResult, unknown, number>({
    mutationFn: (id) => revokeAllAdminUserSessions(id),
    onSuccess: invalidate,
  });
}

/**
 * 管理员代建账号（201）。
 *
 * ★ 不返回 token：新账号要自己登录，这里不是"代登录"，
 *   所以也不写 auth store —— 当前登录者仍是操作者本人。
 */
export function useCreateAdminUser() {
  const invalidate = useInvalidateAdmin();
  return useMutation<AdminUser, unknown, CreateAdminUserInput>({
    mutationFn: (input) => createAdminUser(input),
    onSuccess: invalidate,
  });
}

/** 重试语音任务（200 返回更新后的任务对象）。 */
export function useRetryAdminAudioTask() {
  const invalidate = useInvalidateAdmin();
  return useMutation<AdminAudioTask, unknown, number>({
    mutationFn: (taskId) => retryAdminAudioTask(taskId),
    onSuccess: invalidate,
  });
}
