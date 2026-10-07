/**
 * ============================================================================
 * EZNews Web 端 API 类型定义
 * ============================================================================
 *
 * ⚠️ 本文件是 **`docs/api/openapi.yaml` 的手写转写**，是前端唯一的类型真相源。
 * 每一个类型上方都标注了对应的 openapi schema / path 名；未在 openapi 中出现的
 * 字段（如 `ArticleDTO.isFavorited`）会显式标注 [契约外] 并说明依据。
 *
 * 规则（硬性）：
 * 1. 禁止 `any`。确实需要宽松解析的地方用 `unknown` + 显式收窄。
 * 2. 字段名严格对齐 openapi 的 JSON 名称（camelCase）。
 * 3. 时间字段：openapi 声明为 `format: date-time` 的一律是 ISO8601 UTC 字符串；
 *    openapi 明确声明为 `integer` 的（MergeItem.updatedAt / StateItem.updatedAt /
 *    expiresIn / serverTimeMs 等）一律是 epoch 毫秒数字。**不要混淆。**
 */

/* ========================================================================== *
 * Envelope —— components.schemas.Envelope
 * ========================================================================== */

/**
 * openapi `Envelope.code` 的枚举（openapi.yaml Envelope.code 的 description 列举）。
 *
 * 注意 openapi 侧声明为 `type: string` + description 列举，**不是 `enum` 数组**，
 * 因此本联合类型即前端对该 description 的完整转写，顺序与之一致。
 * 未知码不要 `as EnvelopeCode` 强转，交由错误处理按 `code === 'TOKEN_EXPIRED'` 这类
 * 精确比较分支处理，未知值一律走通用错误提示。
 */
export type EnvelopeCode =
  | 'OK'
  | 'BAD_REQUEST'
  | 'VALIDATION_FAILED'
  | 'UNAUTHORIZED'
  | 'INVALID_API_KEY'
  | 'INVALID_CREDENTIALS'
  /** 401 的具体成因：access token 过期（区别于 INVALID_CREDENTIALS）。 */
  | 'TOKEN_EXPIRED'
  /** 403 的具体成因：注册关闭、越权等。 */
  | 'FORBIDDEN'
  | 'NOT_FOUND'
  | 'CONFLICT'
  | 'PAYLOAD_TOO_LARGE'
  | 'UNPROCESSABLE'
  | 'RATE_LIMITED'
  /** TTS 合成链路临时态（503 + Retry-After），客户端应退避重试。 */
  | 'TTS_UNAVAILABLE'
  /** TTS 队列已满（429 + Retry-After），客户端应退避重试。 */
  | 'TTS_QUEUE_FULL'
  | 'SERVICE_BUSY'
  | 'DB_BUSY'
  | 'INTERNAL_ERROR';

/** openapi `Envelope.data.details[]`（VALIDATION_FAILED 时携带）。 */
export interface EnvelopeDetail {
  field: string;
  message: string;
}

/** openapi `components.schemas.Envelope`。 */
export interface Envelope<T> {
  code: EnvelopeCode;
  message: string;
  requestId?: string;
  data?: T | EnvelopeDetail[] | null;
}

/* ========================================================================== *
 * 枚举
 * ========================================================================== */

/** openapi `components.schemas.Category`。 */
export type Category =
  | 'tech'
  | 'finance'
  | 'sports'
  | 'world'
  | 'china'
  | 'ent'
  | 'life'
  | 'auto'
  | 'military'
  | 'science'
  | 'health'
  | 'other';

/** openapi `SourceDTO.type` / `SourceInput.type`。 */
export type SourceType = 'rss' | 'atom' | 'api' | 'manual';

/** openapi `AudioTaskDTO.status`。 */
export type AudioTaskStatus = 'pending' | 'processing' | 'ready' | 'failed';

/** openapi `AudioBrief.status`（比 task 多一个 `none`）。 */
export type AudioBriefStatus = 'none' | AudioTaskStatus;

/* ========================================================================== *
 * 文章查询
 * ========================================================================== */

/**
 * openapi `components.schemas.AudioBrief`。
 * 仅在列表请求 `withAudio=true` 时挂在 ArticleDTO.audio 上。
 *
 * ★ `status: 'none'` 是**常态**，不是异常：服务端在 `withAudio=true` 时会给
 *   **每一篇**都带上 audio 字段，从未提交过合成任务的文章显式返回 `none`
 *   （而不是省略整个字段）。所以判断「有没有音频」要靠 `status === 'ready'`，
 *   不要依赖 `audio` 是否存在 —— 依赖字段存在性是这个接口最容易踩的坑。
 * ★ `voice` / `speed` 只在该文章**确有**合成任务时回填；`status: 'none'` 时为 null，
 *   即「还不知道会用哪个音色」，客户端要发起合成就自己带上偏好的音色与语速。
 */
export interface AudioBrief {
  status: AudioBriefStatus;
  audioId: number | null;
  voice: string | null;
  speed: number | null;
}

/**
 * openapi `components.schemas.ArticleDTO`。
 *
 * [契约外] 服务端 `internal/httpapi/article_handler.go` 的 ArticleDTO 额外输出
 * `isFavorited` / `isRead`（`omitempty`，仅带合法 JWT 时出现），openapi 的
 * ArticleDTO 未声明。客户端按"可选且可能缺失"处理。
 */
export interface ArticleDTO {
  id: number;
  sourceId: number;
  sourceKey: string;
  sourceName: string;
  category: Category;
  title: string;
  summary: string;
  /** 仅当该文章实际存了正文时出现（服务端默认不存正文）。 */
  content?: string | null;
  imageUrl: string | null;
  author: string | null;
  url: string;
  publishedAt: string;
  updatedAt: string;
  tags: string[];
  /** 仅 withAudio=true 时出现。 */
  audio?: AudioBrief | null;
  /** [契约外] 见上。undefined = 服务端未返回（游客态或无用户上下文）。 */
  isFavorited?: boolean;
  /** [契约外] 见上。 */
  isRead?: boolean;
}

/** openapi `components.schemas.ArticlePage`。 */
export interface ArticlePage {
  items: ArticleDTO[];
  /** 增量模式下一页水位；hasMore=false 时为 null。 */
  nextCursor: string | null;
  /** 翻页模式下一页游标；无更多时为 null。 */
  nextPageCursor: string | null;
  /** 服务端当前水位（now-2s 重叠窗口），客户端保存用于下次增量。 */
  syncCursor: string;
  hasMore: boolean;
  serverTime: string;
  serverTimeMs: number;
}

/** openapi `GET /api/v1/categories` 的 `data.items[]`。 */
export interface CategoryCount {
  key: Category;
  label: string;
  count: number;
}

/** openapi `GET /api/v1/categories` 的 `data`。 */
export interface CategoryList {
  items: CategoryCount[];
}

/* ========================================================================== *
 * 新闻源
 * ========================================================================== */

/** openapi `components.schemas.SourceDTO`。 */
export interface SourceDTO {
  id: number;
  key: string;
  name: string;
  url: string;
  type: SourceType;
  category: Category;
  /** 系统默认源：不可删除，只能停用。 */
  isDefault: boolean;
  enabled: boolean;
  /** 建议采集间隔（秒），仅供外部采集器参考。 */
  suggestInterval: number;
  iconUrl: string | null;
  language: string | null;
  remark: string | null;
  /** [契约外] openapi 标注为 optional，服务端实际总是返回数字（可能为 0）。 */
  articleCount?: number;
  createdAt: string;
  updatedAt: string;
}

/** openapi `GET /api/v1/sources` 的 `data`。 */
export interface SourceList {
  items: SourceDTO[];
}

/**
 * openapi `components.schemas.SourceInput`。
 * `key` 不在输入里——服务端按 url 自行派生稳定业务键。
 */
export interface SourceInput {
  name: string;
  url: string;
  type?: SourceType;
  category?: Category;
  suggestInterval?: number;
  iconUrl?: string | null;
  language?: string | null;
  remark?: string | null;
  enabled?: boolean;
}

/** openapi `PATCH /api/v1/sources/{id}/enabled` 的请求体。 */
export interface SetSourceEnabledInput {
  enabled: boolean;
}

/* ========================================================================== *
 * 语音（TTS）
 * ========================================================================== */

/** openapi `components.schemas.AudioTaskDTO`。 */
export interface AudioTaskDTO {
  id: number;
  articleId: number;
  voice: string;
  speed: number;
  status: AudioTaskStatus;
  audioId: number | null;
  provider: 'tencent' | 'ali' | 'xunfei' | null;
  retryCount: number;
  errorCode: string | null;
  errorMsg: string | null;
  textChars: number;
  createdAt: string;
  updatedAt: string;
}

/** openapi `components.schemas.AudioDTO`。 */
export interface AudioDTO {
  id: number;
  articleId: number;
  voice: string;
  speed: number;
  format: string;
  durationMs: number;
  sizeBytes: number;
  sampleRate: number;
  provider: string | null;
  /** 音频下载地址，形如 `/api/v1/audio/5127`（相对路径，勿拼绝对域名）。 */
  url: string;
  createdAt: string;
}

/** openapi `POST /api/v1/audio/tasks` 的请求体。 */
export interface CreateAudioTaskInput {
  articleId: number;
  voice?: string;
  speed?: number;
}

/* ========================================================================== *
 * 账号（v1.1）
 * ========================================================================== */

/** openapi `components.schemas.UserDTO`。 */
export interface UserDTO {
  id: number;
  username: string;
  email: string | null;
  role: 'user' | 'admin';
  createdAt: string;
  lastLoginAt: string | null;
}

/** openapi `components.schemas.TokenResponse`。 */
export interface TokenResponse {
  /** JWT(HS256)，2 小时；客户端仅存内存，绝不落 localStorage。 */
  accessToken: string;
  /** 同源 Cookie 部署下为 null；跨域部署降级为响应体返回。 */
  refreshToken: string | null;
  /** accessToken 剩余秒数。 */
  expiresIn: number;
  user: UserDTO;
}

/** openapi `POST /api/v1/auth/register` 的请求体。 */
export interface RegisterInput {
  username: string;
  password: string;
  email?: string | null;
  deviceName?: string | null;
  clientId?: string | null;
}

/** openapi `POST /api/v1/auth/login` 的请求体。 */
export interface LoginInput {
  username: string;
  password: string;
  deviceName?: string | null;
  clientId?: string | null;
}

/**
 * openapi `POST /api/v1/auth/refresh` 的请求体。
 *
 * ★ `clientId` 是 **DEC-7 宽限重放判定的必需字段**：服务端在同一 refresh token
 *   再次出现时会比对 `client_id` ——相同则判为弱网重试（再轮换一次，不惩罚），
 *   不同则判为疑似泄露（撤销该用户**全部** session + `token_version+1`）。
 *   缺省（null）按"无法确认同源"处理，**不享受宽限豁免**。
 *   刷新由 client.ts 内部完成（buildRefreshBody 无条件带上），故本类型仅供外部引用。
 */
export interface RefreshInput {
  refreshToken?: string | null;
  clientId?: string | null;
  deviceName?: string | null;
}

/**
 * openapi `POST /api/v1/auth/logout` 的请求体。
 * 服务端优先按 Access Token 的 `sid` 声明定位会话（DEC-14），body 仅作回退。
 */
export interface LogoutInput {
  refreshToken?: string | null;
}

/** openapi `PATCH /api/v1/me/password` 的请求体。 */
export interface ChangePasswordInput {
  oldPassword: string;
  newPassword: string;
}

/** openapi `DELETE /api/v1/me` 的请求体（密码二次确认，DEC-8）。 */
export interface DeleteAccountInput {
  password: string;
}

/* ========================================================================== *
 * Merge Policy v1
 * ========================================================================== */

/**
 * openapi `components.schemas.MergeItem`。
 *
 * ★ `updatedAt` 在 openapi 中明确为 **integer（epoch 毫秒）**，不是 ISO 串。
 *   MEMORY.md 陷阱：合并时必须钳制客户端时间戳（<=0 或超前 >7 天压到"此刻"），
 *   否则客户端时钟不准会让本地数据永久赢过服务端。
 */
export interface MergeItem {
  articleId: number;
  /** true = 墓碑（本地已取消收藏/已读）。 */
  deleted: boolean;
  updatedAt: number;
}

/**
 * openapi `components.schemas.PreferenceKV`。
 *
 * ⚠️ 契约漂移：openapi 声明 value 为 `string | number | boolean | null`，
 *   但服务端 `dto_me.go` 的 `MergeRequest.Preferences` / `PutPreferencesRequest.Preferences`
 *   都是 `map[string]string`。因此**客户端一律以字符串下发**，number/boolean
 *   在本地编码为字符串（如 speed → "1.25"），由 UI 层负责反解。
 */
export type PreferenceKV = Record<string, string>;

/** openapi `components.schemas.MergeRequest`。 */
export interface MergeRequest {
  /** 客户端实例标识，首次启动生成并持久化（reinstall 重置）。 */
  clientId: string;
  /** 幂等键。每批次生成一次并**持久化至成功**，重试必须沿用同一 nonce。 */
  nonce: string;
  favorites?: MergeItem[];
  reads?: MergeItem[];
  preferences?: PreferenceKV;
}

/** openapi `components.schemas.MergeCounts`。 */
export interface MergeCounts {
  received: number;
  /** 新增行。 */
  added: number;
  /** 命中且本地更新被采纳（LWW 胜出）。 */
  merged: number;
  /** 命中但云端更新（LWW 判负）。 */
  skipped: number;
}

/** openapi `components.schemas.MergeResult`。 */
export interface MergeResult {
  applied: boolean;
  /** true = 命中 merge_log 幂等键，直接回放历史结果（零写入）。 */
  replayed: boolean;
  favorites: MergeCounts;
  reads: MergeCounts;
  preferenceApplied: boolean;
  /** 合并后服务端水位，可作后续增量同步起点。 */
  snapshotCursor: string;
}

/* ========================================================================== *
 * 收藏 / 已读 增量流
 * ========================================================================== */

/**
 * openapi `components.schemas.StateItem`。
 *
 * ★ DEC-11 实现强制项：`deleted=true` 的墓碑项**必须随增量流返回**，
 *   客户端收到后应**本地移除**该条，而不是忽略它。
 * ★ `updatedAt` 是 **epoch 毫秒整数**（不是 ISO 串）。
 *
 * ==================== 摘要字段（`withArticles`） ====================
 * `GET /me/favorites` 与 `GET /me/reads` 支持查询参数 `withArticles`
 * （boolean，默认 `true`）：
 * - `true`（默认）→ 服务端以**一次批量 `id IN (...)`** 查询为每条附加文章摘要，
 *   代价与页大小无关，因此下面 4 个字段通常都在；
 * - `false` → 完全跳过文章表查询，响应退化为只有前 3 个基础字段。
 *
 * ★ 下面 4 个字段**一律 optional**，两种原因都会让它们缺席：
 *   1. 服务端 `omitempty`（`withArticles=false`，或摘要本身为空串）；
 *   2. ★ **文章行已被物理删除** —— retention 清理（`retention.days`，默认 90 天）
 *      或源被删除都会级联删掉 article 行。收藏/已读行本身还在（墓碑语义要求保留），
 *      但已无从取回标题。
 * 因此客户端必须按"缺字段 = 该文章可能已不可用"降级渲染，
 * **不得视为协议错误**，更不得因为一条缺失就中断整页解析。
 */
export interface StateItem {
  articleId: number;
  deleted: boolean;
  updatedAt: number;
  /** 文章标题。缺失 → 文章可能已被 retention 清理或源被删除。 */
  title?: string;
  /** 文章摘要。缺失原因同 `title`。 */
  summary?: string;
  /** 来源名（如"新华社"）。缺失原因同 `title`。 */
  sourceName?: string;
  /** 发布时间，ISO 8601 UTC（与 `ArticleDTO.publishedAt` 同型，**不是** epoch 毫秒）。 */
  publishedAt?: string;
}

/** openapi `components.schemas.StatePage`。 */
export interface StatePage {
  /** 同时包含新增/更新项与墓碑项，两类都要处理。 */
  items: StateItem[];
  nextCursor: string | null;
  hasMore: boolean;
  serverTimeMs: number;
}

/** openapi `POST /api/v1/me/favorites` 与 `/me/reads` 的请求体。 */
export interface SetStateInput {
  articleId: number;
  /** true = 取消（写墓碑，非物理删除）。 */
  deleted: boolean;
}

/**
 * openapi `components.schemas.BatchStateRequest`
 * —— `POST /api/v1/me/state/batch` 的请求体（operationId: batchSetState）。
 *
 * ==================== 为什么是"整批原子"而不是逐条 ====================
 * 服务端把整个批次放在**同一个数据库写事务**里执行：中途任一条失败则**整批回滚**，
 * 库里不留任何半成品。因此本端点**只有两种结果**，不存在"部分成功"：
 * - `200` → `readsChanged` / `favoritesChanged` 对应的行已全部落库；
 * - 4xx / 5xx → **整批一条都没生效**，客户端可原样重试。
 *
 * 这也是前端可以安全做乐观更新的唯一依据，也是客户端**不做分片**的理由：
 * 自己切片会撕碎这条保证，用户会拿到"前 500 条生效、后 500 条没生效"的中间态。
 *
 * ★ **单次上限 500 条**（openapi `maxItems`），超限 `413` 且整批未写入。
 *   客户端必须自己裁剪，不能指望服务端兜底。
 */
export interface BatchStateInput {
  /**
   * 文章 id 列表，1-500 条。
   *
   * ★ **`articleId <= 0` 会让整批 400**（不是被静默丢弃），所以客户端
   *   必须先自行过滤非法 id —— 列表数据来自服务端，理论上不会出现非正数，
   *   但一旦出现就是"整批失败"而不是"这一条被忽略"，代价不对称。
   * ★ 重复 id 由服务端去重，不会报错。
   */
  articleIds: number[];
  /**
   * true = 标记为已读；false = 标记为未读（写墓碑）。
   *
   * ★ **必填，不接受省略** —— 声明成 `boolean` 而不是 `boolean | undefined`
   *   就是为了让"忘了传"在编译期就报错。服务端把缺省视为 `false`
   *   （即"标记为未读"），漏传等于静默抹掉这批文章的已读记录。
   */
  read: boolean;
  /**
   * 可选。true = 同时收藏；false = 同时取消收藏（写墓碑）。
   *
   * ★ **省略 = 本批次完全不改动收藏**（只改已读）。
   *   用 `undefined` 而非 `null` 表达"不改"：`JSON.stringify` 会直接丢掉
   *   `undefined` 的键，正好落到服务端的"不改动"分支。
   */
  favorite?: boolean;
}

/**
 * openapi `components.schemas.BatchStateResult`
 * —— `POST /api/v1/me/state/batch` 的回执。
 *
 * ★ 两个计数即**全量结果**，没有逐条成败清单（批次是原子的）。
 * ★ 计数语义是"本批次**提交**了多少条"，**不是**"有多少条的状态真的变了"。
 *   实测（对同一批已读文章重复提交）仍返回 `readsChanged = 2`：
 *   服务端对每个去重后的 id 无条件 `ReadsChanged++`，并不比较库内现状。
 *   所以它只可靠地反映"提交了多少"，**不能用来告诉用户"几篇变已读了"**——
 *   拿它当成功提示的计数会让用户困惑（尤其在反复点同一个按钮时，每次都是同一个数）。
 *   本仓库的「全部已读」因此改用**提交前自己算出的未读条数**做提示。
 */
export interface BatchStateResult {
  /** 本批次提交的已读条数（已按 articleId 去重）。 */
  readsChanged: number;
  /** 本批次提交的收藏条数；`favorite` 省略时恒为 0。 */
  favoritesChanged: number;
}

/** openapi `GET /api/v1/me/preferences` 的 `data`。 */
export interface PreferencesDTO {
  preferences: PreferenceKV;
  updatedAt: string;
}

/** openapi `PUT /api/v1/me/preferences` 的请求体。 */
export interface PutPreferencesInput {
  preferences: PreferenceKV;
}

/** openapi `components.schemas.SessionDTO`。 */
export interface SessionDTO {
  id: number;
  deviceName: string | null;
  clientId: string;
  lastSeenAt: string;
  expiresAt: string;
  createdAt: string;
  /** 是否为当前请求所使用的会话。 */
  current: boolean;
}

/** openapi `GET /api/v1/me/sessions` 的 `data`。 */
export interface SessionList {
  items: SessionDTO[];
}

/* ========================================================================== *
 * 管理后台（v1.2）—— `/api/v1/admin/**`
 *
 * ⚠️ **契约外**：`docs/api/openapi.yaml` 尚未收录 admin 端点，服务端实现在
 *   `server/internal/service/admin_service.go`（`Admin*DTO`），那里才是唯一权威。
 *   本段是该 Go 结构体的一对一手写转写，字段名严格照抄，**不得自行推断**。
 *
 * ★ 时间字段口径（最容易踩的一处）：后台 DTO 的时间一律是 **epoch 秒整数**，
 *   不是阅读端那种 ISO 8601 字符串，也不是 MergeItem/StateItem 的 epoch **毫秒**。
 *   用 `lib/datetime` 的 `formatRelativeSec` / `formatAbsoluteSec` 渲染，不要混用。
 *   `lastLoginAt` 为 0 表示"从未登录"（服务端用 0 而非 null 表达缺省）。
 *
 * 鉴权：全部端点要求 Bearer + 角色 admin + 账号未停用，否则
 * - 401 UNAUTHORIZED（无 token）
 * - 403 FORBIDDEN（message="需要管理员权限" / "账号已被停用"）
 * ========================================================================== */

/** openapi `UserDTO.role`（后台复用的同一枚举）。 */
export type UserRole = 'user' | 'admin';

/**
 * `GET /api/v1/admin/overview` 的 `data`（`AdminOverviewDTO`，**扁平**，无嵌套分组）。
 *
 * ★ `admins` 是 `COUNT(*) WHERE role='admin'`，**包含已停用的管理员**。
 *   想知道"还有几个可用管理员"要用 `GET /admin/users` 的 `admins`（那个只数未停用的）。
 *   两个数字不一样不是 bug，概览页因此把"已停用用户"与它并排展示。
 */
export interface AdminOverview {
  users: number;
  admins: number;
  disabledUsers: number;
  activeSessions: number;
  articles: number;
  sources: number;
  audios: number;
  failedAudioTasks: number;
  pendingAudioTasks: number;
  articlesLast24h: number;
  /** 各状态的任务数，形如 `{ pending: 2, failed: 3 }`；键来自状态枚举。 */
  audioTaskStatus: Record<string, number>;
  schemaVersion: number;
  uptimeSeconds: number;
  serverVersion: string;
  goVersion: string;
  /** 服务端生成这份数据的时刻（epoch 秒）。 */
  generatedAt: number;
}

/**
 * `GET /admin/users` 的 `items[]` / `GET /admin/users/{id}` /
 * `PATCH .../role` / `PATCH .../disabled` / `POST /admin/users` 的响应，
 * 五个-descriptor 全部复用同一个 DTO（`AdminUserDTO`）。
 *
 * ★ 三个计数在**列表**里就有（服务端对每行都算），不要再为了看计数去打详情接口。
 *
 * ★ 末尾五个是**服务端算好的能力位**。前端据此置灰按钮，
 *   **不要**自己用 `admins` 计数去推导"是不是最后一个管理员"：
 *   那条规则的判据是"目标本人是管理员且可用管理员只剩他一个"
 *   （见服务端 `computeCaps`），前端复算必然漂移，而且服务端还会返回 409 ——
 *   这些位只服务于 UX，真正的保护是服务端。
 */
export interface AdminUser {
  id: number;
  username: string;
  /** 未填写时为空串（不是 null）。 */
  email: string;
  role: UserRole;
  disabled: boolean;
  /** epoch 秒。 */
  createdAt: number;
  /** epoch 秒；0 = 从未登录。 */
  lastLoginAt: number;
  sessionCount: number;
  favoriteCount: number;
  readCount: number;
  /** 这一行是不是登录者本人。 */
  self: boolean;
  canChangeRole: boolean;
  canDisable: boolean;
  canDelete: boolean;
  canRevokeAll: boolean;
}

/** `GET /api/v1/admin/users` 的 `data`（`AdminUserListDTO`，游标翻页）。 */
export interface AdminUserPage {
  items: AdminUser[];
  /** 下一页游标（最后一行 user.id 的字符串形式）；无下一页时为**空串**。 */
  nextCursor: string;
  hasMore: boolean;
  /** 符合当前过滤条件的总数，**不受翻页影响**，可直接用于算页数。 */
  total: number;
  /** 未停用的管理员总数（区别于 overview.admins 那个含停用的计数）。 */
  admins: number;
}

/** `POST /api/v1/admin/users` 的请求体（管理员代建账号）。 */
export interface CreateAdminUserInput {
  username: string;
  password: string;
  /** 选填，不填传空串即可。 */
  email?: string;
  /** 省略（空串）时服务端按 `user` 处理。 */
  role?: UserRole;
}

/** `POST /api/v1/admin/users/{id}/sessions/revoke-all` 的 `data`。 */
export interface AdminRevokeResult {
  /** ★ key 是 `revoked`（不是 `revokedSessions`）。 */
  revoked: number;
}

/** `GET /api/v1/admin/stats/content` 的 `data.byCategory[]`。 */
export interface AdminCategoryStat {
  category: string;
  count: number;
}

/** `GET /api/v1/admin/stats/content` 的 `data.topSources[]`。 */
export interface AdminSourceStat {
  sourceId: number;
  sourceName: string;
  enabled: boolean;
  articleCount: number;
  /** 该源最新文章发布时间（epoch 秒）；源下无文章时为 0。 */
  lastSeenAt: number;
}

/**
 * `GET /api/v1/admin/stats/content` 的 `data`（`AdminContentDTO`）。
 *
 * ★ 三个口径要分清，否则用户会以为接口算错了：
 * - `total` = **本页 topSources 各源文章数之和**（服务端按 Top N 累加），
 *   不是全库总数；全库总数看 overview 的 `articles`；
 * - `orphanArticles` = 不属于任何现存源的文章数（源被删后的残留），
 *   它**不计入** `total` 与任何一行 —— 必须在 UI 上显式显示，
 *   否则表格合计对不上总数；
 * - `last24h` / `last7d` 按文章的 **published_at** 算，不是写入时间。
 */
export interface AdminContentStats {
  total: number;
  last24h: number;
  last7d: number;
  disabledSources: number;
  orphanArticles: number;
  byCategory: AdminCategoryStat[];
  topSources: AdminSourceStat[];
}

/**
 * `GET /admin/audio/tasks` 的 `items[]`（`AdminAudioTaskDTO`）。
 *
 * ★ 与 `/api/v1/audio/tasks` 的 `AudioTaskDTO` 是**两个不同的 schema**，不要混用：
 *   这里主键叫 `id`（那边也是 id 但字段集不同）、文章标题是 `articleTitle`、
 *   错误拆成 `errorCode` + `errorMsg`（早期稿里的 `error` 字段已删除）。
 */
export interface AdminAudioTask {
  id: number;
  /** 合成成功后回填的音频 id；尚未产出为 0。 */
  audioId: number;
  articleId: number;
  articleTitle: string;
  voice: string;
  speed: number;
  status: AudioTaskStatus;
  provider: string;
  errorCode: string;
  errorMsg: string;
  retryCount: number;
  textChars: number;
  /** epoch 秒。 */
  createdAt: number;
  /** epoch 秒。 */
  updatedAt: number;
}

/** `GET /api/v1/admin/audio/tasks` 的 `data`（游标翻页，按 id DESC）。 */
export interface AdminAudioTaskPage {
  items: AdminAudioTask[];
  /** 各状态的任务数；**不受当前 status 过滤影响**，始终是全量分布。 */
  statusCounts: Record<string, number>;
  audioCount: number;
  audioBytes: number;
  /** 符合当前 status 过滤的总数。 */
  total: number;
  hasMore: boolean;
  /** 下一页游标（最后一行 task.id 的字符串形式）；无下一页时为空串。 */
  nextCursor: string;
}

/**
 * `GET /api/v1/admin/system` 的 `data`（`AdminSystemDTO`）。
 *
 * ★ `dbBytes` 是**逻辑占用**（`page_size × page_count`），删数据后会立刻下降；
 *   SQLite 并不把空闲页归还文件系统，磁盘上的文件大小几乎不降 —— 这是有意为之，
 *   不要改成去 stat 磁盘文件。`freeRatio` 就是为此准备的：空闲页占比，
 *   > 0.3 基本说明该跑一次 `VACUUM` 了。
 */
export interface AdminSystemInfo {
  serverVersion: string;
  goVersion: string;
  goos: string;
  goarch: string;
  numGoroutine: number;
  uptimeSeconds: number;
  /** epoch 秒。 */
  startedAt: number;
  schemaVersion: number;
  journalMode: string;
  dbBytes: number;
  pageSize: number;
  pageCount: number;
  freePages: number;
  /** 0–1。 */
  freeRatio: number;
  heapAllocMb: number;
  /** 各表行数，键形如 `article` / `user` / `session` / `user_favorite`。 */
  tableCounts: Record<string, number>;
}

/** `PATCH /api/v1/admin/users/{id}/role` 的请求体。 */
export interface SetAdminUserRoleInput {
  role: UserRole;
}

/** `PATCH /api/v1/admin/users/{id}/disabled` 的请求体（★ `disabled` 必填）。 */
export interface SetAdminUserDisabledInput {
  disabled: boolean;
}

/* ========================================================================== *
 * Ops（用于离线态探测）
 * ========================================================================== */

/** openapi `GET /healthz` 的响应（注意：**不是** Envelope 包裹）。 */
export interface Healthz {
  status: string;
  uptimeSec: number;
  version: string;
}

/** openapi `GET /readyz` 的响应（非 Envelope）。 */
export interface Readyz {
  status: string;
  db: string;
}