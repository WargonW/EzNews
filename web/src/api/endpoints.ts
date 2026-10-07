/**
 * API 端点封装 —— 一函数对应 openapi.yaml 的一个 operationId。
 *
 * 好处：契约变更时只需改这一处，且每个函数名都对应契约里的 operationId，
 * 便于与契约文档对照核查。
 */

import { API_BASE, del, get, patch, post, put, request } from './client';
import type {
  AdminAudioTask,
  AdminAudioTaskPage,
  AdminContentStats,
  AdminOverview,
  AdminRevokeResult,
  AdminSystemInfo,
  AdminUser,
  AdminUserPage,
  ArticleDTO,
  ArticlePage,
  AudioDTO,
  AudioTaskDTO,
  AudioTaskStatus,
  BatchStateInput,
  BatchStateResult,
  Category,
  CategoryList,
  ChangePasswordInput,
  CreateAudioTaskInput,
  CreateAdminUserInput,
  DeleteAccountInput,
  Healthz,
  LoginInput,
  LogoutInput,
  MergeRequest,
  MergeResult,
  PreferencesDTO,
  PutPreferencesInput,
  Readyz,
  RefreshInput,
  RegisterInput,
  SessionList,
  SetAdminUserDisabledInput,
  SetAdminUserRoleInput,
  SetSourceEnabledInput,
  SetStateInput,
  SourceDTO,
  SourceInput,
  SourceList,
  StatePage,
  TokenResponse,
  UserDTO,
  UserRole,
} from './types';

/* ========================================================================== *
 * Articles —— tags: [articles]
 * ========================================================================== */

/** openapi `GET /api/v1/articles` 的查询参数（operationId: listArticles）。 */
export interface ListArticlesParams {
  /** 增量同步水位。传入即切换为增量模式（与 pageCursor 互斥）。 */
  cursor?: string;
  /** 翻页游标（不传 cursor 时生效）。 */
  pageCursor?: string;
  limit?: number;
  sourceId?: number;
  sourceKey?: string;
  category?: Category;
  /** published_at 下界（ISO 8601 或 epoch 毫秒）。 */
  from?: string;
  /** published_at 上界。 */
  to?: string;
  /** 关键词搜索（title/summary），openapi 约束 maxLength=64。 */
  q?: string;
  includeDisabled?: boolean;
  withAudio?: boolean;
}

/** operationId: listArticles —— 文章列表（首屏/翻页 或 增量同步）。 */
export function listArticles(params: ListArticlesParams = {}): Promise<ArticlePage> {
  return get<ArticlePage>('/api/v1/articles', {
    cursor: params.cursor,
    pageCursor: params.pageCursor,
    limit: params.limit,
    sourceId: params.sourceId,
    sourceKey: params.sourceKey,
    category: params.category,
    from: params.from,
    to: params.to,
    q: params.q?.slice(0, 64),
    includeDisabled: params.includeDisabled,
    withAudio: params.withAudio,
  });
}

/** operationId: getArticle —— 文章详情。 */
export function getArticle(id: number): Promise<ArticleDTO> {
  return get<ArticleDTO>(`/api/v1/articles/${id}`);
}

/** operationId: listCategories —— 分类枚举及文章计数。 */
export function listCategories(): Promise<CategoryList> {
  return get<CategoryList>('/api/v1/categories');
}

/* ========================================================================== *
 * Sources —— tags: [sources]
 * ========================================================================== */

/** operationId: listSources —— 源列表。 */
export function listSources(includeDisabled = true): Promise<SourceList> {
  return get<SourceList>('/api/v1/sources', { includeDisabled });
}

/** operationId: createSource —— 新增自定义源。 */
export function createSource(input: SourceInput): Promise<SourceDTO> {
  return post<SourceDTO>('/api/v1/sources', input);
}

/** operationId: updateSource —— 编辑源（默认源仅允许改 enabled）。 */
export function updateSource(id: number, input: SourceInput): Promise<SourceDTO> {
  return put<SourceDTO>(`/api/v1/sources/${id}`, input);
}

/** operationId: setSourceEnabled —— 启用/停用源。 */
export function setSourceEnabled(id: number, enabled: boolean): Promise<SourceDTO> {
  return patch<SourceDTO>(`/api/v1/sources/${id}/enabled`, { enabled } satisfies SetSourceEnabledInput);
}

/** operationId: deleteSource —— 删除自定义源（默认源 → 409）。 */
export function deleteSource(id: number): Promise<void> {
  return del<void>(`/api/v1/sources/${id}`);
}

/* ========================================================================== *
 * Audio —— tags: [audio]
 * ========================================================================== */

/**
 * operationId: createAudioTask —— 提交合成任务。
 *
 * 响应语义（openapi 用两个 status 码表达同一语义）：
 * - 200 + status=ready → 缓存命中，可直接播放；
 * - 202 + pending/processing → 已入队，需轮询。
 * 两者 data 都是 AudioTaskDTO，这里不区分状态码，只看 data.status。
 */
export function createAudioTask(input: CreateAudioTaskInput): Promise<AudioTaskDTO> {
  return post<AudioTaskDTO>('/api/v1/audio/tasks', input);
}

/** operationId: getAudioTask —— 查询合成状态（客户端按 1s→2s→4s 退避轮询）。 */
export function getAudioTask(taskId: number): Promise<AudioTaskDTO> {
  return get<AudioTaskDTO>(`/api/v1/audio/tasks/${taskId}`);
}

/** operationId: retryAudioTask —— 重试失败任务。 */
export function retryAudioTask(taskId: number): Promise<AudioTaskDTO> {
  return post<AudioTaskDTO>(`/api/v1/audio/tasks/${taskId}/retry`);
}

/**
 * operationId: getArticleAudio —— 查询已合成音频（缓存直查，**不触发合成**）。
 * 未合成时服务端返回 404，调用方应捕获并改为 createAudioTask。
 */
export function getArticleAudio(articleId: number, voice?: string, speed?: number): Promise<AudioDTO> {
  return get<AudioDTO>(`/api/v1/articles/${articleId}/audio`, { voice, speed });
}

/**
 * operationId: getAudioFile —— 音频文件地址。
 *
 * 不走 JSON envelope（返回 audio/mpeg 二进制），因此不走 request()，
 * 而是拼出可直接喂给 `<audio src>` 的绝对（同源）URL。
 *
 * 注意：openapi 的 AudioDTO.url 已是 `/api/v1/audio/5127` 形式，
 * 这里做 base 拼接以兼容 dev proxy 与生产反代两种部署。
 */
export function audioFileUrl(audioId: number): string {
  return `${API_BASE}/api/v1/audio/${audioId}`;
}

/* ========================================================================== *
 * Auth —— tags: [auth]
 * ========================================================================== */

/** operationId: register —— 注册（游客可调用）。成功 201 + TokenResponse。 */
export function register(input: RegisterInput): Promise<TokenResponse> {
  return post<TokenResponse>('/api/v1/auth/register', {
    username: input.username,
    password: input.password,
    email: input.email ?? null,
    deviceName: input.deviceName ?? null,
    clientId: input.clientId ?? null,
  });
}

/** operationId: login —— 登录。 */
export function login(input: LoginInput): Promise<TokenResponse> {
  return post<TokenResponse>('/api/v1/auth/login', {
    username: input.username,
    password: input.password,
    deviceName: input.deviceName ?? null,
    clientId: input.clientId ?? null,
  });
}

/**
 * operationId: logout —— 登出，销毁当前 session。
 *
 * 服务端优先按 Access Token 的 `sid` 声明定位会话（DEC-14），
 * 不依赖 refreshToken 是否送达，因此我们**总是**带上当前 accessToken
 * （由 request() 自动加 Authorization 头）。
 * 返回 204，幂等。
 */
export function logout(body?: LogoutInput): Promise<void> {
  return request<void>({
    method: 'POST',
    path: '/api/v1/auth/logout',
    body: body ?? {},
    expectNoContent: true,
  });
}

/** RefreshInput 目前只在类型层保留（刷新由 client.ts 内部完成，不对外暴露）。 */
export type { RefreshInput };

/* ========================================================================== *
 * Me —— tags: [me]，全部需要 Bearer
 * ========================================================================== */

/** operationId: getMe —— 当前账号信息。 */
export function getMe(): Promise<UserDTO> {
  return get<UserDTO>('/api/v1/me');
}

/** operationId: deleteMe —— 注销账号（需密码二次确认，返回 204，不可逆）。 */
export function deleteAccount(input: DeleteAccountInput): Promise<void> {
  return del<void>('/api/v1/me', input);
}

/** operationId: changePassword —— 改密（token_version+1，响应下发新 token 对）。 */
export function changePassword(input: ChangePasswordInput): Promise<TokenResponse> {
  return patch<TokenResponse>('/api/v1/me/password', input);
}

/** operationId: mergeGuestData —— ★ 游客数据幂等合并（Merge Policy v1）。 */
export function mergeGuestData(body: MergeRequest): Promise<MergeResult> {
  return post<MergeResult>('/api/v1/me/merge', body);
}

/**
 * 收藏/已读列表的查询参数（`GET /me/favorites` 与 `/me/reads` 结构相同）。
 *
 * `since` 缺省即**全量**；传了就走增量（复合键 `(updated_at, id)` 排序的
 * opaque 游标，语义与文章列表 `cursor` 完全一致）。
 */
export interface ListStateParams {
  /** 增量游标。不传 = 全量（首页/收藏页用）。 */
  since?: string;
  /** 每页条数，默认 200，最大 1000。 */
  limit?: number;
  /**
   * 是否附带文章摘要（`title` / `summary` / `sourceName` / `publishedAt`）。
   *
   * ★ **默认不传**，走服务端默认 `true`。理由：服务端以一次批量
   *   `id IN (...)` 覆盖整页（非逐条查询），开启的代价与页大小无关，
   *   所以"要显示标题"不需要权衡任何东西。
   *   只有纯同步场景（`useUserStateSync` / 增量 LWW 合并）才显式传 `false`
   *   跳过文章表查询 —— 那些流程只读 `articleId/deleted/updatedAt`。
   *
   *   注意：服务端对非法取值（如 `maybe`）**静默按 `true` 处理**，不返回 400。
   */
  withArticles?: boolean;
}

/** operationId: listFavorites —— 收藏列表（since 增量游标 / withArticles 附摘要）。 */
export function listFavorites(params: ListStateParams = {}): Promise<StatePage> {
  return get<StatePage>('/api/v1/me/favorites', {
    since: params.since,
    limit: params.limit,
    withArticles: params.withArticles,
  });
}

/** operationId: setFavorite —— 收藏/取消收藏（取消写墓碑）。 */
export function setFavorite(input: SetStateInput): Promise<void> {
  // openapi 的成功响应只声明 Envelope（200），不保证有 data；这里只要 200 即成功。
  return post<void>('/api/v1/me/favorites', input);
}

/** operationId: listReads —— 已读列表（since 增量游标 / withArticles 附摘要）。 */
export function listReads(params: ListStateParams = {}): Promise<StatePage> {
  return get<StatePage>('/api/v1/me/reads', {
    since: params.since,
    limit: params.limit,
    withArticles: params.withArticles,
  });
}

/** operationId: setRead —— 标记已读/取消已读（取消写墓碑）。 */
export function setRead(input: SetStateInput): Promise<void> {
  return post<void>('/api/v1/me/reads', input);
}

/**
 * operationId: batchSetState —— 批量设置收藏/已读（如"全部标记已读"）。
 *
 * ★ 整批原子：整个批次在同一个写事务里提交，中途任一条失败则**整批回滚**。
 *   所以客户端可以安全地做乐观更新（成功全批生效 / 失败全批未生效），
 *   也**不应该**自己把大批切片分多次发 —— 那会破坏这条原子性保证。
 * ★ `read` 必填：省略会被服务端视为 `false`（=标记为未读，写墓碑），
 *   等于静默抹掉这批文章的已读记录。
 * ★ 单次上限 500 条，超限返回 413（整批未写入），调用方需自行裁剪。
 * ★ 任一 `articleId` 不存在 → 整批 404（整批未写入）。
 */
export function batchSetState(input: BatchStateInput): Promise<BatchStateResult> {
  return post<BatchStateResult>('/api/v1/me/state/batch', input);
}

/** operationId: getPreferences —— 拉取偏好 KV 全量。 */
export function getPreferences(): Promise<PreferencesDTO> {
  return get<PreferencesDTO>('/api/v1/me/preferences');
}

/** operationId: putPreferences —— 整包写入偏好 KV（整包覆盖，天然幂等）。 */
export function putPreferences(input: PutPreferencesInput): Promise<void> {
  return put<void>('/api/v1/me/preferences', input);
}

/** operationId: listSessions —— 活跃会话列表。 */
export function listSessions(): Promise<SessionList> {
  return get<SessionList>('/api/v1/me/sessions');
}

/** operationId: revokeSession —— 踢下线指定会话。 */
export function revokeSession(id: number): Promise<void> {
  return del<void>(`/api/v1/me/sessions/${id}`);
}

/* ========================================================================== *
 * Admin —— `/api/v1/admin/**`
 *
 * ⚠️ **契约外**：openapi.yaml 未收录这批端点；服务端 `internal/service/admin_service.go`
 *   的 `Admin*DTO` 才是唯一权威，本段是它的一对一转写。
 *
 * 鉴权（由服务端 `authAdmin` 中间件统一处理）：Bearer + 角色 admin + 账号未停用。
 * - 无 token       → 401 UNAUTHORIZED
 * - 非管理员       → 403 FORBIDDEN（"需要管理员权限"）
 * - 账号已被停用   → 403 FORBIDDEN（"账号已被停用"）
 * 前端仍要在 UI 层拦（见 hooks/useAdmin 的注释），但**真正的保护在服务端**。
 *
 * ============================ 翻页参数 ============================
 * 用户列表与语音任务列表都是"游标 + hasMore"式：
 * `cursor` 传上一页 `nextCursor`（= 最后一行主键的十进制字符串），
 * 非法值服务端回落到第一页（不报错）。**没有下一页时 `nextCursor` 是空串**，
 * 所以客户端必须按"空串 = 到底了"处理，不能只看 hasMore。
 * ========================================================================== */

/** `GET /api/v1/admin/overview` —— 后台概览（扁平结构，无 counts/storage 分组）。 */
export function getAdminOverview(): Promise<AdminOverview> {
  return get<AdminOverview>('/api/v1/admin/overview');
}

/** `GET /api/v1/admin/users` 的查询参数。 */
export interface ListAdminUsersParams {
  /** 1–100，服务端默认 20，超限会被服务端夹到上限。 */
  limit?: number;
  /** 上一页 `nextCursor`（user.id 的字符串形式）；不传 = 第一页。 */
  cursor?: string;
  /** 用户名 / 邮箱模糊匹配（服务端已转义 LIKE 通配符）。 */
  q?: string;
  /** 只能取 `user` / `admin`，其它值服务端返回 400。 */
  role?: UserRole;
}

/**
 * `GET /api/v1/admin/users` —— 用户列表。
 *
 * 每行都带 `sessionCount/favoriteCount/readCount` 与五个能力位，
 * **不需要**再为看计数去打详情接口。
 */
export function listAdminUsers(params: ListAdminUsersParams = {}): Promise<AdminUserPage> {
  return get<AdminUserPage>('/api/v1/admin/users', {
    limit: params.limit,
    cursor: params.cursor,
    q: params.q?.trim() || undefined,
    role: params.role,
  });
}

/** `GET /api/v1/admin/users/{id}` —— 单个用户详情（与列表同结构）。 */
export function getAdminUser(id: number): Promise<AdminUser> {
  return get<AdminUser>(`/api/v1/admin/users/${id}`);
}

/**
 * `POST /api/v1/admin/users` —— 管理员代建账号（201 + AdminUserDTO）。
 *
 * ★ **不下发 token**：新账号需要自己登录，这里不是"代登录"。
 * ★ 用户名重复 → 409 CONFLICT。
 *
 * 空串即"不填"：`email` / `role` 传空串时服务端按缺省处理（role → user）。
 */
export function createAdminUser(input: CreateAdminUserInput): Promise<AdminUser> {
  return post<AdminUser>('/api/v1/admin/users', {
    username: input.username,
    password: input.password,
    email: input.email ?? '',
    role: input.role ?? '',
  });
}

/**
 * `PATCH /api/v1/admin/users/{id}/role` —— 改角色（200 + 更新后的用户）。
 *
 * 对自己操作、或降级最后一个可用管理员 → 409 CONFLICT。
 */
export function setAdminUserRole(id: number, role: UserRole): Promise<AdminUser> {
  return patch<AdminUser>(`/api/v1/admin/users/${id}/role`, { role } satisfies SetAdminUserRoleInput);
}

/**
 * `PATCH /api/v1/admin/users/{id}/disabled` —— 停用 / 启用账号（200 + 更新后的用户）。
 *
 * ★ **`disabled` 必填且必须显式出现在请求体里**：服务端用 `*bool` 区分
 *   "显式 false（启用）"与"没传这个字段"，漏传会 400 VALIDATION_FAILED
 *   （details.field = "disabled"）。所以这里无条件构造 `{ disabled }`，
 *   **绝不**因为值是 false 就把字段删掉 —— 那会把"启用"变成"校验失败"。
 */
export function setAdminUserDisabled(id: number, disabled: boolean): Promise<AdminUser> {
  return patch<AdminUser>(`/api/v1/admin/users/${id}/disabled`, { disabled } satisfies SetAdminUserDisabledInput);
}

/**
 * `DELETE /api/v1/admin/users/{id}` —— 删除账号（204 空 body）。
 *
 * 级联清掉 session / user_favorite / user_read / user_preference / merge_log 与 user 本身；
 * **刻意不删** article / source —— 那是全局共享数据。
 * 对自己或最后一个可用管理员 → 409。
 */
export function deleteAdminUser(id: number): Promise<void> {
  return del<void>(`/api/v1/admin/users/${id}`);
}

/** `POST /api/v1/admin/users/{id}/sessions/revoke-all` —— 吊销全部会话（200 + `{revoked}`）。 */
export function revokeAllAdminUserSessions(id: number): Promise<AdminRevokeResult> {
  return post<AdminRevokeResult>(`/api/v1/admin/users/${id}/sessions/revoke-all`, {});
}

/** `GET /api/v1/admin/stats/content` —— 内容统计（分类 + Top 源 + 孤儿文章）。 */
export function getAdminContentStats(): Promise<AdminContentStats> {
  return get<AdminContentStats>('/api/v1/admin/stats/content');
}

/** `GET /api/v1/admin/audio/tasks` 的查询参数。 */
export interface ListAdminAudioTasksParams {
  /** 白名单 `pending|processing|ready|failed`；其它值服务端返回 400（不会静默空列表）。 */
  status?: AudioTaskStatus;
  /** 1–200，服务端默认 20。 */
  limit?: number;
  /** 上一页 `nextCursor`（task.id 的字符串形式）。 */
  cursor?: string;
}

/**
 * `GET /api/v1/admin/audio/tasks` —— 语音合成任务（后台视角，按 id DESC）。
 *
 * `statusCounts` 是**全量**分布，不受 status 过滤影响，适合直接渲染筛选 chip。
 */
export function listAdminAudioTasks(params: ListAdminAudioTasksParams = {}): Promise<AdminAudioTaskPage> {
  return get<AdminAudioTaskPage>('/api/v1/admin/audio/tasks', {
    status: params.status,
    limit: params.limit,
    cursor: params.cursor,
  });
}

/** `POST /api/v1/admin/audio/tasks/{taskId}/retry` —— 重试任务（200 + 更新后的任务对象）。 */
export function retryAdminAudioTask(taskId: number): Promise<AdminAudioTask> {
  return post<AdminAudioTask>(`/api/v1/admin/audio/tasks/${taskId}/retry`, {});
}

/** `GET /api/v1/admin/system` —— 运行环境与数据库状态。 */
export function getAdminSystem(): Promise<AdminSystemInfo> {
  return get<AdminSystemInfo>('/api/v1/admin/system');
}

/* ========================================================================== *
 * Ops —— tags: [ops]（非 Envelope 包裹，仅用于离线/健康探测）
 * ========================================================================== */

/** operationId: healthz —— 存活探针。 */
export async function healthz(): Promise<Healthz> {
  const res = await fetch(`${API_BASE}/healthz`, { credentials: 'include' });
  if (!res.ok) throw new Error(`healthz failed: ${res.status}`);
  return (await res.json()) as Healthz;
}

/** operationId: readyz —— 就绪探针。 */
export async function readyz(): Promise<Readyz> {
  const res = await fetch(`${API_BASE}/readyz`, { credentials: 'include' });
  return (await res.json()) as Readyz;
}